package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/storage"
	"github.com/git-pkgs/registries/fetch"
	"github.com/go-chi/chi/v5"
)

const (
	testHostedToken     = "push-token"
	testScopedToken     = "acme-token"
	testHostedMaxUpload = 4096
)

type hostedTestServer struct {
	router           http.Handler
	fetches          *int
	upstreamRequests *int
}

// countingFetcher fails every fetch, so a test fails loudly if the proxy goes
// upstream for something it should serve from the pushed copy.
type countingFetcher struct{ calls *int }

func (f countingFetcher) Fetch(ctx context.Context, url string) (*fetch.Artifact, error) {
	return f.FetchWithHeaders(ctx, url, nil)
}

func (f countingFetcher) FetchWithHeaders(context.Context, string, http.Header) (*fetch.Artifact, error) {
	*f.calls++
	return nil, fetch.ErrNotFound
}

func (f countingFetcher) Head(context.Context, string) (int64, string, error) {
	*f.calls++
	return 0, "", fetch.ErrNotFound
}

func newHostedTestServer(t *testing.T) hostedTestServer {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Create(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("database.Create: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := storage.OpenBucket(context.Background(), "file://"+filepath.Join(dir, "artifacts"))
	if err != nil {
		t.Fatalf("storage.OpenBucket: %v", err)
	}

	calls := 0
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := handler.NewProxy(db, store, countingFetcher{calls: &calls}, fetch.NewResolver(), logger)
	proxy.HostedEnabled = true

	upstreamRequests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamRequests++
		http.NotFound(w, nil)
	}))
	t.Cleanup(upstream.Close)

	r := chi.NewRouter()
	auth := newHostedAuth(config.HostedConfig{
		Token:  testHostedToken,
		Tokens: []config.HostedToken{{Name: "acme", Token: testScopedToken, Push: []string{"npm:@acme/*", "composer:acme/*"}}},
	})
	api := NewHostedAPIHandler(proxy, testHostedMaxUpload, logger)
	registerHostedRoutes(r, api, auth)
	npm := handler.NewNPMHandler(proxy, "http://proxy.test", upstream.URL).Routes()
	publish := requireHostedToken(auth)(http.HandlerFunc(api.HandleNPMPublish))
	r.Mount("/npm", http.StripPrefix("/npm", withNPMPublish(npm, publish)))
	composer := handler.NewComposerHandlerWithUpstreams(proxy, "http://proxy.test", upstream.URL, upstream.URL)
	r.Mount("/composer", http.StripPrefix("/composer", composer.Routes()))
	r.Mount("/generic", http.StripPrefix("/generic", handler.NewGenericHandler(proxy, nil).Routes()))
	return hostedTestServer{router: r, fetches: &calls, upstreamRequests: &upstreamRequests}
}

func (s hostedTestServer) do(t *testing.T, method, target, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	return rec
}

func TestHostedPushIsServedByTheEcosystemHandler(t *testing.T) {
	s := newHostedTestServer(t)

	rec := s.do(t, http.MethodPut, "/api/hosted/npm/request/2.88.3", testHostedToken, requestTarball)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d, body %s", rec.Code, rec.Body)
	}
	var pushed HostedPushResponse
	if err := json.NewDecoder(rec.Body).Decode(&pushed); err != nil {
		t.Fatalf("decoding PUT response: %v", err)
	}
	if pushed.Ecosystem != "npm" || pushed.Name != "request" || pushed.Version != "2.88.3" ||
		pushed.Filename != "request-2.88.3.tgz" || pushed.Size != int64(len(requestTarball)) {
		t.Errorf("PUT response = %+v", pushed)
	}

	rec = s.do(t, http.MethodGet, "/npm/request/-/request-2.88.3.tgz", "", "")
	if rec.Code != http.StatusOK || rec.Body.String() != requestTarball {
		t.Fatalf("download status = %d, body %q, want 200 and the pushed tarball", rec.Code, rec.Body)
	}
	if *s.fetches != 0 {
		t.Errorf("download contacted the upstream %d times", *s.fetches)
	}
}

func TestHostedPushIsServedByTheComposerHandler(t *testing.T) {
	s := newHostedTestServer(t)

	rec := s.do(t, http.MethodPut, "/api/hosted/composer/monolog/monolog/3.5.0", testHostedToken, monologZip)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d, body %s", rec.Code, rec.Body)
	}

	rec = s.do(t, http.MethodGet, "/composer/files/monolog/monolog/3.5.0/monolog-3.5.0.zip", "", "")
	if rec.Code != http.StatusOK || rec.Body.String() != monologZip {
		t.Fatalf("download status = %d, body %q, want 200 and the pushed archive", rec.Code, rec.Body)
	}
	if *s.upstreamRequests != 0 || *s.fetches != 0 {
		t.Errorf("download contacted the upstream (%d metadata requests, %d fetches)", *s.upstreamRequests, *s.fetches)
	}
}

func TestComposerDownloadOfUnpushedFileStillNeedsUpstreamMetadata(t *testing.T) {
	s := newHostedTestServer(t)
	if rec := s.do(t, http.MethodPut, "/api/hosted/composer/monolog/monolog/3.5.0", testHostedToken, monologZip); rec.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d, body %s", rec.Code, rec.Body)
	}

	rec := s.do(t, http.MethodGet, "/composer/files/monolog/monolog/3.5.0/other.zip", "", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("download of another filename status = %d, want 404 from the upstream lookup", rec.Code)
	}
	if *s.upstreamRequests == 0 {
		t.Error("a filename that was not pushed was served without the upstream metadata lookup")
	}
}

func TestHostedPushScopedNpmPackage(t *testing.T) {
	s := newHostedTestServer(t)

	rec := s.do(t, http.MethodPut, "/api/hosted/npm/@types/node/20.1.0", testHostedToken, nodeTarball)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d, body %s", rec.Code, rec.Body)
	}
	rec = s.do(t, http.MethodGet, "/npm/@types/node/-/node-20.1.0.tgz", "", "")
	if rec.Code != http.StatusOK || rec.Body.String() != nodeTarball {
		t.Fatalf("download status = %d, body %q, want 200 and the pushed tarball", rec.Code, rec.Body)
	}
}

func TestHostedListAndDelete(t *testing.T) {
	s := newHostedTestServer(t)
	if rec := s.do(t, http.MethodPut, "/api/hosted/npm/request/2.88.3", testHostedToken, requestTarball); rec.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d, body %s", rec.Code, rec.Body)
	}

	rec := s.do(t, http.MethodGet, "/api/hosted/npm/request", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body %s", rec.Code, rec.Body)
	}
	var listed HostedListResponse
	if err := json.NewDecoder(rec.Body).Decode(&listed); err != nil {
		t.Fatalf("decoding GET response: %v", err)
	}
	if listed.Ecosystem != "npm" || listed.Name != "request" || len(listed.Artifacts) != 1 || listed.Artifacts[0].Version != "2.88.3" {
		t.Errorf("GET response = %+v", listed)
	}

	if rec := s.do(t, http.MethodDelete, "/api/hosted/npm/request/2.88.3", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("DELETE without token status = %d, want 401", rec.Code)
	}
	if rec := s.do(t, http.MethodDelete, "/api/hosted/npm/request/2.88.3", testHostedToken, ""); rec.Code != http.StatusNoContent {
		t.Errorf("DELETE status = %d, body %s", rec.Code, rec.Body)
	}
	if rec := s.do(t, http.MethodDelete, "/api/hosted/npm/request/2.88.3", testHostedToken, ""); rec.Code != http.StatusNotFound {
		t.Errorf("second DELETE status = %d, want 404", rec.Code)
	}
}

func TestHostedPushRejections(t *testing.T) {
	tests := []struct {
		name       string
		target     string
		token      string
		body       string
		wantStatus int
	}{
		{name: "no token", target: "/api/hosted/npm/request/2.88.3", body: requestTarball, wantStatus: http.StatusUnauthorized},
		{name: "wrong token", target: "/api/hosted/npm/request/2.88.3", token: "guess", body: requestTarball, wantStatus: http.StatusUnauthorized},
		{name: "unsupported ecosystem", target: "/api/hosted/cargo/serde/1.0.200", token: testHostedToken, body: requestTarball, wantStatus: http.StatusBadRequest},
		{name: "composer name without vendor", target: "/api/hosted/composer/monolog/3.5.0", token: testHostedToken, body: requestTarball, wantStatus: http.StatusBadRequest},
		{name: "missing version", target: "/api/hosted/npm/request", token: testHostedToken, body: requestTarball, wantStatus: http.StatusBadRequest},
		{name: "not an archive", target: "/api/hosted/npm/request/2.88.3", token: testHostedToken, body: "plain text", wantStatus: http.StatusBadRequest},
		{name: "manifest of another version", target: "/api/hosted/npm/request/2.88.4", token: testHostedToken, body: requestTarball, wantStatus: http.StatusBadRequest},
		{name: "empty body", target: "/api/hosted/npm/request/2.88.3", token: testHostedToken, wantStatus: http.StatusBadRequest},
		{name: "too large", target: "/api/hosted/npm/request/2.88.3", token: testHostedToken, body: strings.Repeat("x", testHostedMaxUpload+1), wantStatus: http.StatusRequestEntityTooLarge},
		{name: "sha256 mismatch", target: "/api/hosted/npm/request/2.88.3?sha256=" + strings.Repeat("0", 64), token: testHostedToken, body: requestTarball, wantStatus: http.StatusBadRequest},
		{name: "malformed sha256", target: "/api/hosted/npm/request/2.88.3?sha256=abc", token: testHostedToken, body: requestTarball, wantStatus: http.StatusBadRequest},
		{name: "generic without file", target: "/api/hosted/generic/request/2.88.3", token: testHostedToken, body: "binary", wantStatus: http.StatusBadRequest},
		{name: "generic name with two segments", target: "/api/hosted/generic/acme/tools/1.0.0/tool.bin", token: testHostedToken, body: "binary", wantStatus: http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newHostedTestServer(t)
			rec := s.do(t, http.MethodPut, tt.target, tt.token, tt.body)
			if rec.Code != tt.wantStatus {
				t.Fatalf("PUT status = %d, want %d, body %s", rec.Code, tt.wantStatus, rec.Body)
			}
			listed := s.do(t, http.MethodGet, "/api/hosted/npm/request", "", "")
			if strings.Contains(listed.Body.String(), "2.88.3") {
				t.Error("rejected push was stored")
			}
		})
	}
}

func TestHostedListUnsupportedEcosystem(t *testing.T) {
	s := newHostedTestServer(t)
	if rec := s.do(t, http.MethodGet, "/api/hosted/maven/junit", "", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("GET status = %d, want 400", rec.Code)
	}
}

func startHostedServer(t *testing.T, hosted config.HostedConfig) string {
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
	cfg.Hosted = hosted
	upstream := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(upstream.Close)
	cfg.Upstream.NPM = upstream.URL
	cfg.Upstream.AllowLoopback = true
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
		<-startErr
	})
	return cfg.BaseURL
}

func putArchive(t *testing.T, url, token string) int {
	t.Helper()
	return putBody(t, url, token, requestTarball)
}

func putBody(t *testing.T, url, token, body string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", url, err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestHostedRoutesFollowConfig(t *testing.T) {
	disabled := startHostedServer(t, config.HostedConfig{})
	if status := putArchive(t, disabled+"/api/hosted/npm/request/2.88.3", testHostedToken); status == http.StatusCreated {
		t.Error("push accepted with hosted.enabled false")
	}

	enabled := startHostedServer(t, config.HostedConfig{Enabled: true, Token: testHostedToken})
	if status := putArchive(t, enabled+"/api/hosted/npm/request/2.88.3", ""); status != http.StatusUnauthorized {
		t.Errorf("push without token status = %d, want 401", status)
	}
	if status := putArchive(t, enabled+"/api/hosted/npm/request/2.88.3", testHostedToken); status != http.StatusCreated {
		t.Errorf("push with token status = %d, want 201", status)
	}
}

func TestHostedNpmPackageIsListedByTheRunningServer(t *testing.T) {
	base := startHostedServer(t, config.HostedConfig{Enabled: true, Token: testHostedToken})
	private := npmTarball(`{"name":"@acme/private","version":"1.0.0"}`)
	if status := putBody(t, base+"/api/hosted/npm/@acme/private/1.0.0", testHostedToken, private); status != http.StatusCreated {
		t.Fatalf("push status = %d, want 201", status)
	}

	resp, err := http.Get(base + "/npm/@acme%2fprivate")
	if err != nil {
		t.Fatalf("GET packument: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"1.0.0"`) {
		t.Errorf("packument status = %d, body %s", resp.StatusCode, body)
	}
}

func TestHostedComposerVersionIsInstallableFromMetadata(t *testing.T) {
	s := newHostedTestServer(t)
	if rec := s.do(t, http.MethodPut, "/api/hosted/composer/acme/private/1.0.0", testHostedToken,
		composerZip(`{"name":"acme/private","require":{"php":">=8.1"}}`)); rec.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d, body %s", rec.Code, rec.Body)
	}

	rec := s.do(t, http.MethodGet, "/composer/p2/acme/private.json", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("metadata status = %d, body %s", rec.Code, rec.Body)
	}
	var doc struct {
		Packages map[string][]struct {
			Dist struct {
				URL string `json:"url"`
			} `json:"dist"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decoding metadata: %v", err)
	}
	versions := doc.Packages["acme/private"]
	if len(versions) != 1 {
		t.Fatalf("metadata = %s", rec.Body)
	}
	distPath := strings.TrimPrefix(versions[0].Dist.URL, "http://proxy.test")
	if rec := s.do(t, http.MethodGet, distPath, "", ""); rec.Code != http.StatusOK {
		t.Errorf("download of %s status = %d", distPath, rec.Code)
	}
}

func TestHostedRepushIsRefused(t *testing.T) {
	s := newHostedTestServer(t)
	const target = "/api/hosted/npm/request/2.88.3"

	if rec := s.do(t, http.MethodPut, target, testHostedToken, requestTarball); rec.Code != http.StatusCreated {
		t.Fatalf("first PUT status = %d, body %s", rec.Code, rec.Body)
	}
	if rec := s.do(t, http.MethodPut, target, testHostedToken, requestTarball); rec.Code != http.StatusConflict {
		t.Fatalf("second PUT status = %d, want 409, body %s", rec.Code, rec.Body)
	}
	if rec := s.do(t, http.MethodDelete, target, testHostedToken, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d", rec.Code)
	}
	if rec := s.do(t, http.MethodPut, target, testHostedToken, requestTarball); rec.Code != http.StatusCreated {
		t.Fatalf("PUT after DELETE status = %d, want 201, body %s", rec.Code, rec.Body)
	}
}

func TestHostedComposerRepushUnderAnotherCaseIsRefused(t *testing.T) {
	s := newHostedTestServer(t)
	original := composerZip(`{"name":"acme/private","description":"first"}`)
	if rec := s.do(t, http.MethodPut, "/api/hosted/composer/acme/private/1.0.0", testHostedToken, original); rec.Code != http.StatusCreated {
		t.Fatalf("first PUT status = %d, body %s", rec.Code, rec.Body)
	}
	if rec := s.do(t, http.MethodPut, "/api/hosted/composer/acme/Private/1.0.0", testHostedToken,
		composerZip(`{"name":"acme/private","description":"second"}`)); rec.Code != http.StatusConflict {
		t.Fatalf("PUT of acme/Private status = %d, want 409, body %s", rec.Code, rec.Body)
	}
	rec := s.do(t, http.MethodGet, "/composer/files/acme/private/1.0.0/private-1.0.0.zip", "", "")
	if rec.Code != http.StatusOK || rec.Body.String() != original {
		t.Fatalf("download status = %d, want 200 and the first archive", rec.Code)
	}
}

func TestHostedPushWithMatchingSHA256(t *testing.T) {
	s := newHostedTestServer(t)
	digest := sha256.Sum256([]byte(requestTarball))
	target := "/api/hosted/npm/request/2.88.3?sha256=" + strings.ToUpper(hex.EncodeToString(digest[:]))

	if rec := s.do(t, http.MethodPut, target, testHostedToken, requestTarball); rec.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d, body %s", rec.Code, rec.Body)
	}
}

func TestHostedGenericFiles(t *testing.T) {
	s := newHostedTestServer(t)
	const (
		arm64 = "tool-linux-arm64.tar.gz"
		amd64 = "tool-linux-amd64.tar.gz"
	)

	for _, file := range []string{arm64, amd64} {
		rec := s.do(t, http.MethodPut, "/api/hosted/generic/tools/1.2.0/"+file, testHostedToken, "bytes of "+file)
		if rec.Code != http.StatusCreated {
			t.Fatalf("PUT %s status = %d, body %s", file, rec.Code, rec.Body)
		}
		var pushed HostedPushResponse
		if err := json.NewDecoder(rec.Body).Decode(&pushed); err != nil {
			t.Fatalf("decoding PUT response: %v", err)
		}
		digest := sha256.Sum256([]byte("bytes of " + file))
		if pushed.Filename != file || pushed.Version != "1.2.0" || pushed.SHA256 != hex.EncodeToString(digest[:]) {
			t.Errorf("PUT response = %+v", pushed)
		}
	}

	rec := s.do(t, http.MethodGet, "/generic/hosted/tools/1.2.0/"+arm64, "", "")
	if rec.Code != http.StatusOK || rec.Body.String() != "bytes of "+arm64 {
		t.Fatalf("download status = %d, body %q", rec.Code, rec.Body)
	}
	digest := sha256.Sum256([]byte("bytes of " + arm64))
	if etag := rec.Header().Get("ETag"); etag != `"`+hex.EncodeToString(digest[:])+`"` {
		t.Errorf("ETag = %q, want the sha256", etag)
	}
	if rec := s.do(t, http.MethodHead, "/generic/hosted/tools/1.2.0/"+arm64, "", ""); rec.Code != http.StatusOK {
		t.Errorf("HEAD status = %d", rec.Code)
	}

	listed := s.do(t, http.MethodGet, "/api/hosted/generic/tools", "", "")
	var list HostedListResponse
	if err := json.NewDecoder(listed.Body).Decode(&list); err != nil {
		t.Fatalf("decoding list: %v", err)
	}
	if len(list.Artifacts) != 2 {
		t.Fatalf("listed %d files, want 2: %+v", len(list.Artifacts), list.Artifacts)
	}

	if rec := s.do(t, http.MethodDelete, "/api/hosted/generic/tools/1.2.0/"+arm64, testHostedToken, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE file status = %d, body %s", rec.Code, rec.Body)
	}
	if rec := s.do(t, http.MethodGet, "/generic/hosted/tools/1.2.0/"+arm64, "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("download of deleted file status = %d, want 404", rec.Code)
	}
	if rec := s.do(t, http.MethodGet, "/generic/hosted/tools/1.2.0/"+amd64, "", ""); rec.Code != http.StatusOK {
		t.Errorf("download of the other file status = %d, want 200", rec.Code)
	}
	if rec := s.do(t, http.MethodDelete, "/api/hosted/generic/tools/1.2.0/"+arm64, testHostedToken, ""); rec.Code != http.StatusNotFound {
		t.Errorf("second DELETE status = %d, want 404", rec.Code)
	}

	for _, path := range []string{
		"/generic/hosted/tools/1.2.0/missing.bin",
		"/generic/hosted/tools/1.2.0",
		"/generic/hosted/tools/1.2.0/a/b",
	} {
		if rec := s.do(t, http.MethodGet, path, "", ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404", path, rec.Code)
		}
	}
	if *s.fetches != 0 || *s.upstreamRequests != 0 {
		t.Errorf("hosted generic files went upstream: %d fetches, %d requests", *s.fetches, *s.upstreamRequests)
	}
}

func TestHostedGenericFileIsServedByTheRunningServer(t *testing.T) {
	base := startHostedServer(t, config.HostedConfig{Enabled: true, Token: testHostedToken})
	if status := putBody(t, base+"/api/hosted/generic/tools/1.2.0/tool.tar.gz", testHostedToken, "tool bytes"); status != http.StatusCreated {
		t.Fatalf("push status = %d, want 201", status)
	}

	resp, err := http.Get(base + "/generic/hosted/tools/1.2.0/tool.tar.gz")
	if err != nil {
		t.Fatalf("GET file: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "tool bytes" {
		t.Errorf("download status = %d, body %q", resp.StatusCode, body)
	}
}

// unreadBody fails the test if the handler reads it.
type unreadBody struct{ t *testing.T }

func (b unreadBody) Read([]byte) (int, error) {
	b.t.Error("body of a refused push was read")
	return 0, io.EOF
}

func TestHostedRepushIsRefusedBeforeTheBodyIsRead(t *testing.T) {
	s := newHostedTestServer(t)
	const target = "/api/hosted/generic/tools/1.2.0/tool.tar.gz"
	if rec := s.do(t, http.MethodPut, target, testHostedToken, "first"); rec.Code != http.StatusCreated {
		t.Fatalf("first PUT status = %d, body %s", rec.Code, rec.Body)
	}

	req := httptest.NewRequest(http.MethodPut, target, unreadBody{t})
	req.Header.Set("Authorization", "Bearer "+testHostedToken)
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second PUT status = %d, want 409, body %s", rec.Code, rec.Body)
	}
}

func TestHostedDeleteRejectsMalformedPaths(t *testing.T) {
	s := newHostedTestServer(t)
	for _, target := range []string{
		"/api/hosted/generic/tools/1.2.0/%2e%2e",
		"/api/hosted/generic/tools/1.2.0/tool%0a",
		"/api/hosted/npm/request/2.88.3/request-2.88.3.tgz/extra",
		"/api/hosted/composer/monolog/3.5.0",
		"/api/hosted/cargo/serde/1.0.200",
	} {
		if rec := s.do(t, http.MethodDelete, target, testHostedToken, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("DELETE %s status = %d, want 400, body %s", target, rec.Code, rec.Body)
		}
	}
}
