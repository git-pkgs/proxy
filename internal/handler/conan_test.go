package handler

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func conanTestProxy() *Proxy {
	return &Proxy{
		Logger: slog.Default(),
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
		proxyURL: "http://localhost:8080",
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
		proxyURL: "http://localhost:8080",
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

	req := httptest.NewRequest(http.MethodGet, "/v2/files/zlib/1.2.13/_/_/abc123/recipe/conanfile.py", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/v2/files/zlib/1.2.13/_/_/abc123/package/pkgref1/pkgrev1/conaninfo.txt", nil)
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
	if h.proxyURL != "http://localhost:8080" {
		t.Errorf("proxyURL = %q, want %q (trailing slash should be trimmed)", h.proxyURL, "http://localhost:8080")
	}
}

func TestNewConanHandlerNoTrailingSlash(t *testing.T) {
	proxy := conanTestProxy()
	h := NewConanHandler(proxy, "http://localhost:8080")

	if h.proxyURL != "http://localhost:8080" {
		t.Errorf("proxyURL = %q, want %q", h.proxyURL, "http://localhost:8080")
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
