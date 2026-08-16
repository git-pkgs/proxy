package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	upstreamhttp "github.com/git-pkgs/proxy/internal/httpclient"
	"github.com/git-pkgs/proxy/internal/storage"
	"github.com/git-pkgs/registries/fetch"
	"gopkg.in/yaml.v3"
)

func TestHelmHandler_RewritesIndexAndCachesChart(t *testing.T) {
	chart := []byte("a Helm chart")
	digest := helmSHA256Hex(chart)
	var available atomic.Bool
	available.Store(true)
	var indexRequests atomic.Int32
	var chartRequests atomic.Int32

	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !available.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case "/charts/index.yaml":
			indexRequests.Add(1)
			w.Header().Set("Content-Type", "application/x-yaml")
			_, _ = fmt.Fprintf(w, `apiVersion: v1
entries:
  demo:
    - annotations:
        example.com/retained: "true"
      created: 2020-01-02T03:04:05Z
      digest: %s
      name: demo
      urls:
        - demo-1.0.0.tgz
        - %s/charts/mirror/demo-1.0.0.tgz
      version: 1.0.0
generated: 2020-01-02T03:04:05Z
`, digest, upstream.URL)
		case "/charts/demo-1.0.0.tgz", "/charts/mirror/demo-1.0.0.tgz":
			chartRequests.Add(1)
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(chart)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.HTTPClient = upstream.Client()
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })

	h := NewHelmHandler(proxy, "http://proxy.example", map[string]string{"stable": upstream.URL + "/charts"})

	indexResponse := serveHelmRequest(h, "/stable/index.yaml")
	if indexResponse.Code != http.StatusOK {
		t.Fatalf("index status = %d, want 200: %s", indexResponse.Code, indexResponse.Body.String())
	}
	if got := indexResponse.Header().Get("Content-Type"); got != "application/x-yaml" {
		t.Errorf("index Content-Type = %q, want application/x-yaml", got)
	}
	if strings.Contains(indexResponse.Body.String(), upstream.URL) {
		t.Errorf("rewritten index contains upstream URL: %s", indexResponse.Body.String())
	}
	if !strings.Contains(indexResponse.Body.String(), "example.com/retained") {
		t.Errorf("rewritten index lost an unrelated field: %s", indexResponse.Body.String())
	}

	var index map[string]any
	if err := yaml.Unmarshal(indexResponse.Body.Bytes(), &index); err != nil {
		t.Fatalf("parse rewritten index: %v", err)
	}
	entries := index["entries"].(map[string]any)
	release := entries["demo"].([]any)[0].(map[string]any)
	urls := release["urls"].([]any)
	wantURL := "http://proxy.example/helm/stable/charts/" + digest + "/demo-1.0.0.tgz"
	for _, rawURL := range urls {
		if rawURL != wantURL {
			t.Errorf("rewritten URL = %q, want %q", rawURL, wantURL)
		}
	}

	firstChart := serveHelmRequest(h, "/stable/charts/"+digest+"/demo-1.0.0.tgz")
	if firstChart.Code != http.StatusOK {
		t.Fatalf("chart status = %d, want 200: %s", firstChart.Code, firstChart.Body.String())
	}
	if got := firstChart.Body.String(); got != string(chart) {
		t.Errorf("chart body = %q, want %q", got, chart)
	}
	if got := firstChart.Header().Get("Content-Type"); got != "application/gzip" {
		t.Errorf("chart Content-Type = %q, want application/gzip", got)
	}

	available.Store(false)
	cachedChart := serveHelmRequest(h, "/stable/charts/"+digest+"/demo-1.0.0.tgz")
	if cachedChart.Code != http.StatusOK {
		t.Fatalf("cached chart status = %d, want 200: %s", cachedChart.Code, cachedChart.Body.String())
	}
	if got := cachedChart.Body.String(); got != string(chart) {
		t.Errorf("cached chart body = %q, want %q", got, chart)
	}
	if got := indexRequests.Load(); got != 1 {
		t.Errorf("index requests = %d, want 1", got)
	}
	if got := chartRequests.Load(); got != 1 {
		t.Errorf("chart requests = %d, want 1", got)
	}
}

func TestHelmHandler_RejectsChartDigestMismatch(t *testing.T) {
	chart := []byte("tampered chart")
	digest := helmSHA256Hex([]byte("expected chart"))
	requests := 0

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.yaml":
			_, _ = fmt.Fprintf(w, "apiVersion: v1\nentries:\n  demo:\n    - digest: %s\n      urls: [demo.tgz]\n", digest)
		case "/demo.tgz":
			requests++
			_, _ = w.Write(chart)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	proxy, _, store, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.HTTPClient = upstream.Client()
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	h := NewHelmHandler(proxy, "http://proxy.example", map[string]string{"test": upstream.URL})

	for range 2 {
		response := serveHelmRequest(h, "/test/charts/"+digest+"/demo.tgz")
		if response.Code != http.StatusBadGateway {
			t.Errorf("status = %d, want 502: %s", response.Code, response.Body.String())
		}
	}
	if requests != 2 {
		t.Errorf("chart requests = %d, want 2 after invalid cache entry is cleared", requests)
	}
	storagePath := storage.ArtifactPath(helmMetadataEcosystem, "", "test", digest, "demo.tgz")
	if exists, err := store.Exists(t.Context(), storagePath); err != nil {
		t.Fatalf("checking rejected chart storage: %v", err)
	} else if exists {
		t.Errorf("rejected chart remains in storage at %q", storagePath)
	}
}

func TestHelmHandler_IndexCacheChangesWithUpstreamURL(t *testing.T) {
	firstDigest := strings.Repeat("a", sha256HexLength)
	secondDigest := strings.Repeat("b", sha256HexLength)
	firstRequests := 0
	secondRequests := 0
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstRequests++
		_, _ = fmt.Fprintf(w, "apiVersion: v1\nentries:\n  demo:\n    - digest: %s\n      urls: [demo.tgz]\n", firstDigest)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondRequests++
		_, _ = fmt.Fprintf(w, "apiVersion: v1\nentries:\n  demo:\n    - digest: %s\n      urls: [demo.tgz]\n", secondDigest)
	}))
	defer second.Close()

	proxy, db, store, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.HTTPClient = first.Client()
	firstHandler := NewHelmHandler(proxy, "http://proxy.example", map[string]string{"stable": first.URL})
	if response := serveHelmRequest(firstHandler, "/stable/index.yaml"); response.Code != http.StatusOK {
		t.Fatalf("first index status = %d, want 200: %s", response.Code, response.Body.String())
	}

	// Model a restarted server with the same database and storage but a changed
	// repository URL. Its cache key must not reuse the previous index or ETag.
	restartedProxy := NewProxy(db, store, &mockFetcher{}, fetch.NewResolver(), nil)
	restartedProxy.CacheMetadata = true
	restartedProxy.MetadataTTL = time.Hour
	restartedProxy.HTTPClient = second.Client()
	secondHandler := NewHelmHandler(restartedProxy, "http://proxy.example", map[string]string{"stable": second.URL})
	response := serveHelmRequest(secondHandler, "/stable/index.yaml")
	if response.Code != http.StatusOK {
		t.Fatalf("second index status = %d, want 200: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), secondDigest) {
		t.Errorf("second index did not use the new upstream: %s", response.Body.String())
	}
	if firstRequests != 1 {
		t.Errorf("first upstream requests = %d, want 1", firstRequests)
	}
	if secondRequests != 1 {
		t.Errorf("second upstream requests = %d, want 1", secondRequests)
	}
}

func TestHelmHandler_UsesConfiguredUpstreamAuthentication(t *testing.T) {
	chart := []byte("private Helm chart")
	digest := helmSHA256Hex(chart)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/index.yaml":
			_, _ = fmt.Fprintf(w, "apiVersion: v1\nentries:\n  demo:\n    - digest: %s\n      urls: [demo.tgz]\n", digest)
		case "/demo.tgz":
			_, _ = w.Write(chart)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	authClient := &http.Client{Transport: upstreamhttp.NewTransport(http.DefaultTransport,
		upstreamhttp.AuthFunc(func(string) (string, string) {
			return "Authorization", "Bearer private-token"
		}))}
	proxy.HTTPClient = authClient
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(authClient), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	h := NewHelmHandler(proxy, "http://proxy.example", map[string]string{"private": upstream.URL})

	response := serveHelmRequest(h, "/private/charts/"+digest+"/demo.tgz")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	if got := response.Body.String(); got != string(chart) {
		t.Errorf("body = %q, want %q", got, chart)
	}
}

func TestHelmHandler_FiltersNewChartsFromIndex(t *testing.T) {
	oldDigest := strings.Repeat("a", 64)
	newDigest := strings.Repeat("b", 64)
	proxy := &Proxy{Cooldown: &cooldown.Config{Default: "3d"}}
	h := NewHelmHandler(proxy, "http://proxy.example", map[string]string{"test": "https://charts.example"})

	body := fmt.Sprintf(`apiVersion: v1
entries:
  demo:
    - created: %s
      digest: %s
      urls: [demo-old.tgz]
    - created: %s
      digest: %s
      urls: [demo-new.tgz]
`, time.Now().Add(-10*24*time.Hour).Format(time.RFC3339), oldDigest,
		time.Now().Add(-time.Hour).Format(time.RFC3339), newDigest)

	rewritten, err := h.rewriteIndex("test", "https://charts.example", []byte(body))
	if err != nil {
		t.Fatalf("rewriteIndex() error = %v", err)
	}
	if strings.Contains(string(rewritten), newDigest) {
		t.Errorf("rewritten index includes a chart still in cooldown: %s", rewritten)
	}
	if !strings.Contains(string(rewritten), oldDigest) {
		t.Errorf("rewritten index omitted an old chart: %s", rewritten)
	}
}

func TestNormalizeHelmDigest(t *testing.T) {
	digest := strings.Repeat("a", 64)
	for _, input := range []string{digest, "sha256:" + digest, "SHA256:" + strings.ToUpper(digest)} {
		if got, ok := normalizeHelmDigest(input); !ok || got != digest {
			t.Errorf("normalizeHelmDigest(%q) = %q, %t; want %q, true", input, got, ok, digest)
		}
	}
	if _, ok := normalizeHelmDigest("bad"); ok {
		t.Error("normalizeHelmDigest accepted an invalid digest")
	}
}

func serveHelmRequest(h *HelmHandler, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	return w
}

func helmSHA256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
