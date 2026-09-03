package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

func TestCondaParseFilename(t *testing.T) {
	h := &CondaHandler{proxy: &Proxy{Logger: slog.Default()}}

	tests := []struct {
		filename    string
		wantName    string
		wantVersion string
	}{
		{"numpy-1.24.0-py311h64a7726_0.conda", "numpy", "1.24.0"},
		{"scipy-1.11.4-py310h64a7726_0.tar.bz2", "scipy", "1.11.4"},
		{"python-dateutil-2.8.2-pyhd8ed1ab_0.conda", "python-dateutil", "2.8.2"},
		{"ca-certificates-2023.11.17-hbcca054_0.conda", "ca-certificates", "2023.11.17"},
		{"invalid", "", ""},
	}

	for _, tt := range tests {
		name, version := h.parseFilename(tt.filename)
		if name != tt.wantName || version != tt.wantVersion {
			t.Errorf("parseFilename(%q) = (%q, %q), want (%q, %q)",
				tt.filename, name, version, tt.wantName, tt.wantVersion)
		}
	}
}

func TestCondaIsPackageFile(t *testing.T) {
	h := &CondaHandler{}

	tests := []struct {
		filename string
		want     bool
	}{
		{"numpy-1.24.0-py311h64a7726_0.conda", true},
		{"scipy-1.11.4-py310h64a7726_0.tar.bz2", true},
		{"repodata.json", false},
		{"repodata.json.bz2", false},
	}

	for _, tt := range tests {
		got := h.isPackageFile(tt.filename)
		if got != tt.want {
			t.Errorf("isPackageFile(%q) = %v, want %v", tt.filename, got, tt.want)
		}
	}
}

func TestCondaCooldownFiltering(t *testing.T) {
	now := time.Now()
	oldTimestamp := float64(now.Add(-7 * 24 * time.Hour).UnixMilli())
	recentTimestamp := float64(now.Add(-1 * time.Hour).UnixMilli())

	repodata := map[string]any{
		"info": map[string]any{},
		"packages": map[string]any{
			"numpy-1.24.0-old.tar.bz2": map[string]any{
				"name":      "numpy",
				"version":   "1.24.0",
				"timestamp": oldTimestamp,
			},
			"numpy-1.25.0-new.tar.bz2": map[string]any{
				"name":      "numpy",
				"version":   "1.25.0",
				"timestamp": recentTimestamp,
			},
		},
		"packages.conda": map[string]any{
			"scipy-1.11.0-old.conda": map[string]any{
				"name":      "scipy",
				"version":   "1.11.0",
				"timestamp": oldTimestamp,
			},
			"scipy-1.12.0-new.conda": map[string]any{
				"name":      "scipy",
				"version":   "1.12.0",
				"timestamp": recentTimestamp,
			},
		},
	}

	body, err := json.Marshal(repodata)
	if err != nil {
		t.Fatal(err)
	}

	proxy := testProxy()
	proxy.Cooldown = &cooldown.Config{
		Default: "3d",
	}

	h := &CondaHandler{
		proxy:    proxy,
		proxyURL: "http://localhost:8080",
	}

	filtered, err := h.applyCooldownFiltering(body)
	if err != nil {
		t.Fatal(err)
	}

	var result map[string]any
	if err := json.Unmarshal(filtered, &result); err != nil {
		t.Fatal(err)
	}

	packages := result["packages"].(map[string]any)
	if len(packages) != 1 {
		t.Fatalf("expected 1 package in packages, got %d", len(packages))
	}
	if _, ok := packages["numpy-1.24.0-old.tar.bz2"]; !ok {
		t.Error("expected old numpy to survive filtering")
	}

	condaPkgs := result["packages.conda"].(map[string]any)
	if len(condaPkgs) != 1 {
		t.Fatalf("expected 1 package in packages.conda, got %d", len(condaPkgs))
	}
	if _, ok := condaPkgs["scipy-1.11.0-old.conda"]; !ok {
		t.Error("expected old scipy to survive filtering")
	}
}

func TestCondaCooldownFilteringWithPackageOverride(t *testing.T) {
	now := time.Now()
	recentTimestamp := float64(now.Add(-2 * time.Hour).UnixMilli())

	repodata := map[string]any{
		"info": map[string]any{},
		"packages": map[string]any{
			"special-1.0.0-build.tar.bz2": map[string]any{
				"name":      "special",
				"version":   "1.0.0",
				"timestamp": recentTimestamp,
			},
		},
		"packages.conda": map[string]any{},
	}

	body, err := json.Marshal(repodata)
	if err != nil {
		t.Fatal(err)
	}

	proxy := testProxy()
	proxy.Cooldown = &cooldown.Config{
		Default:  "3d",
		Packages: map[string]string{"pkg:conda/special": "1h"},
	}

	h := &CondaHandler{
		proxy:    proxy,
		proxyURL: "http://localhost:8080",
	}

	filtered, err := h.applyCooldownFiltering(body)
	if err != nil {
		t.Fatal(err)
	}

	var result map[string]any
	if err := json.Unmarshal(filtered, &result); err != nil {
		t.Fatal(err)
	}

	packages := result["packages"].(map[string]any)
	if len(packages) != 1 {
		t.Fatalf("expected 1 package (override allows it), got %d", len(packages))
	}
}

func TestCondaCooldownFilteringNoTimestamp(t *testing.T) {
	repodata := map[string]any{
		"info": map[string]any{},
		"packages": map[string]any{
			"old-pkg-1.0.0-build.tar.bz2": map[string]any{
				"name":    "old-pkg",
				"version": "1.0.0",
				// no timestamp field
			},
		},
		"packages.conda": map[string]any{},
	}

	body, err := json.Marshal(repodata)
	if err != nil {
		t.Fatal(err)
	}

	proxy := testProxy()
	proxy.Cooldown = &cooldown.Config{
		Default: "3d",
	}

	h := &CondaHandler{
		proxy:    proxy,
		proxyURL: "http://localhost:8080",
	}

	filtered, err := h.applyCooldownFiltering(body)
	if err != nil {
		t.Fatal(err)
	}

	var result map[string]any
	if err := json.Unmarshal(filtered, &result); err != nil {
		t.Fatal(err)
	}

	packages := result["packages"].(map[string]any)
	if len(packages) != 1 {
		t.Fatalf("entries without timestamp should pass through, got %d", len(packages))
	}
}

func TestCondaHandleRepodataWithCooldown(t *testing.T) {
	now := time.Now()
	oldTimestamp := float64(now.Add(-7 * 24 * time.Hour).UnixMilli())
	recentTimestamp := float64(now.Add(-1 * time.Hour).UnixMilli())

	repodataJSON, _ := json.Marshal(map[string]any{
		"info": map[string]any{},
		"packages": map[string]any{
			"old-1.0.0-build.tar.bz2": map[string]any{
				"name": "testpkg", "version": "1.0.0", "timestamp": oldTimestamp,
			},
			"new-2.0.0-build.tar.bz2": map[string]any{
				"name": "testpkg", "version": "2.0.0", "timestamp": recentTimestamp,
			},
		},
		"packages.conda": map[string]any{},
	})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(repodataJSON)
	}))
	defer upstream.Close()

	proxy := testProxy()
	proxy.Cooldown = &cooldown.Config{
		Default: "3d",
	}

	h := &CondaHandler{
		proxy:       proxy,
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/conda-forge/noarch/repodata.json", nil)
	req.SetPathValue("channel", "conda-forge")
	req.SetPathValue("arch", "noarch")
	w := httptest.NewRecorder()
	h.handleRepodata(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}

	packages := result["packages"].(map[string]any)
	if len(packages) != 1 {
		t.Fatalf("expected 1 package after filtering, got %d", len(packages))
	}
}

func TestCondaHandleRepodataWithoutCooldown(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"info":{},"packages":{},"packages.conda":{}}`))
	}))
	defer upstream.Close()

	h := &CondaHandler{
		proxy:       &Proxy{Logger: slog.Default(), HTTPClient: http.DefaultClient},
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/conda-forge/noarch/repodata.json", nil)
	req.SetPathValue("channel", "conda-forge")
	req.SetPathValue("arch", "noarch")
	w := httptest.NewRecorder()
	h.handleRepodata(w, req)

	// Without cooldown, should proxy directly (response comes from upstream)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

// TestCondaRepodataRequestsGzip covers issue #305: the large *.json repodata
// routes must fetch, cache and serve gzip-compressed (Content-Encoding: gzip)
// so neither hop pays the uncompressed size, while repodata.json.bz2 (already
// compressed) stays identity. conda/mamba/pixi solicit and decode gzip on .json.
func TestCondaRepodataRequestsGzip(t *testing.T) {
	plain := []byte(`{"packages":{},"repodata_version":1}`)
	compressed := gzipPayload(t, plain)

	var available atomic.Bool
	available.Store(true)
	var jsonUpstreamReqs atomic.Int32
	var sawAcceptEncoding sync.Map // path -> last Accept-Encoding seen

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAcceptEncoding.Store(r.URL.Path, r.Header.Get(headerAcceptEncoding))
		if strings.HasSuffix(r.URL.Path, ".json") {
			jsonUpstreamReqs.Add(1)
		}
		if !available.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if strings.HasSuffix(r.URL.Path, ".json") {
			if strings.Contains(r.Header.Get(headerAcceptEncoding), "gzip") {
				w.Header().Set(headerContentType, contentTypeJSON)
				w.Header().Set(headerContentEncoding, "gzip")
				_, _ = w.Write(compressed)
				return
			}
			w.Header().Set(headerContentType, contentTypeJSON)
			_, _ = w.Write(plain)
			return
		}
		// .bz2: already compressed, upstream sends no Content-Encoding.
		w.Header().Set(headerContentType, "application/octet-stream")
		_, _ = w.Write([]byte("bz2-bytes"))
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.HTTPClient = upstream.Client()
	routes := NewCondaHandlerWithUpstream(proxy, "http://proxy.local", upstream.URL).Routes()

	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		routes.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}
	lastAE := func(path string) string {
		v, _ := sawAcceptEncoding.Load(path)
		s, _ := v.(string)
		return s
	}

	for _, name := range []string{"repodata.json", "current_repodata.json"} {
		path := "/conda-forge/linux-64/" + name
		w := get(path)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200: %s", name, w.Code, w.Body.String())
		}
		if got := lastAE(path); got != "gzip" {
			t.Errorf("%s: upstream Accept-Encoding = %q, want %q", name, got, "gzip")
		}
		if !bytes.Equal(w.Body.Bytes(), compressed) {
			t.Errorf("%s: body not the compressed bytes (got %d, want %d)", name, w.Body.Len(), len(compressed))
		}
		if got := w.Header().Get(headerContentEncoding); got != "gzip" {
			t.Errorf("%s: Content-Encoding = %q, want %q", name, got, "gzip")
		}
		if got := w.Header().Get(headerContentLength); got != strconv.Itoa(len(compressed)) {
			t.Errorf("%s: Content-Length = %q, want %d", name, got, len(compressed))
		}
	}

	// .bz2 stays identity.
	wbz := get("/conda-forge/linux-64/repodata.json.bz2")
	if wbz.Code != http.StatusOK {
		t.Fatalf("bz2: status = %d, want 200", wbz.Code)
	}
	if got := lastAE("/conda-forge/linux-64/repodata.json.bz2"); got != "identity" {
		t.Errorf("bz2: upstream Accept-Encoding = %q, want %q", got, "identity")
	}
	if got := wbz.Header().Get(headerContentEncoding); got != "" {
		t.Errorf("bz2: Content-Encoding = %q, want empty", got)
	}

	// Cached replay with the upstream down: same compressed bytes + header, no new .json fetch.
	reqsBefore := jsonUpstreamReqs.Load()
	available.Store(false)
	wc := get("/conda-forge/linux-64/repodata.json")
	if wc.Code != http.StatusOK {
		t.Fatalf("cached repodata.json: status = %d, want 200: %s", wc.Code, wc.Body.String())
	}
	if !bytes.Equal(wc.Body.Bytes(), compressed) {
		t.Errorf("cached repodata.json: body not the compressed bytes")
	}
	if got := wc.Header().Get(headerContentEncoding); got != "gzip" {
		t.Errorf("cached repodata.json: Content-Encoding = %q, want %q", got, "gzip")
	}
	if jsonUpstreamReqs.Load() != reqsBefore {
		t.Errorf("cached repodata.json hit upstream: reqs %d -> %d", reqsBefore, jsonUpstreamReqs.Load())
	}
}

// TestCondaRepodataStreamPathRequestsGzip covers the default cache_metadata=off
// branch: the streaming path must also request gzip for repodata.json and
// forward the Content-Encoding header.
func TestCondaRepodataStreamPathRequestsGzip(t *testing.T) {
	plain := []byte(`{"packages":{}}`)
	compressed := gzipPayload(t, plain)
	var sawAcceptEncoding string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAcceptEncoding = r.Header.Get(headerAcceptEncoding)
		if strings.Contains(sawAcceptEncoding, "gzip") {
			w.Header().Set(headerContentType, contentTypeJSON)
			w.Header().Set(headerContentEncoding, "gzip")
			_, _ = w.Write(compressed)
			return
		}
		w.Header().Set(headerContentType, contentTypeJSON)
		_, _ = w.Write(plain)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = false
	proxy.HTTPClient = upstream.Client()
	routes := NewCondaHandlerWithUpstream(proxy, "http://proxy.local", upstream.URL).Routes()

	w := httptest.NewRecorder()
	routes.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/conda-forge/linux-64/repodata.json", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if sawAcceptEncoding != "gzip" {
		t.Errorf("stream path upstream Accept-Encoding = %q, want %q", sawAcceptEncoding, "gzip")
	}
	if !bytes.Equal(w.Body.Bytes(), compressed) {
		t.Errorf("stream path body not the compressed bytes")
	}
	if got := w.Header().Get(headerContentEncoding); got != "gzip" {
		t.Errorf("stream path Content-Encoding = %q, want %q", got, "gzip")
	}
}

// TestCondaRepodataGzipSurvivesCacheWriteFailure covers the failure the gzip
// route makes reachable: when the metadata cache write fails the freshly
// fetched body is still served, so its Content-Encoding must come from the
// fetch and not from the (unwritten) cache row -- otherwise gzip bytes go out
// labelled application/json with no Content-Encoding and every conda client
// fails to parse them.
func TestCondaRepodataGzipSurvivesCacheWriteFailure(t *testing.T) {
	plain := []byte(`{"packages":{},"repodata_version":1}`)
	compressed := gzipPayload(t, plain)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get(headerAcceptEncoding), "gzip") {
			w.Header().Set(headerContentType, contentTypeJSON)
			w.Header().Set(headerContentEncoding, "gzip")
			_, _ = w.Write(compressed)
			return
		}
		w.Header().Set(headerContentType, contentTypeJSON)
		_, _ = w.Write(plain)
	}))
	defer upstream.Close()

	proxy, _, store, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.HTTPClient = upstream.Client()
	store.storeErr = errors.New("disk full")

	routes := NewCondaHandlerWithUpstream(proxy, "http://proxy.local", upstream.URL).Routes()
	w := httptest.NewRecorder()
	routes.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/conda-forge/linux-64/repodata.json", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if !bytes.Equal(w.Body.Bytes(), compressed) {
		t.Fatalf("body is not the fetched compressed bytes (got %d, want %d)", w.Body.Len(), len(compressed))
	}
	// The cache row was never written, so the header must come from the fetch.
	if got := w.Header().Get(headerContentEncoding); got != "gzip" {
		t.Errorf("Content-Encoding = %q, want %q (gzip body would be unparseable without it)", got, "gzip")
	}
	if got := w.Header().Get(headerContentLength); got != strconv.Itoa(len(compressed)) {
		t.Errorf("Content-Length = %q, want %d", got, len(compressed))
	}
}

// TestCondaRepodataGzipStaleFallbackKeepsEncoding pins the stale-fallback
// return of fetchOrCacheMetadata: when the upstream fails after the entry has
// expired, the stored gzip blob must be served with its Content-Encoding taken
// from the cache row, not dropped.
func TestCondaRepodataGzipStaleFallbackKeepsEncoding(t *testing.T) {
	plain := []byte(`{"packages":{},"repodata_version":1}`)
	compressed := gzipPayload(t, plain)

	var available atomic.Bool
	available.Store(true)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !available.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if strings.Contains(r.Header.Get(headerAcceptEncoding), "gzip") {
			w.Header().Set(headerContentType, contentTypeJSON)
			w.Header().Set(headerContentEncoding, "gzip")
			_, _ = w.Write(compressed)
			return
		}
		w.Header().Set(headerContentType, contentTypeJSON)
		_, _ = w.Write(plain)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = 0 // every request revalidates; an upstream failure falls back to the stale row
	proxy.HTTPClient = upstream.Client()
	routes := NewCondaHandlerWithUpstream(proxy, "http://proxy.local", upstream.URL).Routes()

	get := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		routes.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/conda-forge/linux-64/repodata.json", nil))
		return w
	}

	if first := get(); first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200: %s", first.Code, first.Body.String())
	}
	available.Store(false)
	stale := get()
	if stale.Code != http.StatusOK {
		t.Fatalf("stale status = %d, want 200: %s", stale.Code, stale.Body.String())
	}
	if !bytes.Equal(stale.Body.Bytes(), compressed) {
		t.Errorf("stale body is not the stored compressed bytes")
	}
	if got := stale.Header().Get(headerContentEncoding); got != "gzip" {
		t.Errorf("stale Content-Encoding = %q, want %q", got, "gzip")
	}
}
