package handler

import (
	"context"
	"encoding/json"
	"errors"
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
	if cooldownEnabled {
		// Non-concurrent missing versions must each check current upstream
		// metadata, even inside the non-cooldown limiter window.
		for i := range 24 {
			rec := serve(h.Routes(), fmt.Sprintf("/leftpad/-/leftpad-9.0.%d.tgz", i), nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("unlisted version fallback status = %d", rec.Code)
			}
		}
		if got := hits.Load(); got != 25 {
			t.Fatalf("cooldown upstream requests = %d, want warm + 24 refreshes", got)
		}
		return
	}
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
	p.Cooldown = nil
	decodeStreamed(t, serve(handler, "/big", nil))
	h := NewNPMHandler(p, "http://proxy.example", u.URL)
	u.fail.Store(true)
	if res := h.refreshMissingPackument(t.Context(), u.URL+"/big", "big", true); res.err == nil {
		t.Fatalf("failed refresh: error=%v", res.err)
	}
	for range 3 {
		if res := h.refreshMissingPackument(t.Context(), u.URL+"/big", "big", true); res.err == nil {
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
	res := h.refreshMissingPackument(t.Context(), u.URL+"/big", "big", true)
	if res.err != nil || res.entry == nil {
		t.Fatalf("retry: error=%v", res.err)
	}
	if got := u.requests.Load(); got != 3 {
		t.Fatalf("requests after expiry = %d, want 3", got)
	}
}

func TestNPMMissingVersionRefreshSharesDocumentAcrossVersions(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cooldown=%t", enabled), func(t *testing.T) {
			testNPMMissingVersionRefreshSharesDocumentAcrossVersions(t, enabled)
		})
	}
}

func testNPMMissingVersionRefreshSharesDocumentAcrossVersions(t *testing.T, cooldownEnabled bool) {
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
	if cooldownEnabled {
		p.Cooldown = &cooldown.Config{Default: "7d"}
	}
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
	got := hits.Load()
	if !cooldownEnabled {
		if got != 2 {
			t.Fatalf("requests = %d, want warm + shared refresh", got)
		}
		return
	}
	// Under cooldown nothing is retained between requests: a caller that
	// read the old packument but reaches the refresh after the shared one
	// finished fetches again. That depends on scheduling, so only bound it.
	if got < 2 || got > 25 {
		t.Fatalf("requests = %d, want warm + between 1 and 24 refreshes", got)
	}
}

// A failed cooldown refresh must not turn into a fallback or a 404, and the
// next request must be able to retry after upstream recovers.
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
	if got := hits.Load(); got != 3 {
		t.Fatalf("requests = %d, want warm + two failed refreshes", got)
	}
	fail.Store(false)
	p.Fetcher.(*mockFetcher).artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader("package"))}
	if rec := serve(h.Routes(), "/leftpad/-/leftpad-9.0.0.tgz", nil); rec.Code != http.StatusOK {
		t.Fatalf("recovered upstream still blocked: %d %s", rec.Code, rec.Body.String())
	}
}

func TestNPMCooldownRefreshWithholdsVersionPublishedInsideWindow(t *testing.T) {
	var published atomic.Bool
	var hits, tarballs atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/-/") {
			tarballs.Add(1)
			_, _ = io.WriteString(w, "package")
			return
		}
		hits.Add(1)
		versions := map[string]any{"1.0.0": map[string]string{"version": "1.0.0"}}
		times := map[string]string{"1.0.0": "2016-01-01T00:00:00Z"}
		if published.Load() {
			versions["2.0.0"] = map[string]string{"version": "2.0.0"}
			times["2.0.0"] = time.Now().UTC().Format(time.RFC3339)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "leftpad", "versions": versions, "time": times})
	}))
	defer upstream.Close()
	p, _, _, _ := setupTestProxy(t)
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	p.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	p.CacheMetadata, p.MetadataTTL, p.HTTPClient = true, time.Hour, upstream.Client()
	p.Cooldown = &cooldown.Config{Default: "7d"}
	h := NewNPMHandler(p, "http://proxy.test", upstream.URL)
	if rec := serve(h.Routes(), "/leftpad", nil); rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	if rec := serve(h.Routes(), "/leftpad/-/leftpad-9.0.0.tgz", nil); rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	published.Store(true)
	rec := serve(h.Routes(), "/leftpad/-/leftpad-2.0.0.tgz", nil)
	if rec.Code != http.StatusNotFound || hits.Load() != 3 || tarballs.Load() != 1 {
		t.Fatalf("new publication: status=%d metadata=%d tarballs=%d", rec.Code, hits.Load(), tarballs.Load())
	}
}

func TestNPMRefreshDoesNotRetainPackument(t *testing.T) {
	u := newStreamUpstream(t, bigPackument(100))
	p, _, _ := streamProxy(t, u)
	p.Cooldown = nil
	p.MetadataTTL = 0 // Use the non-streaming path, which returns a full body.
	h := NewNPMHandler(p, "http://proxy.test", u.URL)
	res := h.refreshMissingPackument(t.Context(), u.URL+"/big", "big", true)
	if res.err != nil || len(res.body) == 0 {
		t.Fatalf("in-memory refresh: %v, body bytes=%d", res.err, len(res.body))
	}
	h.refreshMu.Lock()
	retained := h.refreshResult["big"]
	h.refreshMu.Unlock()
	if len(retained.body) != 0 || retained.entry != nil {
		t.Fatalf("retained full packument: body=%d entry=%v", len(retained.body), retained.entry)
	}
}

func TestNPMRefreshReadsReplacedDocument(t *testing.T) {
	u := newStreamUpstream(t, bigPackument(5))
	p, _, _ := streamProxy(t, u)
	p.Cooldown = nil
	h := NewNPMHandler(p, "http://proxy.test", u.URL)
	res := h.refreshMissingPackument(t.Context(), u.URL+"/big", "big", true)
	if res.err != nil || res.entry == nil {
		t.Fatalf("refresh: %v", res.err)
	}
	// Replace bytes and row together, leaving the first waiter's row stale.
	body := bigPackument(100)
	if _, _, err := p.Storage.Store(t.Context(), res.entry.StoragePath, strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	current := *res.entry
	current.ContentDigest.String = sha256Digest([]byte(body))
	current.Size.Int64 = int64(len(body))
	if err := p.DB.UpsertMetadataCache(&current); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		data, err := h.readRefreshedNPMVersion(t.Context(), res, u.URL+"/big", "big", "1.0.50", true)
		if err != nil || data.entry == nil {
			t.Fatalf("waiter did not reread changed document: %s, %v", data.entry, err)
		}
	}
	if got := u.requests.Load(); got != 1 {
		t.Fatalf("recovery issued another upstream fetch: %d", got)
	}
}

func TestNPMRefreshHonorsDetachCap(t *testing.T) {
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-release:
		}
	}))
	defer upstream.Close()
	defer close(release)
	p, _, _, _ := setupTestProxy(t)
	p.CacheMetadata, p.MetadataTTL, p.HTTPClient = true, time.Hour, upstream.Client()
	for range maxDetachedMetadataFetches {
		if !p.acquireMetadataDetachSlot() {
			t.Fatal("could not occupy detach slot")
		}
		defer p.releaseMetadataDetachSlot()
	}
	h := NewNPMHandler(p, "http://proxy.test", upstream.URL)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan metadataResult, 1)
	go func() { done <- h.refreshMissingPackument(ctx, upstream.URL+"/leftpad", "leftpad", true) }()
	<-entered
	cancel()
	if res := <-done; !errors.Is(res.err, context.Canceled) {
		t.Fatalf("caller error = %v", res.err)
	}
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream fetch outlived caller with all detach slots occupied")
	}
}
