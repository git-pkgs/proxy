package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/registries/fetch"
)

func TestAPKHandler_parseAPKPath(t *testing.T) {
	h := &APKHandler{}

	assertPathParser(t, "parseAPKPath", h.parseAPKPath, []pathParseCase{
		{"v3.22/main/x86_64/busybox-1.37.0-r12.apk", "busybox", "1.37.0-r12", "x86_64"},
		{"v3.22/main/aarch64/alpine-baselayout-data-3.7.0-r0.apk", "alpine-baselayout-data", "3.7.0-r0", "aarch64"},
		{"edge/community/x86_64/openjdk21-jre-21.0.2_p13-r1.apk", "openjdk21-jre", "21.0.2_p13-r1", "x86_64"},
		{"busybox-1.37.0-r12.apk", "busybox", "1.37.0-r12", ""},
		{"v3.22/main/x86_64/invalid.apk", "", "", ""},
		{"v3.22/main/x86_64/not-an-apk-file", "", "", ""},
	})
}

func TestAPKHandler_Routes(t *testing.T) {
	h := NewAPKHandler(nil, "http://localhost:8080", nil)
	assertRoutesBasics(t, h.Routes(), "/alpine/v3.22/main/x86_64/APKINDEX.tar.gz", "/alpine/v3.22/../../../etc/passwd")
}

func TestAPKHandler_DefaultsToOfficialMirror(t *testing.T) {
	h := NewAPKHandler(nil, "http://localhost:8080", nil)
	if got := h.repositories[defaultAPKRepositoryName]; got != defaultAPKUpstream {
		t.Errorf("default repository = %q, want %q", got, defaultAPKUpstream)
	}
}

func TestAPKHandler_UnknownRepositoryReturns404(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	h := NewAPKHandler(proxy, "http://localhost:8080", map[string]string{"alpine": "https://example.test"})

	for _, target := range []string{
		"/unknown/v3.22/main/x86_64/APKINDEX.tar.gz",
		"/alpine",
		"/",
	} {
		w := serveAPKRequest(h, target)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", target, w.Code)
		}
	}
}

// TestAPKHandler_MetadataCacheKeysDoNotCollideAcrossRepositories guards the
// hashed metadata cache key: with a separator-based key, repositories named
// "alpine" and "alpine_edge" would share cache entries for
// /alpine/edge/main/x86_64/APKINDEX.tar.gz and
// /alpine_edge/main/x86_64/APKINDEX.tar.gz, serving one repository's signed
// index to clients of the other.
func TestAPKHandler_MetadataCacheKeysDoNotCollideAcrossRepositories(t *testing.T) {
	indexA := "signed index of repository A"
	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/edge/main/x86_64/APKINDEX.tar.gz" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprint(w, indexA)
	}))
	defer upstreamA.Close()

	indexB := "signed index of repository B"
	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/main/x86_64/APKINDEX.tar.gz" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprint(w, indexB)
	}))
	defer upstreamB.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.HTTPClient = http.DefaultClient

	h := NewAPKHandler(proxy, "http://proxy.example", map[string]string{
		"alpine":      upstreamA.URL,
		"alpine_edge": upstreamB.URL,
	})

	first := serveAPKRequest(h, "/alpine/edge/main/x86_64/APKINDEX.tar.gz")
	if first.Code != http.StatusOK || first.Body.String() != indexA {
		t.Fatalf("repository A: status = %d, body = %q, want 200 %q", first.Code, first.Body.String(), indexA)
	}

	// Served within the metadata TTL: a colliding key would return indexA here.
	second := serveAPKRequest(h, "/alpine_edge/main/x86_64/APKINDEX.tar.gz")
	if second.Code != http.StatusOK {
		t.Fatalf("repository B: status = %d, want 200: %s", second.Code, second.Body.String())
	}
	if second.Body.String() != indexB {
		t.Errorf("repository B served %q, want %q (cache key collision)", second.Body.String(), indexB)
	}
}

// TestAPKHandler_IndexesServedUnchanged covers v2 (APKINDEX.tar.gz) and v3
// (Packages.adb) indexes plus detached signatures: bytes must be served
// unchanged so apk signature verification keeps working, and within the
// metadata TTL cached copies must be served without contacting the upstream
// (the stale-after-TTL fallback itself is covered by the shared ProxyCached
// tests).
func TestAPKHandler_IndexesServedUnchanged(t *testing.T) {
	files := map[string][]byte{
		"/v3.22/main/x86_64/APKINDEX.tar.gz":  []byte("\x1f\x8b\x08v2-index-with-embedded-signature"),
		"/v3.22/main/x86_64/Packages.adb":     []byte("ADB.v3-index-binary\x00payload"),
		"/v3.22/main/x86_64/Packages.adb.sig": []byte("detached-signature-bytes"),
	}

	var available atomic.Bool
	available.Store(true)
	var upstreamRequests atomic.Int32
	var authHeader string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !available.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		authHeader = r.Header.Get("Authorization")
		data, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		upstreamRequests.Add(1)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(data)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.HTTPClient = upstream.Client()
	proxy.AuthForURL = func(string) (string, string) {
		return "Authorization", "Bearer apk-token"
	}

	h := NewAPKHandler(proxy, "http://proxy.example", map[string]string{"alpine": upstream.URL})

	for path, want := range files {
		w := serveAPKRequest(h, "/alpine"+path)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200: %s", path, w.Code, w.Body.String())
		}
		if got := w.Body.Bytes(); string(got) != string(want) {
			t.Errorf("%s: body altered:\ngot  %q\nwant %q", path, got, want)
		}
	}
	if authHeader != "Bearer apk-token" {
		t.Errorf("Authorization = %q, want %q", authHeader, "Bearer apk-token")
	}

	// Upstream goes away: cached indexes must still be served, unchanged.
	available.Store(false)
	requestsBefore := upstreamRequests.Load()
	for path, want := range files {
		w := serveAPKRequest(h, "/alpine"+path)
		if w.Code != http.StatusOK {
			t.Fatalf("%s offline: status = %d, want 200: %s", path, w.Code, w.Body.String())
		}
		if got := w.Body.Bytes(); string(got) != string(want) {
			t.Errorf("%s offline: body altered:\ngot  %q\nwant %q", path, got, want)
		}
	}
	if got := upstreamRequests.Load(); got != requestsBefore {
		t.Errorf("upstream requests during offline reads = %d, want %d", got, requestsBefore)
	}
}

// TestAPKHandler_PackageDownloadCachesPerArch covers package downloads, cache
// hits, offline reads, and that identically named packages for different
// architectures are cached separately.
func TestAPKHandler_PackageDownloadCachesPerArch(t *testing.T) {
	packages := map[string][]byte{
		"/v3.22/main/x86_64/busybox-1.37.0-r12.apk":  []byte("x86_64 package bytes"),
		"/v3.22/main/aarch64/busybox-1.37.0-r12.apk": []byte("aarch64 package bytes"),
	}

	var available atomic.Bool
	available.Store(true)
	var packageRequests atomic.Int32

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !available.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		data, ok := packages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		packageRequests.Add(1)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(data)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })

	h := NewAPKHandler(proxy, "http://proxy.example", map[string]string{"alpine": upstream.URL})

	for path, want := range packages {
		w := serveAPKRequest(h, "/alpine"+path)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200: %s", path, w.Code, w.Body.String())
		}
		if got := w.Body.String(); got != string(want) {
			t.Errorf("%s: body = %q, want %q", path, got, want)
		}
	}
	if got := packageRequests.Load(); got != 2 {
		t.Fatalf("upstream package requests = %d, want 2 (one per architecture)", got)
	}

	// Second round must be served from cache, even with the upstream down.
	available.Store(false)
	for path, want := range packages {
		w := serveAPKRequest(h, "/alpine"+path)
		if w.Code != http.StatusOK {
			t.Fatalf("%s cached: status = %d, want 200: %s", path, w.Code, w.Body.String())
		}
		if got := w.Body.String(); got != string(want) {
			t.Errorf("%s cached: body = %q, want %q", path, got, want)
		}
	}
	if got := packageRequests.Load(); got != 2 {
		t.Errorf("upstream package requests after cache hits = %d, want 2", got)
	}
}

func TestAPKHandler_PackageDownloadSendsUpstreamAuth(t *testing.T) {
	var authHeader string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		if authHeader != "Bearer apk-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprint(w, "private package")
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	client := upstream.Client()
	client.Transport = &authRoundTripper{base: client.Transport, header: "Authorization", value: "Bearer apk-token"}
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(client), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })

	h := NewAPKHandler(proxy, "http://proxy.example", map[string]string{"private": upstream.URL})

	w := serveAPKRequest(h, "/private/v3.22/main/x86_64/busybox-1.37.0-r12.apk")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != "private package" {
		t.Errorf("body = %q, want %q", w.Body.String(), "private package")
	}
	if authHeader != "Bearer apk-token" {
		t.Errorf("Authorization = %q, want %q", authHeader, "Bearer apk-token")
	}
}

func TestAPKHandler_UnparseablePackageProxiedDirectly(t *testing.T) {
	var requested string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = r.URL.Path
		_, _ = fmt.Fprint(w, "raw bytes")
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()

	h := NewAPKHandler(proxy, "http://proxy.example", map[string]string{"alpine": upstream.URL})

	w := serveAPKRequest(h, "/alpine/v3.22/main/x86_64/no-version.apk")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if requested != "/v3.22/main/x86_64/no-version.apk" {
		t.Errorf("upstream path = %q, want %q", requested, "/v3.22/main/x86_64/no-version.apk")
	}
	if w.Body.String() != "raw bytes" {
		t.Errorf("body = %q, want %q", w.Body.String(), "raw bytes")
	}
}

// authRoundTripper adds a static auth header, mimicking the server's
// authentication-aware upstream transport.
type authRoundTripper struct {
	base   http.RoundTripper
	header string
	value  string
}

func (a *authRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set(a.header, a.value)
	base := a.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

func serveAPKRequest(h *APKHandler, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	return w
}
