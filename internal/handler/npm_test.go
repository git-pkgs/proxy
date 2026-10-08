package handler

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/registries/fetch"
)

const testVersion100 = "1.0.0"

func testProxy() *Proxy {
	return &Proxy{
		Logger:     slog.Default(),
		HTTPClient: http.DefaultClient,
	}
}

func TestNPMExtractVersionFromFilename(t *testing.T) {
	h := &NPMHandler{}

	tests := []struct {
		packageName string
		filename    string
		want        string
	}{
		{"lodash", "lodash-4.17.21.tgz", "4.17.21"},
		{"@babel/core", "core-7.23.0.tgz", "7.23.0"},
		{"@types/node", "node-20.10.0.tgz", "20.10.0"},
		{"express", "express-4.18.2.tgz", "4.18.2"},
		{"lodash", "lodash.tgz", ""},         // no version
		{"lodash", "lodash-4.17.21.zip", ""}, // wrong extension
		{"lodash", "other-4.17.21.tgz", ""},  // wrong package name
	}

	for _, tt := range tests {
		got := h.extractVersionFromFilename(tt.packageName, tt.filename)
		if got != tt.want {
			t.Errorf("extractVersionFromFilename(%q, %q) = %q, want %q",
				tt.packageName, tt.filename, got, tt.want)
		}
	}
}

func TestNPMHandlerUsesConfiguredUpstream(t *testing.T) {
	t.Run("metadata", func(t *testing.T) {
		var requestPath, authHeader string
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestPath = r.URL.Path
			authHeader = r.Header.Get("Authorization")
			if authHeader != "Bearer npm-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"versions":{}}`)
		}))
		defer upstream.Close()

		proxy, _, _, _ := setupTestProxy(t)
		proxy.HTTPClient = upstream.Client()
		proxy.AuthForURL = func(string) (string, string) {
			return "Authorization", "Bearer npm-token"
		}
		h := NewNPMHandler(proxy, "http://proxy.test", upstream.URL+"/root/")

		req := httptest.NewRequest(http.MethodGet, "/testpkg", nil)
		w := httptest.NewRecorder()
		h.Routes().ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
		}
		if requestPath != "/root/testpkg" {
			t.Errorf("upstream path = %q, want %q", requestPath, "/root/testpkg")
		}
		if authHeader != "Bearer npm-token" {
			t.Errorf("Authorization = %q, want %q", authHeader, "Bearer npm-token")
		}
	})

	t.Run("download", func(t *testing.T) {
		proxy, _, _, artifactFetcher := setupTestProxy(t)
		artifactFetcher.artifact = &fetch.Artifact{
			Body:        io.NopCloser(strings.NewReader("package")),
			ContentType: "application/gzip",
		}
		h := NewNPMHandler(proxy, "http://proxy.test", "https://npm.example.test/root/")

		req := httptest.NewRequest(http.MethodGet, "/testpkg/-/testpkg-1.0.0.tgz", nil)
		w := httptest.NewRecorder()
		h.Routes().ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
		}
		want := "https://npm.example.test/root/testpkg/-/testpkg-1.0.0.tgz"
		if artifactFetcher.fetchedURL != want {
			t.Errorf("fetched URL = %q, want %q", artifactFetcher.fetchedURL, want)
		}
	})

	t.Run("scoped download", func(t *testing.T) {
		proxy, _, _, artifactFetcher := setupTestProxy(t)
		artifactFetcher.artifact = &fetch.Artifact{
			Body:        io.NopCloser(strings.NewReader("package")),
			ContentType: "application/gzip",
		}
		h := NewNPMHandler(proxy, "http://proxy.test", "https://npm.example.test/root/")

		req := httptest.NewRequest(http.MethodGet, "/@scope/name/-/name-1.0.0.tgz", nil)
		w := httptest.NewRecorder()
		h.Routes().ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
		}
		want := "https://npm.example.test/root/@scope/name/-/name-1.0.0.tgz"
		if artifactFetcher.fetchedURL != want {
			t.Errorf("fetched URL = %q, want %q", artifactFetcher.fetchedURL, want)
		}
	})
}

func TestNPMRewriteMetadata(t *testing.T) {
	h := &NPMHandler{
		proxy:    testProxy(),
		proxyURL: "http://localhost:8080",
	}

	input := `{
		"name": "lodash",
		"versions": {
			"4.17.21": {
				"name": "lodash",
				"version": "4.17.21",
				"dist": {
					"tarball": "https://registry.npmjs.org/lodash/-/lodash-4.17.21.tgz",
					"shasum": "abc123"
				}
			}
		}
	}`

	output, err := h.rewriteMetadata("lodash", []byte(input))
	if err != nil {
		t.Fatalf("rewriteMetadata failed: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	versions := result["versions"].(map[string]any)
	v := versions["4.17.21"].(map[string]any)
	dist := v["dist"].(map[string]any)
	tarball := dist["tarball"].(string)

	expected := "http://localhost:8080/npm/lodash/-/lodash-4.17.21.tgz"
	if tarball != expected {
		t.Errorf("tarball = %q, want %q", tarball, expected)
	}
}

func TestNPMRewriteMetadataScopedPackage(t *testing.T) {
	h := &NPMHandler{
		proxy:    testProxy(),
		proxyURL: "http://localhost:8080",
	}

	input := `{
		"name": "@babel/core",
		"versions": {
			"7.23.0": {
				"name": "@babel/core",
				"version": "7.23.0",
				"dist": {
					"tarball": "https://registry.npmjs.org/@babel/core/-/core-7.23.0.tgz"
				}
			}
		}
	}`

	output, err := h.rewriteMetadata("@babel/core", []byte(input))
	if err != nil {
		t.Fatalf("rewriteMetadata failed: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	versions := result["versions"].(map[string]any)
	v := versions["7.23.0"].(map[string]any)
	dist := v["dist"].(map[string]any)
	tarball := dist["tarball"].(string)

	expected := "http://localhost:8080/npm/@babel%2Fcore/-/core-7.23.0.tgz"
	if tarball != expected {
		t.Errorf("tarball = %q, want %q", tarball, expected)
	}
}

func TestNPMHandlerMetadataProxy(t *testing.T) {
	// Create a mock upstream server
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/testpkg" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"name": "testpkg",
			"versions": {
				"1.0.0": {
					"name": "testpkg",
					"version": "1.0.0",
					"dist": {
						"tarball": "https://registry.npmjs.org/testpkg/-/testpkg-1.0.0.tgz"
					}
				}
			}
		}`))
	}))
	defer upstream.Close()

	h := &NPMHandler{
		proxy:       testProxy(),
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	// Test metadata request
	req := httptest.NewRequest(http.MethodGet, "/testpkg", nil)
	req.SetPathValue("name", "testpkg")

	w := httptest.NewRecorder()
	h.handlePackageMetadata(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	// Check that tarball URL was rewritten
	versions := result["versions"].(map[string]any)
	v := versions[testVersion100].(map[string]any)
	dist := v["dist"].(map[string]any)
	tarball := dist["tarball"].(string)

	if tarball != "http://proxy.local/npm/testpkg/-/testpkg-1.0.0.tgz" {
		t.Errorf("tarball URL not rewritten correctly: %s", tarball)
	}
}

func TestNPMRewriteMetadataCooldown(t *testing.T) {
	now := time.Now()
	old := now.Add(-10 * 24 * time.Hour).Format(time.RFC3339)
	recent := now.Add(-1 * time.Hour).Format(time.RFC3339)

	proxy := testProxy()
	proxy.Cooldown = &cooldown.Config{Default: "3d"}

	h := &NPMHandler{
		proxy:    proxy,
		proxyURL: "http://localhost:8080",
	}

	input := `{
		"name": "testpkg",
		"dist-tags": {"latest": "2.0.0"},
		"time": {
			"1.0.0": "` + old + `",
			"2.0.0": "` + recent + `"
		},
		"versions": {
			"1.0.0": {
				"name": "testpkg",
				"version": "1.0.0",
				"dist": {
					"tarball": "https://registry.npmjs.org/testpkg/-/testpkg-1.0.0.tgz"
				}
			},
			"2.0.0": {
				"name": "testpkg",
				"version": "2.0.0",
				"dist": {
					"tarball": "https://registry.npmjs.org/testpkg/-/testpkg-2.0.0.tgz"
				}
			}
		}
	}`

	output, err := h.rewriteMetadata("testpkg", []byte(input))
	if err != nil {
		t.Fatalf("rewriteMetadata failed: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	versions := result["versions"].(map[string]any)

	// Old version should remain
	if _, ok := versions[testVersion100]; !ok {
		t.Error("version 1.0.0 should not be filtered")
	}

	// Recent version should be filtered
	if _, ok := versions["2.0.0"]; ok {
		t.Error("version 2.0.0 should be filtered by cooldown")
	}

	// dist-tags.latest should be updated to 1.0.0
	distTags := result["dist-tags"].(map[string]any)
	if distTags["latest"] != testVersion100 {
		t.Errorf("dist-tags.latest = %q, want %q", distTags["latest"], testVersion100)
	}
}

func TestNPMRewriteMetadataCooldownExemptPackage(t *testing.T) {
	now := time.Now()
	recent := now.Add(-1 * time.Hour).Format(time.RFC3339)

	proxy := testProxy()
	proxy.Cooldown = &cooldown.Config{
		Default:  "3d",
		Packages: map[string]string{"pkg:npm/testpkg": "0"},
	}

	h := &NPMHandler{
		proxy:    proxy,
		proxyURL: "http://localhost:8080",
	}

	input := `{
		"name": "testpkg",
		"time": {"1.0.0": "` + recent + `"},
		"versions": {
			"1.0.0": {
				"name": "testpkg",
				"version": "1.0.0",
				"dist": {"tarball": "https://registry.npmjs.org/testpkg/-/testpkg-1.0.0.tgz"}
			}
		}
	}`

	output, err := h.rewriteMetadata("testpkg", []byte(input))
	if err != nil {
		t.Fatalf("rewriteMetadata failed: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	versions := result["versions"].(map[string]any)
	if _, ok := versions[testVersion100]; !ok {
		t.Error("exempt package version should not be filtered")
	}
}

func TestNPMHandlerUsesAbbreviatedMetadata(t *testing.T) {
	var gotAccept string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"name": "testpkg",
			"versions": {
				"1.0.0": {
					"name": "testpkg",
					"version": "1.0.0",
					"dist": {
						"tarball": "https://registry.npmjs.org/testpkg/-/testpkg-1.0.0.tgz"
					}
				}
			}
		}`))
	}))
	defer upstream.Close()

	t.Run("no cooldown uses combined accept header", func(t *testing.T) {
		h := &NPMHandler{
			proxy:       testProxy(),
			upstreamURL: upstream.URL,
			proxyURL:    "http://proxy.local",
		}

		req := httptest.NewRequest(http.MethodGet, "/testpkg", nil)
		w := httptest.NewRecorder()
		h.handlePackageMetadata(w, req)

		if gotAccept != npmAcceptDefault {
			t.Errorf("Accept = %q, want %q", gotAccept, npmAcceptDefault)
		}
	})

	t.Run("cooldown enabled uses full metadata only", func(t *testing.T) {
		proxy := testProxy()
		proxy.Cooldown = &cooldown.Config{Default: "3d"}

		h := &NPMHandler{
			proxy:       proxy,
			upstreamURL: upstream.URL,
			proxyURL:    "http://proxy.local",
		}

		req := httptest.NewRequest(http.MethodGet, "/testpkg", nil)
		w := httptest.NewRecorder()
		h.handlePackageMetadata(w, req)

		if gotAccept != contentTypeJSON {
			t.Errorf("Accept = %q, want %q (cooldown requires full metadata)", gotAccept, contentTypeJSON)
		}
	})

	t.Run("full metadata option uses full metadata without cooldown", func(t *testing.T) {
		proxy := testProxy()
		proxy.NPMFullMetadata = true

		h := &NPMHandler{
			proxy:       proxy,
			upstreamURL: upstream.URL,
			proxyURL:    "http://proxy.local",
		}

		req := httptest.NewRequest(http.MethodGet, "/testpkg", nil)
		w := httptest.NewRecorder()
		h.handlePackageMetadata(w, req)

		if gotAccept != contentTypeJSON {
			t.Errorf("Accept = %q, want %q (npm_full_metadata requires full metadata)", gotAccept, contentTypeJSON)
		}
	})
}

func TestNPMHandlerMetadataNotFound(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	h := &NPMHandler{
		proxy:       testProxy(),
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/nonexistent", nil)
	req.SetPathValue("name", "nonexistent")

	w := httptest.NewRecorder()
	h.handlePackageMetadata(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestNPMDownloadCooldown(t *testing.T) {
	now := time.Now()
	packument := `{
		"name": "leftpad",
		"dist-tags": {"latest": "2.0.0"},
		"time": {
			"1.0.0": "` + now.Add(-30*24*time.Hour).Format(time.RFC3339) + `",
			"2.0.0": "` + now.Add(-1*time.Hour).Format(time.RFC3339) + `"
		},
		"versions": {"1.0.0": {}, "2.0.0": {}}
	}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentTypeJSON)
		_, _ = io.WriteString(w, packument)
	}))
	defer upstream.Close()

	tests := []struct {
		name       string
		version    string
		wantStatus int
	}{
		{"published before the window serves the tarball", testVersion100, http.StatusOK},
		{"published inside the window is withheld", "2.0.0", http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxy, _, _, fetcher := setupTestProxy(t)
			proxy.HTTPClient = upstream.Client()
			proxy.Cooldown = &cooldown.Config{Default: "7d"}
			fetcher.artifact = &fetch.Artifact{
				Body:        io.NopCloser(strings.NewReader("tarball data")),
				ContentType: "application/octet-stream",
			}

			h := NewNPMHandler(proxy, "http://proxy.test", upstream.URL)
			srv := httptest.NewServer(h.Routes())
			defer srv.Close()

			resp, err := http.Get(srv.URL + "/leftpad/-/leftpad-" + tt.version + ".tgz")
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if tt.wantStatus == http.StatusNotFound && fetcher.fetchCalled {
				t.Error("fetched a version that is still inside the cooldown window")
			}
		})
	}
}

func TestNPMDownloadCooldownDisabled(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	h := NewNPMHandler(proxy, "http://proxy.test", "")

	if h.versionInCooldown("leftpad", testVersion100, func() ([]byte, error) {
		t.Fatal("cooldown must not request metadata when disabled")
		return nil, nil
	}) {
		t.Error("versionInCooldown = true, want false when cooldown is not configured")
	}
}

func TestNPMDownloadCooldownUsesStoredPublishTime(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("metadata must not be fetched when the publish time is already stored")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	tests := []struct {
		name        string
		version     string
		publishedAt time.Time
		wantStatus  int
	}{
		{"stored time before the window serves the tarball", testVersion100, time.Now().Add(-30 * 24 * time.Hour), http.StatusOK},
		{"stored time inside the window is withheld", "2.0.0", time.Now().Add(-1 * time.Hour), http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxy, db, store, _ := setupTestProxy(t)
			proxy.HTTPClient = upstream.Client()
			proxy.Cooldown = &cooldown.Config{Default: "7d"}
			seedPackage(t, db, store, "npm", "leftpad", tt.version, "leftpad-"+tt.version+".tgz", "tarball data")

			if err := db.SetVersionPublishedAt("pkg:npm/leftpad@"+tt.version, "pkg:npm/leftpad", tt.publishedAt); err != nil {
				t.Fatalf("seeding publish time failed: %v", err)
			}

			h := NewNPMHandler(proxy, "http://proxy.test", upstream.URL)
			srv := httptest.NewServer(h.Routes())
			defer srv.Close()

			resp, err := http.Get(srv.URL + "/leftpad/-/leftpad-" + tt.version + ".tgz")
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
		})
	}
}

func TestNPMDownloadCooldownFetchesMetadataOnce(t *testing.T) {
	now := time.Now()
	packument := `{
		"name": "leftpad",
		"dist-tags": {"latest": "1.0.0"},
		"time": {
			"1.0.0": "` + now.Add(-30*24*time.Hour).Format(time.RFC3339) + `"
		},
		"versions": {"1.0.0": {}}
	}`

	var metadataRequests atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		metadataRequests.Add(1)
		w.Header().Set("Content-Type", contentTypeJSON)
		_, _ = io.WriteString(w, packument)
	}))
	defer upstream.Close()

	proxy, _, _, fetcher := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	proxy.Cooldown = &cooldown.Config{Default: "7d"}

	h := NewNPMHandler(proxy, "http://proxy.test", upstream.URL)
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	// The first download parses the packument once and persists the publish
	// time; caching the artifact afterwards upserts the versions row without a
	// publish time, which must not erase the stored value. The second download
	// must answer from the stored time alone.
	for i := 0; i < 2; i++ {
		fetcher.artifact = &fetch.Artifact{
			Body:        io.NopCloser(strings.NewReader("tarball data")),
			ContentType: "application/octet-stream",
		}
		resp, err := http.Get(srv.URL + "/leftpad/-/leftpad-" + testVersion100 + ".tgz")
		if err != nil {
			t.Fatalf("request %d failed: %v", i+1, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d status = %d, want %d", i+1, resp.StatusCode, http.StatusOK)
		}
	}

	if got := metadataRequests.Load(); got != 1 {
		t.Errorf("metadata requests = %d, want 1", got)
	}
}

// TestNPMDownloadErrorResponsesAreJSON guards against a regression where
// routing handleDownload's error path through the shared serveArtifactError
// helper silently switched npm's 404/502 tarball error bodies from JSON to
// plain text; npm clients expect a JSON {"error": "..."} body on every
// download failure, including the newer scan-blocked (403) case.
func TestNPMDownloadErrorResponsesAreJSON(t *testing.T) {
	tests := []struct {
		name       string
		fetchErr   error
		blocked    bool
		wantStatus int
	}{
		{"upstream not found", fetch.ErrNotFound, false, http.StatusNotFound},
		{"upstream failure", errors.New("connection refused"), false, http.StatusBadGateway},
		{"blocked by scan", nil, true, http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxy, _, _, fetcher := setupTestProxy(t)
			proxy.ScanSigningKey = []byte("test-signing-key")
			if tt.blocked {
				proxy.Scanners = newTestScanGroup(t, newTestScanServer(t, false, "malware detected").URL, false)
			}
			fetcher.fetchErr = tt.fetchErr
			fetcher.artifact = &fetch.Artifact{
				Body:        io.NopCloser(strings.NewReader("tarball data")),
				ContentType: "application/octet-stream",
			}

			h := NewNPMHandler(proxy, "http://proxy.test", "http://upstream.invalid")
			srv := httptest.NewServer(h.Routes())
			defer srv.Close()

			resp, err := http.Get(srv.URL + "/leftpad/-/leftpad-1.0.0.tgz")
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if ct := resp.Header.Get("Content-Type"); ct != contentTypeJSON {
				t.Errorf("Content-Type = %q, want %q", ct, contentTypeJSON)
			}
			var body map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("response body is not valid JSON: %v", err)
			}
			if _, ok := body["error"]; !ok {
				t.Errorf("response body %v missing \"error\" key", body)
			}
		})
	}
}

// newNPMAuditUpstream returns a handler pointed at a stub registry, plus the
// last request that registry saw.
func newNPMAuditUpstream(t *testing.T, respond http.HandlerFunc) (*NPMHandler, *npmAuditCapture) {
	t.Helper()

	capture := &npmAuditCapture{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture.method = r.Method
		capture.path = r.URL.Path
		capture.query = r.URL.RawQuery
		capture.contentType = r.Header.Get("Content-Type")
		capture.contentEncoding = r.Header.Get("Content-Encoding")
		capture.authorization = r.Header.Get("Authorization")
		capture.body, _ = io.ReadAll(r.Body)
		respond(w, r)
	}))
	t.Cleanup(upstream.Close)

	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	return NewNPMHandler(proxy, "http://proxy.test", upstream.URL), capture
}

type npmAuditCapture struct {
	method          string
	path            string
	query           string
	contentType     string
	contentEncoding string
	authorization   string
	body            []byte
}

func npmAuditJSON(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

func serveNPM(t *testing.T, h *NPMHandler, method, target string, body io.Reader,
	headers map[string]string,
) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, target, body)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)
	return w
}

func TestNPMAuditRelaysRequestAndResponse(t *testing.T) {
	const report = `{"actions":[],"advisories":{},"metadata":{"vulnerabilities":{"total":0}}}`
	const payload = `{"name":"app","requires":{"lodash":"^4.17.21"},"dependencies":{}}`

	h, got := newNPMAuditUpstream(t, npmAuditJSON(report))
	w := serveNPM(t, h, http.MethodPost, "/-/npm/v1/security/audits?foo=bar",
		strings.NewReader(payload), map[string]string{"Content-Type": "application/json"})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if w.Body.String() != report {
		t.Errorf("body = %q, want %q", w.Body.String(), report)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("response Content-Type = %q, want application/json", ct)
	}
	if got.method != http.MethodPost {
		t.Errorf("upstream method = %q, want POST", got.method)
	}
	if got.path != "/-/npm/v1/security/audits" {
		t.Errorf("upstream path = %q, want /-/npm/v1/security/audits", got.path)
	}
	if got.query != "foo=bar" {
		t.Errorf("upstream query = %q, want foo=bar", got.query)
	}
	if string(got.body) != payload {
		t.Errorf("upstream body = %q, want %q", got.body, payload)
	}
	if got.contentType != "application/json" {
		t.Errorf("upstream Content-Type = %q, want application/json", got.contentType)
	}
}

// npm gzips its audit payload, so the bytes and the header describing them
// must travel together.
func TestNPMAuditForwardsGzippedBody(t *testing.T) {
	var gzipped bytes.Buffer
	zw := gzip.NewWriter(&gzipped)
	if _, err := io.WriteString(zw, `{"name":"app"}`); err != nil {
		t.Fatalf("writing gzip body: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing gzip writer: %v", err)
	}
	want := gzipped.Bytes()

	h, got := newNPMAuditUpstream(t, npmAuditJSON(`{}`))
	w := serveNPM(t, h, http.MethodPost, "/-/npm/v1/security/audits",
		bytes.NewReader(want), map[string]string{
			"Content-Type":     "application/json",
			"Content-Encoding": "gzip",
		})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if got.contentEncoding != "gzip" {
		t.Errorf("upstream Content-Encoding = %q, want gzip", got.contentEncoding)
	}
	if !bytes.Equal(got.body, want) {
		t.Errorf("upstream body was altered: got %d bytes, want %d", len(got.body), len(want))
	}
}

func TestNPMAuditCoversAllSecurityEndpoints(t *testing.T) {
	paths := []string{
		"/-/npm/v1/security/audits",          // pnpm audit, npm audit (full)
		"/-/npm/v1/security/audits/quick",    // yarn npm audit, npm audit fallback
		"/-/npm/v1/security/advisories/bulk", // npm audit (npm 7+)
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			h, got := newNPMAuditUpstream(t, npmAuditJSON(`{}`))
			w := serveNPM(t, h, http.MethodPost, path, strings.NewReader(`{}`), nil)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
			}
			if got.path != path {
				t.Errorf("upstream path = %q, want %q", got.path, path)
			}
		})
	}
}

func TestNPMAuditAppliesUpstreamAuth(t *testing.T) {
	h, got := newNPMAuditUpstream(t, npmAuditJSON(`{}`))
	h.proxy.AuthForURL = func(string) (string, string) {
		return "Authorization", "Bearer npm-token"
	}

	serveNPM(t, h, http.MethodPost, "/-/npm/v1/security/audits", strings.NewReader(`{}`), nil)

	if got.authorization != "Bearer npm-token" {
		t.Errorf("Authorization = %q, want %q", got.authorization, "Bearer npm-token")
	}
}

func TestNPMAuditRelaysUpstreamError(t *testing.T) {
	const upstreamBody = `{"error":"unauthorized"}`

	h, _ := newNPMAuditUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, upstreamBody)
	})
	w := serveNPM(t, h, http.MethodPost, "/-/npm/v1/security/audits", strings.NewReader(`{}`), nil)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
	if w.Body.String() != upstreamBody {
		t.Errorf("body = %q, want %q", w.Body.String(), upstreamBody)
	}
}

// A proxy-side failure must still be JSON, or the client reports it as a
// malformed audit response.
func TestNPMAuditUpstreamUnreachable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	upstreamURL := upstream.URL
	upstream.Close() // nothing is listening now

	proxy, _, _, _ := setupTestProxy(t)
	h := NewNPMHandler(proxy, "http://proxy.test", upstreamURL)

	w := serveNPM(t, h, http.MethodPost, "/-/npm/v1/security/audits", strings.NewReader(`{}`), nil)

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadGateway)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if !json.Valid(w.Body.Bytes()) {
		t.Errorf("body is not valid JSON: %q", w.Body.String())
	}
}

func TestNPMAuditRejectsBadRequests(t *testing.T) {
	t.Run("non-POST method", func(t *testing.T) {
		proxy, _, _, _ := setupTestProxy(t)
		h := NewNPMHandler(proxy, "http://proxy.test", "https://npm.example.test")

		w := serveNPM(t, h, http.MethodGet, "/-/npm/v1/security/audits", nil, nil)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
		}
		if !json.Valid(w.Body.Bytes()) {
			t.Errorf("body is not valid JSON: %q", w.Body.String())
		}
	})

	t.Run("body over the size cap", func(t *testing.T) {
		proxy, _, _, _ := setupTestProxy(t)
		h := NewNPMHandler(proxy, "http://proxy.test", "https://npm.example.test")

		body := strings.NewReader(strings.Repeat("a", npmSecurityMaxBody+1))
		w := serveNPM(t, h, http.MethodPost, "/-/npm/v1/security/audits", body, nil)

		if w.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d, want %d", w.Code, http.StatusRequestEntityTooLarge)
		}
	})

	t.Run("unreadable body is not reported as too large", func(t *testing.T) {
		proxy, _, _, _ := setupTestProxy(t)
		h := NewNPMHandler(proxy, "http://proxy.test", "https://npm.example.test")

		req := httptest.NewRequest(http.MethodPost, "/-/npm/v1/security/audits",
			iotest.TimeoutReader(strings.NewReader("{}")))
		w := httptest.NewRecorder()
		h.Routes().ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		if !json.Valid(w.Body.Bytes()) {
			t.Errorf("body is not valid JSON: %q", w.Body.String())
		}
	})
}

func TestNPMAuditDoesNotInjectUpstreamQuery(t *testing.T) {
	h, got := newNPMAuditUpstream(t, npmAuditJSON(`{}`))

	serveNPM(t, h, http.MethodPost,
		"/-/npm/v1/security/audits%3Fevil=1", strings.NewReader(`{}`), nil)

	if got.query != "" {
		t.Errorf("upstream query = %q, want empty: encoded ? leaked into the query", got.query)
	}
	if got.path != "/-/npm/v1/security/audits?evil=1" {
		t.Errorf("upstream path = %q, want the encoded ? kept in the path", got.path)
	}
}

// The audit POST must go through relayResponse, not copy upstream headers
// wholesale: a relayed Connection header would be honoured downstream.
// TestRelayRoutes covers the GET paths; this covers the POST.
func TestNPMAuditStripsHopByHopHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = fmt.Fprint(rw, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n"+
			"Connection: X-Private\r\nX-Private: secret\r\nContent-Length: 2\r\n\r\n{}")
		_ = rw.Flush()
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	downstream := httptest.NewServer(NewNPMHandler(proxy, "http://proxy.test", upstream.URL).Routes())
	defer downstream.Close()

	resp, err := downstream.Client().Post(
		downstream.URL+"/-/npm/v1/security/audits", contentTypeJSON, strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.Header.Get("Connection") != "" || resp.Header.Get("X-Private") != "" {
		t.Errorf("connection-scoped headers leaked: %v", resp.Header)
	}
}

// `npm audit signatures` reads the registry signing keys. The path used to fall
// through to the package dispatch and be escaped into a package name.
func TestNPMKeysProxiesUpstream(t *testing.T) {
	const keys = `{"keys":[{"keyid":"SHA256:jl3bwswu","keytype":"ecdsa-sha2-nistp256"}]}`

	var gotPath, gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// EscapedPath, not Path: the bug escaped the slashes into a package
		// name, and the server decodes %2F back into Path either way.
		gotPath, gotAuth = r.URL.EscapedPath(), r.Header.Get("Authorization")
		if gotPath != npmKeysPath {
			w.WriteHeader(http.StatusMethodNotAllowed) // what registry.npmjs.org answers
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, keys)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	proxy.AuthForURL = func(string) (string, string) {
		return "Authorization", "Bearer npm-token"
	}
	h := NewNPMHandler(proxy, "http://proxy.test", upstream.URL)

	w := serveNPM(t, h, http.MethodGet, npmKeysPath, nil, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if w.Body.String() != keys {
		t.Errorf("body = %q, want %q", w.Body.String(), keys)
	}
	if gotPath != npmKeysPath {
		t.Errorf("upstream path = %q, want %q", gotPath, npmKeysPath)
	}
	if gotAuth != "Bearer npm-token" {
		t.Errorf("Authorization = %q, want it applied", gotAuth)
	}
}

// Tarball paths share the /-/ prefix with the /-/npm/v1 endpoints.
func TestNPMTarballStillRoutesToDownload(t *testing.T) {
	proxy, _, _, artifactFetcher := setupTestProxy(t)
	artifactFetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("package")),
		ContentType: "application/gzip",
	}
	h := NewNPMHandler(proxy, "http://proxy.test", "https://npm.example.test")

	w := serveNPM(t, h, http.MethodGet, "/lodash/-/lodash-4.17.21.tgz", nil, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
}

func TestNPMMetadataPath(t *testing.T) {
	tests := []struct {
		path, wantName, wantRegistry string
	}{
		{"", "", ""},
		{"lodash", "lodash", "lodash"},
		{"lodash/4.17.21", "lodash", "lodash/4.17.21"},
		{"lodash/latest", "lodash", "lodash/latest"},
		{"@babel/core", "@babel/core", "@babel%2Fcore"},
		{"@babel/core/7.23.0", "@babel/core", "@babel%2Fcore/7.23.0"},
	}
	for _, tt := range tests {
		name, registry := npmMetadataPath(tt.path)
		if name != tt.wantName || registry != tt.wantRegistry {
			t.Errorf("npmMetadataPath(%q) = (%q, %q), want (%q, %q)",
				tt.path, name, registry, tt.wantName, tt.wantRegistry)
		}
	}
}

func TestNPMVersionMetadataKeepsVersionSegment(t *testing.T) {
	tests := []struct {
		request, upstreamPath, wantTarball string
		body                                string
	}{
		{
			request:      "/lodash/4.17.21",
			upstreamPath: "/lodash/4.17.21",
			wantTarball:  "http://proxy.local/npm/lodash/-/lodash-4.17.21.tgz",
			body: `{
				"name": "lodash",
				"version": "4.17.21",
				"dist": {"tarball": "https://registry.npmjs.org/lodash/-/lodash-4.17.21.tgz"}
			}`,
		},
		{
			request:      "/@babel/core/7.23.0",
			upstreamPath: "/@babel%2Fcore/7.23.0",
			wantTarball:  "http://proxy.local/npm/@babel%2Fcore/-/core-7.23.0.tgz",
			body: `{
				"name": "@babel/core",
				"version": "7.23.0",
				"dist": {"tarball": "https://registry.npmjs.org/@babel/core/-/core-7.23.0.tgz"}
			}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.request, func(t *testing.T) {
			var gotPath string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.EscapedPath()
				if gotPath != tt.upstreamPath {
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tt.body)
			}))
			defer upstream.Close()

			h := &NPMHandler{proxy: testProxy(), upstreamURL: upstream.URL, proxyURL: "http://proxy.local"}
			w := httptest.NewRecorder()
			h.handlePackageMetadata(w, httptest.NewRequest(http.MethodGet, tt.request, nil))

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
			}
			if gotPath != tt.upstreamPath {
				t.Fatalf("upstream path = %q, want %q", gotPath, tt.upstreamPath)
			}
			var result map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatalf("parse response: %v", err)
			}
			if result["dist"].(map[string]any)["tarball"] != tt.wantTarball {
				t.Errorf("tarball = %v", result["dist"].(map[string]any)["tarball"])
			}
		})
	}
}

func TestNPMMetadataRejectsEmptyPath(t *testing.T) {
	h := NewNPMHandler(testProxy(), "http://proxy.local", "http://unused.test")
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

func TestNPMMetadataRejectsScopeOnly(t *testing.T) {
	h := &NPMHandler{proxy: testProxy(), upstreamURL: "http://unused.test", proxyURL: "http://proxy.local"}
	w := httptest.NewRecorder()
	h.handlePackageMetadata(w, httptest.NewRequest(http.MethodGet, "/@babel", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

func TestNPMVersionMetadataRejectsTraversal(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	defer upstream.Close()

	h := &NPMHandler{proxy: testProxy(), upstreamURL: upstream.URL, proxyURL: "http://proxy.local"}
	req := httptest.NewRequest(http.MethodGet, "/lodash/x", nil)
	req.URL.Path = "/lodash/.."
	w := httptest.NewRecorder()
	h.handlePackageMetadata(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
	if called {
		t.Fatal("upstream contacted for traversal path")
	}
}

func TestNPMVersionMetadataDenied(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{
			"name": "lodash",
			"version": "4.17.21",
			"dist": {"tarball": "https://registry.npmjs.org/lodash/-/lodash-4.17.21.tgz"}
		}`)
	}))
	defer upstream.Close()

	proxy := testProxy()
	setTestDenylist(t, proxy, "pkg:npm/lodash@4.17.21")
	h := &NPMHandler{proxy: proxy, upstreamURL: upstream.URL, proxyURL: "http://proxy.local"}

	w := httptest.NewRecorder()
	h.handlePackageMetadata(w, httptest.NewRequest(http.MethodGet, "/lodash/4.17.21", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body: %s", w.Code, w.Body.String())
	}
}
