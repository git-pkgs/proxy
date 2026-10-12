package handler

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testProxyURL = "http://localhost:8080"

func conanTestProxy() *Proxy {
	return &Proxy{
		Logger:     slog.Default(),
		HTTPClient: http.DefaultClient,
	}
}

func TestConanShouldCacheFile(t *testing.T) {
	h := &ConanHandler{}

	tests := []struct {
		filename string
		want     bool
	}{
		{"conan_sources.tgz", true},
		{"conan_export.tgz", true},
		{"conan_package.tgz", true},
		{"conan_sources.txz", true},
		{"conan_export.txz", true},
		{"conan_package.txz", true},
		{"conan_sources.tzst", true},
		{"conan_export.tzst", true},
		{"conan_package.tzst", true},
		{"conan_package.tar.gz", false},
		{"conanfile.py", false},
		{"conanmanifest.txt", false},
		{"conaninfo.txt", false},
		{"random.tgz", false},
		{"", false},
	}

	for _, tt := range tests {
		got := h.shouldCacheFile(tt.filename)
		if got != tt.want {
			t.Errorf("shouldCacheFile(%q) = %v, want %v", tt.filename, got, tt.want)
		}
	}
}

func TestConanPingV1(t *testing.T) {
	h := &ConanHandler{
		proxy:    conanTestProxy(),
		proxyURL: testProxyURL,
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/ping", nil)
	w := httptest.NewRecorder()

	h.handlePing(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	caps := w.Header().Get("X-Conan-Server-Capabilities")
	if caps != "revisions" {
		t.Errorf("X-Conan-Server-Capabilities = %q, want %q", caps, "revisions")
	}
}

func TestConanPingV2(t *testing.T) {
	h := &ConanHandler{
		proxy:    conanTestProxy(),
		proxyURL: testProxyURL,
	}

	req := httptest.NewRequest(http.MethodGet, "/v2/ping", nil)
	w := httptest.NewRecorder()

	h.handlePing(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	caps := w.Header().Get("X-Conan-Server-Capabilities")
	if caps != "revisions" {
		t.Errorf("X-Conan-Server-Capabilities = %q, want %q", caps, "revisions")
	}
}

func TestConanProxyUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/conans/search" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("q") != "zlib" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"results":["zlib/1.2.13"]}`))
	}))
	defer upstream.Close()

	h := &ConanHandler{
		proxy:       conanTestProxy(),
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/v2/conans/search?q=zlib", nil)
	w := httptest.NewRecorder()
	h.proxyUpstream(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	body := w.Body.String()
	if !strings.Contains(body, "zlib/1.2.13") {
		t.Errorf("response body does not contain expected result: %s", body)
	}
}

func TestConanProxyUpstreamNotFound(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	h := &ConanHandler{
		proxy:       conanTestProxy(),
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/v2/conans/nonexistent", nil)
	w := httptest.NewRecorder()
	h.proxyUpstream(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestConanProxyUpstreamCopiesHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom-Header", "test-value")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	h := &ConanHandler{
		proxy:       conanTestProxy(),
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/v2/conans/test", nil)
	w := httptest.NewRecorder()
	h.proxyUpstream(w, req)

	if w.Header().Get("X-Custom-Header") != "test-value" {
		t.Errorf("X-Custom-Header = %q, want %q", w.Header().Get("X-Custom-Header"), "test-value")
	}
}

func TestConanProxyUpstreamForwardsAuthHeader(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer mytoken" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	h := &ConanHandler{
		proxy:       conanTestProxy(),
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/v2/conans/test", nil)
	req.Header.Set("Authorization", "Bearer mytoken")
	w := httptest.NewRecorder()
	h.proxyUpstream(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestConanProxyUpstreamBadUpstream(t *testing.T) {
	h := &ConanHandler{
		proxy:       conanTestProxy(),
		upstreamURL: "http://127.0.0.1:1", // unreachable
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/v2/conans/test", nil)
	w := httptest.NewRecorder()
	h.proxyUpstream(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadGateway)
	}
}

func TestConanRecipeFileNonCacheable(t *testing.T) {
	// When a recipe file is not cacheable (e.g. conanfile.py), it should be proxied upstream.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("conanfile content"))
	}))
	defer upstream.Close()

	h := &ConanHandler{
		proxy:       conanTestProxy(),
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/v2/conans/zlib/1.2.13/_/_/revisions/abc123/files/conanfile.py", nil)
	req.SetPathValue("name", "zlib")
	req.SetPathValue("version", "1.2.13")
	req.SetPathValue("user", "_")
	req.SetPathValue("channel", "_")
	req.SetPathValue("revision", "abc123")
	req.SetPathValue("filename", "conanfile.py")

	w := httptest.NewRecorder()
	h.handleRecipeFile(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	body := w.Body.String()
	if body != "conanfile content" {
		t.Errorf("body = %q, want %q", body, "conanfile content")
	}
}

func TestConanPackageFileNonCacheable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("conaninfo content"))
	}))
	defer upstream.Close()

	h := &ConanHandler{
		proxy:       conanTestProxy(),
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/v2/conans/zlib/1.2.13/_/_/revisions/abc123/packages/pkgref1/revisions/pkgrev1/files/conaninfo.txt", nil)
	req.SetPathValue("name", "zlib")
	req.SetPathValue("version", "1.2.13")
	req.SetPathValue("user", "_")
	req.SetPathValue("channel", "_")
	req.SetPathValue("revision", "abc123")
	req.SetPathValue("pkgref", "pkgref1")
	req.SetPathValue("pkgrev", "pkgrev1")
	req.SetPathValue("filename", "conaninfo.txt")

	w := httptest.NewRecorder()
	h.handlePackageFile(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	body := w.Body.String()
	if body != "conaninfo content" {
		t.Errorf("body = %q, want %q", body, "conaninfo content")
	}
}

// The Conan 2 client downloads recipe and package files over these routes;
// the archives must be stored as artifacts and served from the cache after.
func TestConanV2FileRoutesCacheArchives(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		filename string
	}{
		{"recipe", "/v2/conans/zlib/1.3.1/_/_/revisions/rrev1/files/conan_sources.tgz",
			"rrev1_conan_sources.tgz"},
		{"package", "/v2/conans/zlib/1.3.1/_/_/revisions/rrev1/packages/pkgid1/revisions/prev1/files/conan_package.tgz",
			"rrev1_pkgid1_prev1_conan_package.tgz"},
		{"recipe xz", "/v2/conans/zlib/1.3.1/_/_/revisions/rrev1/files/conan_export.txz",
			"rrev1_conan_export.txz"},
		{"package zstd", "/v2/conans/zlib/1.3.1/_/_/revisions/rrev1/packages/pkgid1/revisions/prev1/files/conan_package.tzst",
			"rrev1_pkgid1_prev1_conan_package.tzst"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxy, db, _, fetcher := setupTestProxy(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("archive %s was proxied instead of fetched as an artifact", r.URL.Path)
				http.Error(w, "unexpected", http.StatusInternalServerError)
			}))
			defer upstream.Close()
			proxy.HTTPClient = upstream.Client()

			h := NewConanHandlerWithUpstream(proxy, testProxyURL, upstream.URL)
			srv := httptest.NewServer(h.Routes())
			defer srv.Close()

			for i := range 2 {
				fetcher.artifact = artifactBody("archive bytes")
				fetcher.fetchCalled = false

				resp, err := http.Get(srv.URL + tt.path)
				if err != nil {
					t.Fatalf("request %d failed: %v", i+1, err)
				}
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()

				if resp.StatusCode != http.StatusOK || string(body) != "archive bytes" {
					t.Fatalf("request %d: status = %d, body = %q, want 200 %q", i+1, resp.StatusCode, body, "archive bytes")
				}
				if fetcher.fetchCalled != (i == 0) {
					t.Errorf("request %d: fetched = %v, want %v", i+1, fetcher.fetchCalled, i == 0)
				}
				if i == 0 && fetcher.fetchedURL != upstream.URL+tt.path {
					t.Errorf("upstream URL = %q, want %q", fetcher.fetchedURL, upstream.URL+tt.path)
				}
			}

			recordedStoragePath(t, db, "pkg:conan/zlib@1.3.1", tt.filename)
		})
	}
}

// References that differ only in user and channel share pkg:conan/zlib@1.3.1
// but must not share files, even with the same recipe revision.
func TestConanUserChannelKeptApart(t *testing.T) {
	proxy, db, _, fetcher := setupTestProxy(t)
	h := NewConanHandlerWithUpstream(proxy, testProxyURL, "http://upstream.invalid")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	for _, ref := range []struct{ path, body, filename string }{
		{"/v2/conans/zlib/1.3.1/_/_/revisions/rrev1/files/conan_sources.tgz",
			"center sources", "rrev1_conan_sources.tgz"},
		{"/v2/conans/zlib/1.3.1/acme/stable/revisions/rrev1/files/conan_sources.tgz",
			"acme sources", "acme@stable_rrev1_conan_sources.tgz"},
	} {
		fetcher.artifact = artifactBody(ref.body)
		fetcher.fetchCalled = false

		resp, err := http.Get(srv.URL + ref.path)
		if err != nil {
			t.Fatalf("GET %s failed: %v", ref.path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if string(body) != ref.body || !fetcher.fetchCalled {
			t.Errorf("GET %s: body = %q, fetched = %v, want %q from upstream", ref.path, body, fetcher.fetchCalled, ref.body)
		}
		recordedStoragePath(t, db, "pkg:conan/zlib@1.3.1", ref.filename)
	}
}

// A denylist entry for a Conan version blocks its recipe and binary archives.
func TestConanDenylistMatchesVersion(t *testing.T) {
	proxy, _, _, fetcher := setupTestProxy(t)
	setTestDenylist(t, proxy, "pkg:conan/zlib@1.3.1")
	h := NewConanHandlerWithUpstream(proxy, testProxyURL, "http://upstream.invalid")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	for _, path := range []string{
		"/v2/conans/zlib/1.3.1/_/_/revisions/rrev1/files/conan_sources.tgz",
		"/v2/conans/zlib/1.3.1/acme/stable/revisions/rrev1/packages/pkgid1/revisions/prev1/files/conan_package.tgz",
	} {
		fetcher.artifact = artifactBody("archive bytes")
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s failed: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("GET %s: status = %d, want %d", path, resp.StatusCode, http.StatusForbidden)
		}
	}
	if fetcher.fetchCalled {
		t.Error("denied archive was fetched from upstream")
	}
}

// The small files next to the archives keep going through the metadata cache.
func TestConanV2FileRoutesKeepSmallFilesAsMetadata(t *testing.T) {
	for name, path := range map[string]string{
		"recipe":  "/v2/conans/zlib/1.3.1/_/_/revisions/rrev1/files/conanmanifest.txt",
		"package": "/v2/conans/zlib/1.3.1/_/_/revisions/rrev1/packages/pkgid1/revisions/prev1/files/conaninfo.txt",
	} {
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path != path {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte("small file"))
			}))
			defer upstream.Close()

			proxy, _, _, fetcher := setupTestProxy(t)
			proxy.CacheMetadata = true
			proxy.MetadataTTL = time.Hour
			proxy.HTTPClient = upstream.Client()

			h := NewConanHandlerWithUpstream(proxy, testProxyURL, upstream.URL)
			srv := httptest.NewServer(h.Routes())
			defer srv.Close()

			for i := range 2 {
				resp, err := http.Get(srv.URL + path)
				if err != nil {
					t.Fatalf("request %d failed: %v", i+1, err)
				}
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK || string(body) != "small file" {
					t.Fatalf("request %d: status = %d, body = %q, want 200 %q", i+1, resp.StatusCode, body, "small file")
				}
			}

			if got := requests.Load(); got != 1 {
				t.Errorf("upstream requests = %d, want 1 (second served from the metadata cache)", got)
			}
			if fetcher.fetchCalled {
				t.Error("small file was fetched as an artifact")
			}
		})
	}
}

func TestConanRoutes(t *testing.T) {
	h := &ConanHandler{
		proxy:       conanTestProxy(),
		upstreamURL: "http://localhost:1", // won't be called for ping
		proxyURL:    "http://proxy.local",
	}

	routes := h.Routes()

	tests := []struct {
		path       string
		wantStatus int
	}{
		{"/v1/ping", http.StatusOK},
		{"/v2/ping", http.StatusOK},
	}

	for _, tt := range tests {
		req := httptest.NewRequest(http.MethodGet, tt.path, nil)
		w := httptest.NewRecorder()
		routes.ServeHTTP(w, req)

		if w.Code != tt.wantStatus {
			t.Errorf("GET %s: status = %d, want %d", tt.path, w.Code, tt.wantStatus)
		}
	}
}

func TestConanProxyUpstreamPreservesQueryString(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "boost" && r.URL.Query().Get("page") != "2" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer upstream.Close()

	h := &ConanHandler{
		proxy:       conanTestProxy(),
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/v2/conans/search?q=boost&page=2", nil)
	w := httptest.NewRecorder()
	h.proxyUpstream(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestConanProxyUpstreamLargeResponse(t *testing.T) {
	largeBody := strings.Repeat("x", 1024*1024)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(largeBody))
	}))
	defer upstream.Close()

	h := &ConanHandler{
		proxy:       conanTestProxy(),
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/v2/conans/test", nil)
	w := httptest.NewRecorder()
	h.proxyUpstream(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	if w.Body.Len() != len(largeBody) {
		t.Errorf("body length = %d, want %d", w.Body.Len(), len(largeBody))
	}
}

func TestNewConanHandler(t *testing.T) {
	proxy := conanTestProxy()
	h := NewConanHandler(proxy, "http://localhost:8080/")

	if h.proxy != proxy {
		t.Error("proxy not set correctly")
	}
	if h.upstreamURL != conanUpstream {
		t.Errorf("upstreamURL = %q, want %q", h.upstreamURL, conanUpstream)
	}
	if h.proxyURL != testProxyURL {
		t.Errorf("proxyURL = %q, want %q (trailing slash should be trimmed)", h.proxyURL, testProxyURL)
	}
}

func TestNewConanHandlerNoTrailingSlash(t *testing.T) {
	proxy := conanTestProxy()
	h := NewConanHandler(proxy, testProxyURL)

	if h.proxyURL != testProxyURL {
		t.Errorf("proxyURL = %q, want %q", h.proxyURL, testProxyURL)
	}
}

func TestConanProxyUpstreamNoAuthHeaderWhenNotProvided(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("unexpected auth header"))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	h := &ConanHandler{
		proxy:       conanTestProxy(),
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/v2/conans/test", nil)
	w := httptest.NewRecorder()
	h.proxyUpstream(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestConanProxyUpstreamCopiesBody(t *testing.T) {
	expected := `{"name":"zlib","version":"1.2.13","user":"_","channel":"_"}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(expected))
	}))
	defer upstream.Close()

	h := &ConanHandler{
		proxy:       conanTestProxy(),
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/v2/conans/zlib/1.2.13/_/_/latest", nil)
	w := httptest.NewRecorder()
	h.proxyUpstream(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	got, _ := io.ReadAll(w.Body)
	if string(got) != expected {
		t.Errorf("body = %q, want %q", string(got), expected)
	}
}

func TestConanProxyUpstreamPreservesStatusCodes(t *testing.T) {
	codes := []int{
		http.StatusOK,
		http.StatusNotFound,
		http.StatusForbidden,
		http.StatusInternalServerError,
	}

	for _, code := range codes {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))

		h := &ConanHandler{
			proxy:       conanTestProxy(),
			upstreamURL: upstream.URL,
			proxyURL:    "http://proxy.local",
		}

		req := httptest.NewRequest(http.MethodGet, "/v2/test", nil)
		w := httptest.NewRecorder()
		h.proxyUpstream(w, req)

		if w.Code != code {
			t.Errorf("status = %d, want %d", w.Code, code)
		}

		upstream.Close()
	}
}
