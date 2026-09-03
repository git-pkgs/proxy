package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	upstreamhttp "github.com/git-pkgs/proxy/internal/httpclient"
	"github.com/git-pkgs/registries/fetch"
)

const testReleaseAssetPath = "/jqlang/jq/releases/download/jq-1.7.1/jq-linux-amd64"

func TestParseGitHubReleaseAsset(t *testing.T) {
	tests := []struct {
		path string
		want githubReleaseAsset
		ok   bool
	}{
		{
			"jqlang/jq/releases/download/jq-1.7.1/jq-linux-amd64",
			githubReleaseAsset{owner: "jqlang", repo: "jq", tag: "jq-1.7.1", filename: "jq-linux-amd64"},
			true,
		},
		{
			"cli/cli/releases/download/v2.63.2/gh_2.63.2_linux_amd64.tar.gz",
			githubReleaseAsset{owner: "cli", repo: "cli", tag: "v2.63.2", filename: "gh_2.63.2_linux_amd64.tar.gz"},
			true,
		},
		// Mutable: resolves to whatever is latest today.
		{"jqlang/jq/releases/latest/download/jq-linux-amd64", githubReleaseAsset{}, false},
		// API lookups and tag listings are not assets.
		{"repos/jqlang/jq/releases/tags/jq-1.7.1", githubReleaseAsset{}, false},
		{"jqlang/jq/releases/tag/jq-1.7.1", githubReleaseAsset{}, false},
		// Source archives are a different shape.
		{"jqlang/jq/archive/refs/tags/jq-1.7.1.tar.gz", githubReleaseAsset{}, false},
		// Extra or missing segments.
		{"jqlang/jq/releases/download/jq-1.7.1", githubReleaseAsset{}, false},
		{"jqlang/jq/releases/download/jq-1.7.1/dir/asset", githubReleaseAsset{}, false},
		{"", githubReleaseAsset{}, false},
	}

	for _, tt := range tests {
		got, ok := parseGitHubReleaseAsset(tt.path)
		if ok != tt.ok || got != tt.want {
			t.Errorf("parseGitHubReleaseAsset(%q) = (%+v, %v), want (%+v, %v)", tt.path, got, ok, tt.want, tt.ok)
		}
	}
}

func TestGenericHandler_RejectsUnknownUpstreamAndBadPaths(t *testing.T) {
	h := NewGenericHandler(testProxy(), map[string]string{"github": "https://github.com"})

	tests := []struct {
		name   string
		method string
		target string
		want   int
	}{
		{"unknown upstream", http.MethodGet, "/gitlab/owner/repo/releases/download/v1/asset", http.StatusNotFound},
		{"missing path", http.MethodGet, "/github", http.StatusNotFound},
		{"missing path with slash", http.MethodGet, "/github/", http.StatusNotFound},
		{"traversal", http.MethodGet, "/github/../etc/passwd", http.StatusBadRequest},
		{"encoded traversal", http.MethodGet, "/github/%2e%2e/etc/passwd", http.StatusBadRequest},
		{"post", http.MethodPost, "/github/owner/repo/releases/download/v1/asset", http.StatusMethodNotAllowed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.Routes().ServeHTTP(w, httptest.NewRequest(tt.method, tt.target, nil))
			if w.Code != tt.want {
				t.Errorf("status = %d, want %d", w.Code, tt.want)
			}
		})
	}
}

func TestGenericHandler_ReleaseAssetIsCachedAndServedWhenUpstreamDown(t *testing.T) {
	asset := []byte("jq binary bytes")
	var available atomic.Bool
	available.Store(true)
	var upstreamRequests atomic.Int32

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !available.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path != testReleaseAssetPath {
			http.NotFound(w, r)
			return
		}
		upstreamRequests.Add(1)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(asset)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })

	h := NewGenericHandler(proxy, map[string]string{"github": upstream.URL})

	w := serveGenericRequest(h, "/github"+testReleaseAssetPath)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != string(asset) {
		t.Errorf("body = %q, want %q", got, asset)
	}
	if got := upstreamRequests.Load(); got != 1 {
		t.Fatalf("upstream requests = %d, want 1", got)
	}

	// Second request must be served from cache, even with the upstream down.
	available.Store(false)
	w = serveGenericRequest(h, "/github"+testReleaseAssetPath)
	if w.Code != http.StatusOK {
		t.Fatalf("cached: status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != string(asset) {
		t.Errorf("cached: body = %q, want %q", got, asset)
	}
	if got := upstreamRequests.Load(); got != 1 {
		t.Errorf("upstream requests after cache hit = %d, want 1", got)
	}

	// HEAD is answered from the same cache entry without a body.
	w = httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodHead, "/github"+testReleaseAssetPath, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("HEAD: status = %d, want 200", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD: body length = %d, want 0", w.Body.Len())
	}
}

func TestGenericHandler_ReleaseAssetNotFoundIsNotCached(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })

	h := NewGenericHandler(proxy, map[string]string{"github": upstream.URL})
	w := serveGenericRequest(h, "/github"+testReleaseAssetPath)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
}

func TestGenericHandler_MetadataForwardsQueryAndServesStaleOnThrottle(t *testing.T) {
	const apiPath = "/repos/jqlang/jq/releases/tags/jq-1.7.1"
	body := `{"tag_name":"jq-1.7.1"}`
	var throttled atomic.Bool
	var gotQuery string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != apiPath {
			http.NotFound(w, r)
			return
		}
		gotQuery = r.URL.RawQuery
		if throttled.Load() {
			w.Header().Set("Retry-After", "60")
			http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.github+json")
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte(body))
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	proxy.CacheMetadata = true
	// A tiny TTL so the second request is past freshness and has to consult
	// the upstream, and the served copy is marked stale.
	proxy.MetadataTTL = time.Millisecond

	h := NewGenericHandler(proxy, map[string]string{"github-api": upstream.URL})

	req := httptest.NewRequest(http.MethodGet, "/github-api"+apiPath+"?per_page=1", nil)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != body {
		t.Errorf("body = %q, want %q", got, body)
	}
	if gotQuery != "per_page=1" {
		t.Errorf("upstream query = %q, want %q", gotQuery, "per_page=1")
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/vnd.github+json" {
		t.Errorf("Content-Type = %q, want upstream's", ct)
	}

	// The upstream now throttles us: the cached body must be served stale
	// rather than the 429 being passed through.
	throttled.Store(true)
	time.Sleep(5 * time.Millisecond)
	w = httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("throttled: status = %d, want 200 stale: %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != body {
		t.Errorf("throttled: body = %q, want cached %q", got, body)
	}
	if warning := w.Header().Get("Warning"); !strings.Contains(warning, "110") {
		t.Errorf("throttled: Warning = %q, want a 110 stale warning", warning)
	}
}

func TestGenericHandler_DistinctUpstreamsDoNotShareCache(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("from first"))
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("from second"))
	}))
	defer second.Close()

	proxy, _, _, _ := setupTestProxy(t)
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(first.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })

	h := NewGenericHandler(proxy, map[string]string{"one": first.URL, "two": second.URL})

	w := serveGenericRequest(h, "/one"+testReleaseAssetPath)
	if got := w.Body.String(); got != "from first" {
		t.Fatalf("one: body = %q, want %q", got, "from first")
	}
	w = serveGenericRequest(h, "/two"+testReleaseAssetPath)
	if got := w.Body.String(); got != "from second" {
		t.Fatalf("two: body = %q, want %q (must not reuse the first upstream's cache entry)", got, "from second")
	}
}

func TestGenericHandler_UpstreamAuthIsScopedToTheConfiguredHost(t *testing.T) {
	asset := []byte("private asset")
	var storageAuth atomic.Value
	storageAuth.Store("unset")

	// The object store the release host redirects to must never see the
	// token configured for the release host.
	objectStore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		storageAuth.Store(r.Header.Get("Authorization"))
		_, _ = w.Write(asset)
	}))
	defer objectStore.Close()

	var releaseAuth string
	releaseHost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		releaseAuth = r.Header.Get("Authorization")
		if releaseAuth != "Bearer github-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, objectStore.URL+"/signed"+r.URL.Path, http.StatusFound)
	}))
	defer releaseHost.Close()

	proxy, _, _, _ := setupTestProxy(t)
	authClient := &http.Client{Transport: upstreamhttp.NewTransport(http.DefaultTransport,
		upstreamhttp.AuthFunc(func(url string) (string, string) {
			if strings.HasPrefix(url, releaseHost.URL) {
				return "Authorization", "Bearer github-token"
			}
			return "", ""
		}))}
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(authClient), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })

	h := NewGenericHandler(proxy, map[string]string{"github": releaseHost.URL})
	w := serveGenericRequest(h, "/github"+testReleaseAssetPath)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != string(asset) {
		t.Errorf("body = %q, want %q", got, asset)
	}
	if releaseAuth != "Bearer github-token" {
		t.Errorf("release host Authorization = %q, want the configured token", releaseAuth)
	}
	if got := storageAuth.Load(); got != "" {
		t.Errorf("object store Authorization = %q, want none after the cross-host redirect", got)
	}
}

func serveGenericRequest(h *GenericHandler, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	return w
}
