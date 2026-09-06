package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/registries/fetch"
)

func TestHomebrewHandler_PreservesSignedResponseAndClientValidators(t *testing.T) {
	body := " {\n  \"payload\": \"signed bytes\",\n  \"signatures\": []\n}\n"
	etag := `"homebrew-api-etag"`
	lastModified := time.Date(2026, time.August, 14, 9, 30, 0, 0, time.UTC)
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet {
			t.Errorf("upstream method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/api/internal/packages.arm64_tahoe.jws.json" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("upstream Authorization = %q, want empty", got)
		}
		if got := r.Header.Get("Cookie"); got != "" {
			t.Errorf("upstream Cookie = %q, want empty", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", etag)
		w.Header().Set("Last-Modified", lastModified.Format(http.TimeFormat))
		_, _ = io.WriteString(w, body)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.HTTPClient = upstream.Client()
	h := NewHomebrewHandler(proxy, upstream.URL+"/api").Routes()

	req := httptest.NewRequest(http.MethodGet, "/internal/packages.arm64_tahoe.jws.json", nil)
	req.Header.Set("Authorization", "Bearer client-secret")
	req.Header.Set("Cookie", "session=client-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if got := w.Body.String(); got != body {
		t.Errorf("body = %q, want byte-for-byte %q", got, body)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	wantContentLength := strconv.Itoa(len(body))
	if got := w.Header().Get("Content-Length"); got != wantContentLength {
		t.Errorf("Content-Length = %q, want %q", got, wantContentLength)
	}
	if got := w.Header().Get("ETag"); got != etag {
		t.Errorf("ETag = %q, want %q", got, etag)
	}
	if got := w.Header().Get("Last-Modified"); got != lastModified.Format(http.TimeFormat) {
		t.Errorf("Last-Modified = %q, want %q", got, lastModified.Format(http.TimeFormat))
	}

	conditionalRequest := httptest.NewRequest(http.MethodGet, "/internal/packages.arm64_tahoe.jws.json", nil)
	conditionalRequest.Header.Set("If-None-Match", etag)
	conditional := httptest.NewRecorder()
	h.ServeHTTP(conditional, conditionalRequest)
	if conditional.Code != http.StatusNotModified {
		t.Fatalf("conditional status = %d, want %d", conditional.Code, http.StatusNotModified)
	}
	if got := conditional.Header().Get("ETag"); got != etag {
		t.Errorf("conditional ETag = %q, want %q", got, etag)
	}
	if conditional.Body.Len() != 0 {
		t.Errorf("conditional body length = %d, want 0", conditional.Body.Len())
	}

	modifiedSinceRequest := httptest.NewRequest(http.MethodGet, "/internal/packages.arm64_tahoe.jws.json", nil)
	modifiedSinceRequest.Header.Set("If-Modified-Since", lastModified.Format(http.TimeFormat))
	modifiedSince := httptest.NewRecorder()
	h.ServeHTTP(modifiedSince, modifiedSinceRequest)
	if modifiedSince.Code != http.StatusNotModified {
		t.Fatalf("If-Modified-Since status = %d, want %d", modifiedSince.Code, http.StatusNotModified)
	}
	if got := modifiedSince.Header().Get("Last-Modified"); got != lastModified.Format(http.TimeFormat) {
		t.Errorf("conditional Last-Modified = %q, want %q", got, lastModified.Format(http.TimeFormat))
	}
	if requests != 1 {
		t.Errorf("upstream requests = %d, want 1", requests)
	}
}

func TestHomebrewHandler_HeadUsesMetadataCacheAndSurvivesOutage(t *testing.T) {
	body := `{"payload":"signed bytes","signatures":[]}`
	available := true
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("upstream Authorization = %q, want empty", got)
		}
		if !available {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"head-etag"`)
		_, _ = io.WriteString(w, body)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.HTTPClient = upstream.Client()
	h := NewHomebrewHandler(proxy, upstream.URL+"/api").Routes()

	coldRequest := httptest.NewRequest(http.MethodHead, "/formula.jws.json", nil)
	coldRequest.Header.Set("Authorization", "Bearer client-secret")
	cold := httptest.NewRecorder()
	h.ServeHTTP(cold, coldRequest)
	if cold.Code != http.StatusOK {
		t.Fatalf("cold status = %d, want %d", cold.Code, http.StatusOK)
	}
	if cold.Body.Len() != 0 {
		t.Errorf("cold body length = %d, want 0", cold.Body.Len())
	}
	if got := cold.Header().Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Errorf("Content-Length = %q, want %d", got, len(body))
	}
	if got := cold.Header().Get("ETag"); got != `"head-etag"` {
		t.Errorf("ETag = %q, want %q", got, `"head-etag"`)
	}
	if requests != 1 {
		t.Fatalf("cold upstream requests = %d, want 1", requests)
	}

	warm := httptest.NewRecorder()
	h.ServeHTTP(warm, httptest.NewRequest(http.MethodHead, "/formula.jws.json", nil))
	if warm.Code != http.StatusOK {
		t.Fatalf("warm status = %d, want %d", warm.Code, http.StatusOK)
	}
	if warm.Body.Len() != 0 {
		t.Errorf("warm body length = %d, want 0", warm.Body.Len())
	}
	if requests != 1 {
		t.Errorf("warm upstream requests = %d, want 1", requests)
	}

	proxy.MetadataTTL = time.Nanosecond
	available = false
	stale := httptest.NewRecorder()
	h.ServeHTTP(stale, httptest.NewRequest(http.MethodHead, "/formula.jws.json", nil))
	if stale.Code != http.StatusOK {
		t.Fatalf("stale status = %d, want %d; body: %s", stale.Code, http.StatusOK, stale.Body.String())
	}
	if stale.Body.Len() != 0 {
		t.Errorf("stale body length = %d, want 0", stale.Body.Len())
	}
	if got := stale.Header().Get("Warning"); got != containerStaleWarning {
		t.Errorf("Warning = %q, want %q", got, containerStaleWarning)
	}
}

func TestHomebrewHandler_HeadWithoutMetadataCachePreservesUpstreamMethod(t *testing.T) {
	upstreamMethod := ""
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamMethod = r.Method
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "42")
		w.Header().Set("ETag", `"head-etag"`)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = false
	proxy.HTTPClient = upstream.Client()
	h := NewHomebrewHandler(proxy, upstream.URL+"/api").Routes()

	head := httptest.NewRecorder()
	h.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/formula.jws.json", nil))
	if head.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", head.Code, http.StatusOK)
	}
	if upstreamMethod != http.MethodHead {
		t.Errorf("upstream method = %q, want HEAD", upstreamMethod)
	}
	if head.Body.Len() != 0 {
		t.Errorf("body length = %d, want 0", head.Body.Len())
	}
	if got := head.Header().Get("Content-Length"); got != "42" {
		t.Errorf("Content-Length = %q, want 42", got)
	}
}

func TestHomebrewHandler_ServesStaleCachedResponseWhenUpstreamFails(t *testing.T) {
	body := `{"payload":"signed bytes","signatures":[]}`
	available := true
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if !available {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"stale-etag"`)
		_, _ = io.WriteString(w, body)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = 5 * time.Millisecond
	proxy.HTTPClient = upstream.Client()
	h := NewHomebrewHandler(proxy, upstream.URL+"/api").Routes()

	first := httptest.NewRecorder()
	h.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/formula.jws.json", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("warm status = %d, want %d", first.Code, http.StatusOK)
	}

	time.Sleep(10 * time.Millisecond)
	available = false
	stale := httptest.NewRecorder()
	h.ServeHTTP(stale, httptest.NewRequest(http.MethodGet, "/formula.jws.json", nil))
	if stale.Code != http.StatusOK {
		t.Fatalf("stale status = %d, want %d; body: %s", stale.Code, http.StatusOK, stale.Body.String())
	}
	if got := stale.Body.String(); got != body {
		t.Errorf("stale body = %q, want %q", got, body)
	}
	if got := stale.Header().Get("Warning"); got != containerStaleWarning {
		t.Errorf("Warning = %q, want %q", got, containerStaleWarning)
	}
	if requests != 2 {
		t.Errorf("upstream requests = %d, want 2", requests)
	}
}

func TestHomebrewHandler_ProxiesSupportedAPIPaths(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.URL.RequestURI())
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	h := NewHomebrewHandler(proxy, upstream.URL+"/api").Routes()

	paths := []string{
		"/formula.jws.json",
		"/cask.jws.json",
		"/formula/jq.json",
		"/cask/firefox.json",
		"/internal/packages.arm64_tahoe.jws.json?download=1",
	}
	for _, requestPath := range paths {
		t.Run(requestPath, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, requestPath, nil))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
			}
			if got, want := w.Body.String(), "/api"+requestPath; got != want {
				t.Errorf("upstream request = %q, want %q", got, want)
			}
		})
	}
}

func TestHomebrewHandler_RejectsUnsupportedRequests(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	h := NewHomebrewHandler(proxy, "https://example.test/api").Routes()

	method := httptest.NewRecorder()
	h.ServeHTTP(method, httptest.NewRequest(http.MethodPost, "/formula.jws.json", nil))
	if method.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want %d", method.Code, http.StatusMethodNotAllowed)
	}
	if got := method.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q, want GET, HEAD", got)
	}

	root := httptest.NewRecorder()
	h.ServeHTTP(root, httptest.NewRequest(http.MethodGet, "/", nil))
	if root.Code != http.StatusNotFound {
		t.Errorf("root status = %d, want %d", root.Code, http.StatusNotFound)
	}

	traversal := httptest.NewRecorder()
	h.ServeHTTP(traversal, httptest.NewRequest(http.MethodGet, "/%2e%2e/secret", nil))
	if traversal.Code != http.StatusNotFound {
		t.Errorf("traversal status = %d, want %d", traversal.Code, http.StatusNotFound)
	}
}

func TestRegisterHomebrewArtifacts(t *testing.T) {
	h := &ContainerHandler{registryURL: dockerHubRegistry}
	artifactUpstream := "https://homebrew-proxy.example.com"
	RegisterHomebrewArtifacts(h, artifactUpstream+"/")

	if got := h.registryURLFor("homebrew/core/jq"); got != artifactUpstream {
		t.Errorf("homebrew/core registry = %q, want %q", got, artifactUpstream)
	}
	if got := h.registryURLFor("homebrew/cask/firefox"); got != "" {
		t.Errorf("other Homebrew registry = %q, want blocked", got)
	}
	if got := h.registryURLFor("library/nginx"); got != dockerHubRegistry {
		t.Errorf("unrelated registry = %q, want %q", got, dockerHubRegistry)
	}
}

func TestRegisterHomebrewArtifactsRejectsOtherHomebrewRoutes(t *testing.T) {
	upstreamRequests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamRequests++
		_, _ = io.WriteString(w, "unexpected upstream response")
	}))
	defer upstream.Close()

	proxy, _, _, fetcher := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("unexpected upstream blob")),
		ContentType: "application/octet-stream",
	}
	h := &ContainerHandler{proxy: proxy, registryURL: upstream.URL}
	RegisterHomebrewArtifacts(h, "https://ghcr.io")

	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	paths := []string{
		"/homebrew/cask/firefox/blobs/" + digest,
		"/homebrew/cask/firefox/manifests/latest",
		"/homebrew/cask/firefox/tags/list",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			if w.Code != http.StatusNotFound {
				t.Errorf("status = %d, want %d; body: %s", w.Code, http.StatusNotFound, w.Body.String())
			}
		})
	}

	if fetcher.fetchCalled {
		t.Error("blocked Homebrew blob reached the artifact fetcher")
	}
	if upstreamRequests != 0 {
		t.Errorf("blocked Homebrew routes made %d upstream requests, want 0", upstreamRequests)
	}
}
