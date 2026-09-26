package handler

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
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

func TestNPMRewriteMetadataGitHubPackagesTarball(t *testing.T) {
	h := &NPMHandler{
		proxy:    testProxy(),
		proxyURL: "http://localhost:8080",
	}

	input := `{
		"name": "@example/private-package",
		"versions": {
			"1.0.0": {
				"dist": {
					"shasum": "e053d091c6ae91793f6333f5fe0a55633cf3c584",
					"tarball": "https://npm.pkg.github.com/download/@example/private-package/1.0.0/e053d091c6ae91793f6333f5fe0a55633cf3c584"
				}
			}
		}
	}`

	output, err := h.rewriteMetadata("@example/private-package", []byte(input))
	if err != nil {
		t.Fatalf("rewriteMetadata failed: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	versions := result["versions"].(map[string]any)
	v := versions[testVersion100].(map[string]any)
	dist := v["dist"].(map[string]any)
	tarball := dist["tarball"].(string)

	expected := "http://localhost:8080/npm/@example%2Fprivate-package/-/private-package-1.0.0.tgz"
	if tarball != expected {
		t.Errorf("tarball = %q, want %q", tarball, expected)
	}
}

func TestNPMHandlerDownloadsGitHubPackagesTarball(t *testing.T) {
	const shasum = "e053d091c6ae91793f6333f5fe0a55633cf3c584"
	const tarballPath = "/download/@example/private-package/1.0.0/" + shasum

	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/@example/private-package" {
			t.Errorf("metadata path = %q, want scoped package path", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", contentTypeJSON)
		_, _ = io.WriteString(w, `{"versions":{"1.0.0":{"dist":{"tarball":"`+upstream.URL+tarballPath+`"}}}}`)
	}))
	defer upstream.Close()

	proxy, _, _, artifactFetcher := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	artifactFetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("package")),
		ContentType: "application/gzip",
	}
	h := NewNPMHandler(proxy, "http://proxy.test", upstream.URL)

	req := httptest.NewRequest(
		http.MethodGet,
		"/@example/private-package/-/private-package-1.0.0.tgz",
		nil,
	)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if artifactFetcher.fetchedURL != upstream.URL+tarballPath {
		t.Errorf("fetched URL = %q, want %q", artifactFetcher.fetchedURL, upstream.URL+tarballPath)
	}
}

func TestNPMHandlerRejectsMissingMetadataVersion(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentTypeJSON)
		_, _ = io.WriteString(w, `{"versions":{"2.0.0":{}}}`)
	}))
	defer upstream.Close()

	proxy, _, _, artifactFetcher := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	h := NewNPMHandler(proxy, "http://proxy.test", upstream.URL)

	req := httptest.NewRequest(
		http.MethodGet,
		"/pkg/-/pkg-1.0.0.tgz",
		nil,
	)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if artifactFetcher.fetchedURL != "" {
		t.Errorf("artifact fetcher should not be called, fetched URL = %q", artifactFetcher.fetchedURL)
	}
}

func TestNPMHandlerRejectsTarballFromDifferentHost(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentTypeJSON)
		_, _ = io.WriteString(w, `{"versions":{"1.0.0":{"dist":{"tarball":"https://example.invalid/package.tgz"}}}}`)
	}))
	defer upstream.Close()

	proxy, _, _, artifactFetcher := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	h := NewNPMHandler(proxy, "http://proxy.test", upstream.URL)

	req := httptest.NewRequest(
		http.MethodGet,
		"/pkg/-/pkg-1.0.0.tgz",
		nil,
	)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if artifactFetcher.fetchedURL != "" {
		t.Errorf("artifact fetcher should not be called, fetched URL = %q", artifactFetcher.fetchedURL)
	}
}

func TestNPMHandlerRejectsTarballOutsideUpstreamBasePath(t *testing.T) {
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/root/pkg" {
			t.Errorf("metadata path = %q, want %q", r.URL.Path, "/root/pkg")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", contentTypeJSON)
		_, _ = io.WriteString(w, `{"versions":{"1.0.0":{"dist":{"tarball":"`+upstream.URL+`/outside/pkg-1.0.0.tgz"}}}}`)
	}))
	defer upstream.Close()

	proxy, _, _, artifactFetcher := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	h := NewNPMHandler(proxy, "http://proxy.test", upstream.URL+"/root/")

	req := httptest.NewRequest(http.MethodGet, "/pkg/-/pkg-1.0.0.tgz", nil)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if artifactFetcher.fetchedURL != "" {
		t.Errorf("artifact fetcher should not be called, fetched URL = %q", artifactFetcher.fetchedURL)
	}
}

func TestNPMHandlerFallsBackWhenMetadataIsUnavailable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	proxy, _, _, artifactFetcher := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	artifactFetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("package")),
		ContentType: "application/gzip",
	}
	h := NewNPMHandler(proxy, "http://proxy.test", upstream.URL)

	req := httptest.NewRequest(http.MethodGet, "/pkg/-/pkg-1.0.0.tgz", nil)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	want := upstream.URL + "/pkg/-/pkg-1.0.0.tgz"
	if artifactFetcher.fetchedURL != want {
		t.Errorf("fetched URL = %q, want %q", artifactFetcher.fetchedURL, want)
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
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentTypeJSON)
		_, _ = io.WriteString(w, `{
			"name": "leftpad",
			"dist-tags": {"latest": "2.0.0"},
			"time": {
				"1.0.0": "`+now.Add(-30*24*time.Hour).Format(time.RFC3339)+`",
				"2.0.0": "`+now.Add(-1*time.Hour).Format(time.RFC3339)+`"
			},
			"versions": {
				"1.0.0": {"dist": {"tarball": "`+upstream.URL+`/leftpad/-/leftpad-1.0.0.tgz"}},
				"2.0.0": {"dist": {"tarball": "`+upstream.URL+`/leftpad/-/leftpad-2.0.0.tgz"}}
			}
		}`)
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
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("metadata must not be fetched when cooldown is disabled")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	proxy, _, _, fetcher := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("tarball data")),
		ContentType: "application/octet-stream",
	}

	h := NewNPMHandler(proxy, "http://proxy.test", upstream.URL)

	if h.versionInCooldown(httptest.NewRequest(http.MethodGet, "/", nil), "leftpad", testVersion100) {
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
			proxy, db, _, fetcher := setupTestProxy(t)
			proxy.HTTPClient = upstream.Client()
			proxy.Cooldown = &cooldown.Config{Default: "7d"}
			fetcher.artifact = &fetch.Artifact{
				Body:        io.NopCloser(strings.NewReader("tarball data")),
				ContentType: "application/octet-stream",
			}

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
