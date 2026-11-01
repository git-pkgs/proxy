package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/go-chi/chi/v5"
)

const testProjectsToken = "sbom-token"

// syftSBOM is a CycloneDX document syft produced for a directory with an
// npm, Composer and Go lock file and an Alpine package database. It lists
// ten packages; five entries (the OS and the lock files) have no PURL.
func syftSBOM(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("../inventory/testdata/syft-cyclonedx.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

type projectsTestServer struct {
	server *Server
	router http.Handler
	db     *database.DB
}

// newProjectsTestServer routes the project API and the UI pages that show
// the inventory, with projects.enabled set.
func newProjectsTestServer(t *testing.T) projectsTestServer {
	t.Helper()
	db, err := database.Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("database.Create: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{Projects: config.ProjectsConfig{Enabled: true, Token: testProjectsToken}}
	s := &Server{cfg: cfg, db: db, logger: logger, templates: &Templates{}}

	r := chi.NewRouter()
	registerProjectRoutes(r, NewProjectsAPIHandler(db, logger), testProjectsToken)
	r.Route("/ui", func(ui chi.Router) {
		ui.Get("/", s.handleRoot)
		ui.Get("/package/{ecosystem}/*", s.handlePackagePath)
		s.mountProjectUI(ui)
	})
	return projectsTestServer{server: s, router: r, db: db}
}

func (ts projectsTestServer) do(t *testing.T, method, target, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	ts.router.ServeHTTP(rec, req)
	return rec
}

func (ts projectsTestServer) upload(t *testing.T, target string, body []byte) ProjectUploadResponse {
	t.Helper()
	rec := ts.do(t, http.MethodPut, target, testProjectsToken, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT %s = %d: %s", target, rec.Code, rec.Body)
	}
	var resp ProjectUploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding upload response: %v", err)
	}
	return resp
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
}

// componentCount returns a project's component_count, or -1 if not set.
func componentCount(p ProjectSummary) int64 {
	if p.ComponentCount == nil {
		return -1
	}
	return *p.ComponentCount
}

func TestProjectUploadAndGet(t *testing.T) {
	ts := newProjectsTestServer(t)

	resp := ts.upload(t, "/api/projects/acme/shop/web/sbom?ref=0123abcd&branch=main", syftSBOM(t))
	if resp != (ProjectUploadResponse{Project: "acme/shop/web", Components: 9, Skipped: 6}) {
		t.Errorf("upload response = %+v", resp)
	}

	// The same project, URL-encoded as one segment.
	var project ProjectResponse
	decodeJSON(t, ts.do(t, http.MethodGet, "/api/projects/acme%2Fshop%2Fweb", "", nil), &project)
	if project.Name != "acme/shop/web" || project.Ref != "0123abcd" || project.Branch != "main" || componentCount(project.ProjectSummary) != 9 {
		t.Errorf("project = %+v", project.ProjectSummary)
	}
	var purls []string
	for _, c := range project.Components {
		purls = append(purls, c.PURL)
	}
	for _, want := range []string{"pkg:npm/%40babel/core@7.23.0", "pkg:composer/symfony/console@v6.4.1", "pkg:golang/github.com/spf13/cobra@v1.8.0", "pkg:apk/alpine/busybox@1.36.1-r15"} {
		if !strings.Contains(strings.Join(purls, " "), want) {
			t.Errorf("components %v do not include %s", purls, want)
		}
	}

	var list ProjectListResponse
	decodeJSON(t, ts.do(t, http.MethodGet, "/api/projects", "", nil), &list)
	if len(list.Projects) != 1 || list.Projects[0].Name != "acme/shop/web" || componentCount(list.Projects[0]) != 9 {
		t.Errorf("list = %+v", list)
	}
}

func TestProjectUploadReplacesComponents(t *testing.T) {
	ts := newProjectsTestServer(t)
	ts.upload(t, "/api/projects/acme%2Fweb/sbom?ref=one", syftSBOM(t))
	resp := ts.upload(t, "/api/projects/acme%2Fweb/sbom", []byte(`{"bomFormat":"CycloneDX","specVersion":"1.5","components":[
		{"type":"library","name":"lodash","version":"4.17.21","purl":"pkg:npm/lodash@4.17.21"}]}`))
	if resp.Components != 1 || resp.Skipped != 0 {
		t.Errorf("upload response = %+v", resp)
	}
	var project ProjectResponse
	decodeJSON(t, ts.do(t, http.MethodGet, "/api/projects/acme/web", "", nil), &project)
	if len(project.Components) != 1 || project.Components[0].PURL != "pkg:npm/lodash@4.17.21" || project.Ref != "" {
		t.Errorf("project after second upload = %+v", project)
	}
}

func TestProjectLookupByPURL(t *testing.T) {
	ts := newProjectsTestServer(t)
	ts.upload(t, "/api/projects/acme/web/sbom", syftSBOM(t))
	ts.upload(t, "/api/projects/acme/api/sbom", []byte(`{"bomFormat":"CycloneDX","specVersion":"1.5","components":[
		{"type":"library","name":"lodash","version":"4.17.21","purl":"pkg:npm/lodash@4.17.21"}]}`))

	tests := []struct {
		purl string
		want []string
	}{
		{"pkg:npm/lodash", []string{"acme/api@4.17.21", "acme/web@4.17.20"}},
		{"pkg:npm/lodash@4.17.20", []string{"acme/web@4.17.20"}},
		{"pkg:npm/lodash@1.0.0", nil},
		{"pkg:npm/%40babel/core", []string{"acme/web@7.23.0"}},
		{"pkg:apk/alpine/musl@1.2.4-r2?arch=x86_64", []string{"acme/web@1.2.4-r2"}},
	}
	for _, tt := range tests {
		t.Run(tt.purl, func(t *testing.T) {
			var list ProjectListResponse
			decodeJSON(t, ts.do(t, http.MethodGet, "/api/projects?purl="+url.QueryEscape(tt.purl), "", nil), &list)
			var got []string
			for _, p := range list.Projects {
				got = append(got, p.Name+"@"+p.Version)
				if p.ComponentCount != nil {
					t.Errorf("%s has component_count set when listing by purl", p.Name)
				}
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("projects = %v, want %v", got, tt.want)
			}
		})
	}

	for _, bad := range []string{"lodash", "pkg:github/actions/checkout@v4"} {
		if rec := ts.do(t, http.MethodGet, "/api/projects?purl="+url.QueryEscape(bad), "", nil); rec.Code != http.StatusBadRequest {
			t.Errorf("purl %q status = %d, want 400", bad, rec.Code)
		}
	}
}

func TestProjectWritesRequireToken(t *testing.T) {
	ts := newProjectsTestServer(t)
	for _, token := range []string{"", "wrong-token"} {
		if rec := ts.do(t, http.MethodPut, "/api/projects/acme/web/sbom", token, syftSBOM(t)); rec.Code != http.StatusUnauthorized {
			t.Errorf("upload with token %q status = %d, want 401", token, rec.Code)
		}
		if rec := ts.do(t, http.MethodDelete, "/api/projects/acme/web", token, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("delete with token %q status = %d, want 401", token, rec.Code)
		}
	}
	if rec := ts.do(t, http.MethodGet, "/api/projects/acme/web", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("project exists after unauthorized upload: status %d", rec.Code)
	}
}

func TestProjectDelete(t *testing.T) {
	ts := newProjectsTestServer(t)
	ts.upload(t, "/api/projects/acme/web/sbom", syftSBOM(t))
	if rec := ts.do(t, http.MethodDelete, "/api/projects/acme%2Fweb", testProjectsToken, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d: %s", rec.Code, rec.Body)
	}
	if rec := ts.do(t, http.MethodDelete, "/api/projects/acme/web", testProjectsToken, nil); rec.Code != http.StatusNotFound {
		t.Errorf("second delete status = %d, want 404", rec.Code)
	}
	if rec := ts.do(t, http.MethodGet, "/api/projects/acme/web", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("get after delete status = %d, want 404", rec.Code)
	}
}

func TestProjectDeleteOfUploadPath(t *testing.T) {
	ts := newProjectsTestServer(t)
	ts.upload(t, "/api/projects/acme/web/sbom", syftSBOM(t))
	rec := ts.do(t, http.MethodDelete, "/api/projects/acme/web/sbom", testProjectsToken, nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "without /sbom") {
		t.Errorf("DELETE of the upload path = %d: %s, want 400 with a hint", rec.Code, rec.Body)
	}
	if rec := ts.do(t, http.MethodGet, "/api/projects/acme/web", "", nil); rec.Code != http.StatusOK {
		t.Errorf("project is gone after DELETE of the upload path: %d", rec.Code)
	}

	// A project whose last segment is "sbom" is deleted as usual.
	ts.upload(t, "/api/projects/acme/sbom/sbom", syftSBOM(t))
	if rec := ts.do(t, http.MethodDelete, "/api/projects/acme/sbom", testProjectsToken, nil); rec.Code != http.StatusNoContent {
		t.Errorf("DELETE of project acme/sbom = %d: %s", rec.Code, rec.Body)
	}
}

func TestProjectDeleteFromUI(t *testing.T) {
	ts := newProjectsTestServer(t)
	ts.upload(t, "/api/projects/acme/web/sbom", syftSBOM(t))
	for _, token := range []string{"", "wrong-token"} {
		if rec := ts.do(t, http.MethodDelete, "/ui/api/projects/acme%2Fweb", token, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("UI delete with token %q status = %d, want 401", token, rec.Code)
		}
	}
	if rec := ts.do(t, http.MethodDelete, "/ui/api/projects/acme%2Fweb", testProjectsToken, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("UI delete status = %d: %s", rec.Code, rec.Body)
	}
	if rec := ts.do(t, http.MethodGet, "/api/projects/acme/web", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("get after UI delete status = %d, want 404", rec.Code)
	}
}

func TestProjectPathsAreValidated(t *testing.T) {
	ts := newProjectsTestServer(t)
	uploads := []string{
		"/api/projects/sbom",
		"/api/projects/acme/web",
		"/api/projects/acme/../web/sbom",
		"/api/projects/acme%2F..%2Fweb/sbom",
		"/api/projects/..%2Fweb/sbom",
		"/api/projects/acme//web/sbom",
		"/api/projects/acme/./web/sbom",
		"/api/projects/acme%20web/sbom",
		"/api/projects/acme%2Fweb%3F/sbom",
		"/api/projects/" + strings.Repeat("a", 256) + "/sbom",
	}
	for _, target := range uploads {
		if rec := ts.do(t, http.MethodPut, target, testProjectsToken, syftSBOM(t)); rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %s status = %d, want 400", target, rec.Code)
		}
	}
	for _, target := range []string{"/api/projects/", "/api/projects/acme/../web", "/api/projects/acme%2F..%2Fweb"} {
		if rec := ts.do(t, http.MethodGet, target, "", nil); rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s status = %d, want 400", target, rec.Code)
		}
		if rec := ts.do(t, http.MethodDelete, target, testProjectsToken, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("DELETE %s status = %d, want 400", target, rec.Code)
		}
	}
	if projects, err := ts.db.ListProjects(); err != nil || len(projects) != 0 {
		t.Errorf("projects after invalid uploads = %v, %v", projects, err)
	}
}

func TestProjectUploadRejectsBadBodies(t *testing.T) {
	ts := newProjectsTestServer(t)
	tests := []struct {
		name   string
		target string
		body   []byte
		want   int
	}{
		{"empty", "/api/projects/acme/web/sbom", nil, http.StatusBadRequest},
		{"not an SBOM", "/api/projects/acme/web/sbom", []byte(`{"hello":"world"}`), http.StatusBadRequest},
		{"too large", "/api/projects/acme/web/sbom", make([]byte, maxSBOMBytes+1), http.StatusRequestEntityTooLarge},
		{"long ref", "/api/projects/acme/web/sbom?ref=" + strings.Repeat("a", maxRefLength+1), syftSBOM(t), http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if rec := ts.do(t, http.MethodPut, tt.target, testProjectsToken, tt.body); rec.Code != tt.want {
				t.Errorf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body)
			}
		})
	}
}

// startProjectsServer runs the real server with the given projects config.
func startProjectsServer(t *testing.T, projects config.ProjectsConfig) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving proxy address: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	tempDir := t.TempDir()
	cfg := config.Default()
	cfg.Listen = listener.Addr().String()
	cfg.BaseURL = "http://" + cfg.Listen
	cfg.Database.Path = filepath.Join(tempDir, "proxy.db")
	cfg.Storage.URL = "file://" + filepath.Join(tempDir, "artifacts")
	cfg.Projects = projects
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validating config: %v", err)
	}

	proxyServer, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), BuildInfo{Version: "test", Commit: "test"})
	if err != nil {
		t.Fatalf("creating server: %v", err)
	}
	startErr := make(chan error, 1)
	go func() { startErr <- proxyServer.Start(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = proxyServer.Shutdown(ctx)
		if err := <-startErr; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("server stopped with %v", err)
		}
	})
	return cfg.BaseURL
}

func request(t *testing.T, method, url, token string, body []byte) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

func TestProjectRoutesFollowConfig(t *testing.T) {
	disabled := startProjectsServer(t, config.ProjectsConfig{})
	for _, path := range []string{"/api/projects", "/api/projects/acme/web", "/ui/projects"} {
		if status, _ := request(t, http.MethodGet, disabled+path, "", nil); status != http.StatusNotFound {
			t.Errorf("GET %s with projects disabled = %d, want 404", path, status)
		}
	}
	if status, _ := request(t, http.MethodPut, disabled+"/api/projects/acme/web/sbom", testProjectsToken, syftSBOM(t)); status != http.StatusNotFound {
		t.Errorf("upload with projects disabled = %d, want 404", status)
	}
	if status, _ := request(t, http.MethodDelete, disabled+"/ui/api/projects/acme%2Fweb", testProjectsToken, nil); status != http.StatusNotFound {
		t.Errorf("UI delete with projects disabled = %d, want 404", status)
	}
	if _, body := request(t, http.MethodGet, disabled+"/ui/", "", nil); strings.Contains(body, `href="/ui/projects"`) {
		t.Error("navigation links to projects while they are disabled")
	}

	enabled := startProjectsServer(t, config.ProjectsConfig{Enabled: true, Token: testProjectsToken, MaxAge: "90d"})
	if status, _ := request(t, http.MethodPut, enabled+"/api/projects/acme%2Fweb/sbom", "", syftSBOM(t)); status != http.StatusUnauthorized {
		t.Errorf("upload without token = %d, want 401", status)
	}
	if status, body := request(t, http.MethodPut, enabled+"/api/projects/acme%2Fweb/sbom", testProjectsToken, syftSBOM(t)); status != http.StatusOK {
		t.Fatalf("upload = %d: %s", status, body)
	}
	if status, body := request(t, http.MethodGet, enabled+"/api/projects/acme/web", "", nil); status != http.StatusOK || !strings.Contains(body, `"name":"acme/web"`) {
		t.Errorf("GET project = %d: %s", status, body)
	}
	if status, body := request(t, http.MethodGet, enabled+"/ui/projects", "", nil); status != http.StatusOK || !strings.Contains(body, `href="/ui/projects/acme/web"`) {
		t.Errorf("projects page = %d, missing the project link", status)
	}
	if _, body := request(t, http.MethodGet, enabled+"/ui/", "", nil); !strings.Contains(body, `href="/ui/projects"`) {
		t.Error("navigation does not link to projects")
	}
	if status, _ := request(t, http.MethodDelete, enabled+"/ui/api/projects/acme%2Fweb", "", nil); status != http.StatusUnauthorized {
		t.Errorf("UI delete without token = %d, want 401", status)
	}
	if status, body := request(t, http.MethodDelete, enabled+"/ui/api/projects/acme%2Fweb", testProjectsToken, nil); status != http.StatusNoContent {
		t.Errorf("UI delete = %d: %s", status, body)
	}
	// The delete button sends its request there, too.
	if _, body := request(t, http.MethodGet, enabled+"/ui/static/project-delete.js", "", nil); !strings.Contains(body, "'/ui/api/projects/'") {
		t.Error("project-delete.js does not send to /ui/api/projects")
	}
}
