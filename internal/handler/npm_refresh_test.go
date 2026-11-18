package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/registries/fetch"
)

func TestNPMUnknownVersionRefreshesPackument(t *testing.T) {
	var published, hits atomic.Int32
	published.Store(1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentTypeJSON)
		if strings.Contains(r.URL.Path, "/-/") {
			_, _ = io.WriteString(w, npmIntegrityTarball)
			return
		}
		version := func(v string) map[string]any {
			return map[string]any{"name": "leftpad", "version": v, "dist": map[string]string{"integrity": sha512SRI(npmIntegrityTarball), "tarball": "http://" + r.Host + "/leftpad/-/declared-" + v + ".tgz"}}
		}
		if strings.Count(r.URL.Path, "/") > 1 {
			_ = json.NewEncoder(w).Encode(version("2.0.0"))
			return
		}
		hits.Add(1)
		versions := map[string]any{"1.0.0": version("1.0.0")}
		if published.Load() == 2 {
			versions["2.0.0"] = version("2.0.0")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "leftpad", "versions": versions})
	}))
	defer upstream.Close()
	p, _, _, fetcher := setupTestProxy(t)
	p.CacheMetadata = true
	p.MetadataTTL = 24 * time.Hour
	p.HTTPClient = upstream.Client()
	// Exercise the actual recorded SRI using the existing recording fetcher.
	fetcher.artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader(npmIntegrityTarball)), Size: int64(len(npmIntegrityTarball))}
	h := NewNPMHandler(p, "http://proxy.test", upstream.URL)
	request := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	if got := request("/leftpad"); got.Code != 200 {
		t.Fatal(got.Code)
	}
	published.Store(2)
	path := "/leftpad/-/leftpad-2.0.0.tgz"
	if got := request(path); got.Code != 200 {
		t.Fatalf("%s: %d %s", path, got.Code, got.Body.String())
	}
	if got := request("/leftpad"); !strings.Contains(got.Body.String(), `"2.0.0"`) {
		t.Fatalf("packument still stale: %s", got.Body.String())
	}
	if hits.Load() != 2 {
		t.Fatalf("packument requests = %d, want 2", hits.Load())
	}
	if fetcher.fetchedURL != upstream.URL+"/leftpad/-/declared-2.0.0.tgz" {
		t.Fatalf("did not use refreshed tarball URL: %s", fetcher.fetchedURL)
	}
	pkg, ver, _ := packagePURLStrings("npm", "leftpad", "2.0.0")
	record, err := p.DB.GetCachedArtifact(pkg, ver, "leftpad-2.0.0.tgz")
	if err != nil || record == nil || record.Integrity.String != sha512SRI(npmIntegrityTarball) {
		t.Fatalf("wrong recorded integrity: %v %v", record, err)
	}
	upstream.Close()
	cached, err := p.GetCachedArtifact(context.Background(), "npm", "leftpad", "2.0.0", "leftpad-2.0.0.tgz")
	if err != nil || cached == nil {
		t.Fatalf("offline cached artifact: %v %v", cached, err)
	}
	_ = cached.Reader.Close()
}

func TestNPMUnknownVersionRefreshLimited(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cooldown=%t", enabled), func(t *testing.T) {
			testNPMUnknownVersionRefreshLimited(t, enabled)
		})
	}
}

func testNPMUnknownVersionRefreshLimited(t *testing.T, cooldownEnabled bool) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/-/") {
			_, _ = io.WriteString(w, "package")
			return
		}
		hits.Add(1)
		_, _ = io.WriteString(w, `{"name":"leftpad","versions":{"1.0.0":{"name":"leftpad","version":"1.0.0"}}}`)
	}))
	defer upstream.Close()
	p, _, _, _ := setupTestProxy(t)
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	p.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	p.CacheMetadata = true
	p.MetadataTTL = 24 * time.Hour
	p.HTTPClient = upstream.Client()
	if cooldownEnabled {
		p.Cooldown = &cooldown.Config{Default: "7d"}
	}
	h := NewNPMHandler(p, "http://proxy.test", upstream.URL)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/leftpad", nil))
	for range 2 {
		var wg sync.WaitGroup
		for i := range 12 {
			wg.Go(func() {
				rec := httptest.NewRecorder()
				h.Routes().ServeHTTP(rec, httptest.NewRequest("GET", fmt.Sprintf("/leftpad/-/leftpad-9.0.%d.tgz", i), nil))
				if rec.Code != http.StatusOK {
					t.Errorf("unlisted version fallback status = %d, want 200", rec.Code)
				}
			})
		}
		wg.Wait()
	}
	if hits.Load() != 2 {
		t.Errorf("upstream requests = %d, want warm + one refresh", hits.Load())
	}
}

func TestNPMMissingVersionRefreshStaysStreamed(t *testing.T) {
	u := newStreamUpstream(t, "")
	u.replace(bigPackumentAt(u.URL, 5), `"v1"`)
	p, _, handler := streamProxy(t, u)
	p.Cooldown = nil
	p.MetadataMaxSize = 1024
	decodeStreamed(t, serve(handler, "/big", nil))
	u.replace(bigPackumentAt(u.URL, 100), `"v2"`)
	fetcher := p.Fetcher.(*mockFetcher)
	fetcher.artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader("package"))}
	path := "/big/-/big-1.0.50.tgz"
	if rec := serve(handler, path, nil); rec.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
	}
	if doc := decodeStreamed(t, serve(handler, "/big", nil)); doc.Versions["1.0.50"] == nil {
		t.Fatal("shared streamed packument is still stale")
	}
	if got := u.requests.Load(); got != 2 {
		t.Fatalf("packument requests = %d, want warm + refresh", got)
	}
}

func TestNPMMissingVersionRefreshLimitsFailuresAndRetriesAfterExpiry(t *testing.T) {
	u := newStreamUpstream(t, bigPackument(5))
	p, _, handler := streamProxy(t, u)
	decodeStreamed(t, serve(handler, "/big", nil))
	h := NewNPMHandler(p, "http://proxy.example", u.URL)
	u.fail.Store(true)
	if res := h.refreshMissingPackument(t.Context(), u.URL+"/big", "big"); res.err == nil {
		t.Fatalf("failed refresh: error=%v", res.err)
	}
	for range 3 {
		if res := h.refreshMissingPackument(t.Context(), u.URL+"/big", "big"); res.err == nil {
			t.Fatal("limited request lost the refresh error")
		}
	}
	if got := u.requests.Load(); got != 2 {
		t.Fatalf("requests after limited failure = %d, want 2", got)
	}
	// Expire the attempt without a wall-clock wait or pinning its duration.
	h.refreshMu.Lock()
	h.refreshAfter["big"] = time.Now().Add(-time.Hour)
	h.refreshMu.Unlock()
	u.fail.Store(false)
	u.replace(bigPackument(100), `"v2"`)
	res := h.refreshMissingPackument(t.Context(), u.URL+"/big", "big")
	if res.err != nil || res.entry == nil {
		t.Fatalf("retry: error=%v", res.err)
	}
	if got := u.requests.Load(); got != 3 {
		t.Fatalf("requests after expiry = %d, want 3", got)
	}
}

func TestNPMMissingVersionRefreshSharesDocumentAcrossVersions(t *testing.T) {
	var published atomic.Bool
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		versions := map[string]any{"1.0.0": map[string]string{"version": "1.0.0"}}
		if published.Load() {
			for i := range 24 {
				v := fmt.Sprintf("2.0.%d", i)
				versions[v] = map[string]string{"version": v}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "leftpad", "versions": versions})
	}))
	defer upstream.Close()
	p, _, _, _ := setupTestProxy(t)
	p.CacheMetadata = true
	p.MetadataTTL = time.Hour
	p.HTTPClient = upstream.Client()
	h := NewNPMHandler(p, "http://proxy.test", upstream.URL)
	if _, err := h.versionData(t.Context(), "leftpad", upstream.URL+"/leftpad", "leftpad", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	published.Store(true)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range 24 {
		wg.Go(func() {
			<-start
			version := fmt.Sprintf("2.0.%d", i)
			data, err := h.versionData(t.Context(), "leftpad", upstream.URL+"/leftpad", "leftpad", version)
			if err != nil {
				t.Errorf("%s: %v", version, err)
				return
			}
			var entry struct{ Version string }
			if err := json.Unmarshal(data.entry, &entry); err != nil || entry.Version != version {
				t.Errorf("%s got %s: %v", version, data.entry, err)
			}
		})
	}
	close(start)
	wg.Wait()
	if got := hits.Load(); got != 2 {
		t.Fatalf("requests = %d, want warm + shared refresh", got)
	}
}

// TestNPMMissingVersionRefreshPreservesError checks the HTTP boundary as well as
// the limiter: a failed refresh must not turn into a fallback or a 404.
func TestNPMMissingVersionRefreshPreservesError(t *testing.T) {
	var fail atomic.Bool
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"name":"leftpad","versions":{"1.0.0":{"version":"1.0.0"}}}`)
	}))
	defer upstream.Close()
	p, _, _, _ := setupTestProxy(t)
	p.CacheMetadata = true
	p.MetadataTTL = time.Hour
	p.HTTPClient = upstream.Client()
	p.Cooldown = &cooldown.Config{Default: "7d"}
	h := NewNPMHandler(p, "http://proxy.test", upstream.URL)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/leftpad", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("warm: %d", rec.Code)
	}
	fail.Store(true)
	for i := range 2 {
		rec := httptest.NewRecorder()
		h.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/leftpad/-/leftpad-9.0.0.tgz", nil))
		if rec.Code < 500 || rec.Code > 599 {
			t.Fatalf("request %d: got %d, want 5xx: %s", i, rec.Code, rec.Body.String())
		}
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("requests = %d, want warm + one failed refresh", got)
	}
}
