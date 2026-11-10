package handler

import (
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

// TestNPMTarballKeepsCachedPackumentFormat checks that a tarball download
// revalidates the packument in the format the metadata route cached it in,
// rather than replacing the abbreviated document every client is served with
// the full one.
func TestNPMTarballKeepsCachedPackumentFormat(t *testing.T) {
	const abbreviated = "application/vnd.npm.install-v1+json"
	var (
		mu      sync.Mutex
		accepts []string
	)
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept := r.Header.Get("Accept")
		mu.Lock()
		accepts = append(accepts, accept)
		mu.Unlock()
		contentType, etag := contentTypeJSON, `"full"`
		if strings.HasPrefix(accept, abbreviated) {
			contentType, etag = abbreviated, `"abbreviated"`
		}
		w.Header().Set(headerETag, etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set(headerContentType, contentType)
		_, _ = fmt.Fprintf(w, `{"name":"demo","versions":{"1.0.0":{"name":"demo","version":"1.0.0","dist":{"tarball":%q}}}}`,
			upstream.URL+"/demo/-/demo-1.0.0.tgz")
	}))
	t.Cleanup(upstream.Close)

	proxy, db, _, artifactFetcher := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Nanosecond // the tarball request revalidates
	proxy.HTTPClient = upstream.Client()
	artifactFetcher.artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader("package")), ContentType: "application/gzip"}
	h := NewNPMHandler(proxy, "http://proxy.test", upstream.URL)

	if w := serveNPM(t, h, http.MethodGet, "/demo", nil, nil); w.Code != http.StatusOK {
		t.Fatalf("metadata: status %d: %s", w.Code, w.Body.String())
	}
	if w := serveNPM(t, h, http.MethodGet, "/demo/-/demo-1.0.0.tgz", nil, nil); w.Code != http.StatusOK {
		t.Fatalf("tarball: status %d: %s", w.Code, w.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(accepts) != 2 {
		t.Fatalf("upstream packument requests = %d (%q), want 2", len(accepts), accepts)
	}
	for _, accept := range accepts {
		if accept != npmAcceptDefault {
			t.Errorf("packument requested with Accept %q, want %q", accept, npmAcceptDefault)
		}
	}
	entry, err := db.GetMetadataCache("npm", "demo")
	if err != nil || entry == nil {
		t.Fatalf("cache row: %v, %v", entry, err)
	}
	if entry.ContentType.String != abbreviated {
		t.Errorf("cached packument Content-Type = %q, want %q", entry.ContentType.String, abbreviated)
	}
}

// abbreviatedThenFullRegistry serves a packument with a 2.0.0 published an
// hour ago and a 1.0.0 published a month ago. The abbreviated form, like
// registry.npmjs.org's, has no time map. fail makes requests for the full
// document fail.
func abbreviatedThenFullRegistry(t *testing.T, fail *atomic.Bool, fullRequests *atomic.Int64) *httptest.Server {
	t.Helper()
	const abbreviated = "application/vnd.npm.install-v1+json"
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/demo" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		versions := fmt.Sprintf(`{"1.0.0":{"name":"demo","version":"1.0.0","dist":{"tarball":%q}},"2.0.0":{"name":"demo","version":"2.0.0","dist":{"tarball":%q}}}`,
			upstream.URL+"/demo/-/demo-1.0.0.tgz", upstream.URL+"/demo/-/demo-2.0.0.tgz")
		if strings.HasPrefix(r.Header.Get("Accept"), abbreviated) {
			w.Header().Set(headerContentType, abbreviated)
			_, _ = fmt.Fprintf(w, `{"name":"demo","modified":%q,"dist-tags":{"latest":"2.0.0"},"versions":%s}`,
				time.Now().Format(time.RFC3339), versions)
			return
		}
		fullRequests.Add(1)
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set(headerContentType, contentTypeJSON)
		_, _ = fmt.Fprintf(w, `{"name":"demo","dist-tags":{"latest":"2.0.0"},"time":{"1.0.0":%q,"2.0.0":%q},"versions":%s}`,
			time.Now().Add(-30*24*time.Hour).Format(time.RFC3339), time.Now().Add(-time.Hour).Format(time.RFC3339), versions)
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

// seedAbbreviatedPackument caches the abbreviated packument for demo through
// the metadata route while cooldown is off, then turns cooldown on.
func seedAbbreviatedPackument(t *testing.T, upstream *httptest.Server) *NPMHandler {
	t.Helper()
	proxy, db, _, artifactFetcher := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.HTTPClient = upstream.Client()
	artifactFetcher.artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader("package")), ContentType: "application/gzip"}
	h := NewNPMHandler(proxy, "http://proxy.test", upstream.URL)

	if w := serveNPM(t, h, http.MethodGet, "/demo", nil, nil); w.Code != http.StatusOK {
		t.Fatalf("seeding metadata: status %d: %s", w.Code, w.Body.String())
	}
	entry, err := db.GetMetadataCache("npm", "demo")
	if err != nil || entry == nil || entry.ContentType.String != "application/vnd.npm.install-v1+json" {
		t.Fatalf("expected an abbreviated packument in the cache, got %+v (%v)", entry, err)
	}

	proxy.Cooldown = &cooldown.Config{Default: "7d"}
	return h
}

// A packument cached in abbreviated form before cooldown was turned on has
// no publish times, so cooldown could not filter anything while it stayed
// fresh. With cooldown on it must be refetched in full.
func TestNPMCooldownRefetchesCachedAbbreviatedPackument(t *testing.T) {
	var fail atomic.Bool
	var fullRequests atomic.Int64
	upstream := abbreviatedThenFullRegistry(t, &fail, &fullRequests)
	h := seedAbbreviatedPackument(t, upstream)

	w := serveNPM(t, h, http.MethodGet, "/demo", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("metadata: status %d: %s", w.Code, w.Body.String())
	}
	var doc struct {
		DistTags map[string]string          `json:"dist-tags"`
		Versions map[string]json.RawMessage `json:"versions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decoding metadata: %v", err)
	}
	if _, ok := doc.Versions["2.0.0"]; ok {
		t.Error("version 2.0.0, published an hour ago, was not filtered by cooldown")
	}
	if _, ok := doc.Versions["1.0.0"]; !ok {
		t.Error("version 1.0.0 missing")
	}
	if doc.DistTags["latest"] != "1.0.0" {
		t.Errorf("dist-tags.latest = %q, want 1.0.0", doc.DistTags["latest"])
	}
	if fullRequests.Load() == 0 {
		t.Error("full packument was never fetched from upstream")
	}

	if w := serveNPM(t, h, http.MethodGet, "/demo/-/demo-2.0.0.tgz", nil, nil); w.Code != http.StatusNotFound {
		t.Errorf("tarball 2.0.0: status %d, want %d (withheld by cooldown)", w.Code, http.StatusNotFound)
	}
}

// The tarball check alone must not trust the abbreviated packument either.
func TestNPMCooldownTarballRefetchesCachedAbbreviatedPackument(t *testing.T) {
	var fail atomic.Bool
	var fullRequests atomic.Int64
	upstream := abbreviatedThenFullRegistry(t, &fail, &fullRequests)
	h := seedAbbreviatedPackument(t, upstream)

	if w := serveNPM(t, h, http.MethodGet, "/demo/-/demo-2.0.0.tgz", nil, nil); w.Code != http.StatusNotFound {
		t.Errorf("tarball 2.0.0: status %d, want %d (withheld by cooldown)", w.Code, http.StatusNotFound)
	}
	if fullRequests.Load() == 0 {
		t.Error("full packument was never fetched from upstream")
	}
}

// When upstream cannot supply the full packument, the abbreviated one must not
// stand in for it as a stale copy: it would let every version through.
func TestNPMCooldownSkipsAbbreviatedStaleFallback(t *testing.T) {
	var fail atomic.Bool
	var fullRequests atomic.Int64
	upstream := abbreviatedThenFullRegistry(t, &fail, &fullRequests)
	h := seedAbbreviatedPackument(t, upstream)
	h.proxy.MetadataTTL = time.Nanosecond // force a refetch
	fail.Store(true)

	w := serveNPM(t, h, http.MethodGet, "/demo", nil, nil)
	if w.Code != http.StatusBadGateway {
		t.Errorf("metadata: status %d, want %d; body: %s", w.Code, http.StatusBadGateway, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "2.0.0") {
		t.Errorf("served the abbreviated packument as a stale copy: %s", w.Body.String())
	}
}

// Cooldown profiles share a packument's cache row with the unprefixed URLs.
// With cooldown on for only some of them, the others must not replace the
// full packument with the abbreviated one: the next request under cooldown
// would fetch it again, and have no stale copy when upstream fails.
func TestNPMCooldownProfilesShareFullPackument(t *testing.T) {
	for _, tc := range []struct{ name, global, profile string }{
		{"cooldown in the profile", "0", "7d"},
		{"cooldown in the global policy", "7d", "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var fail atomic.Bool
			var fullRequests atomic.Int64
			upstream := abbreviatedThenFullRegistry(t, &fail, &fullRequests)
			proxy, db, _, _ := setupTestProxy(t)
			proxy.CacheMetadata = true
			proxy.MetadataTTL = time.Nanosecond // every request revalidates
			proxy.HTTPClient = upstream.Client()
			proxy.Cooldown = profilePolicy(t, tc.global)
			global := NewNPMHandler(proxy, "http://proxy.test", upstream.URL)
			profile := NewNPMHandler(proxy.WithCooldown(profilePolicy(t, tc.profile), 0), "http://proxy.test/cooldown/p", upstream.URL)
			filtered, unfiltered := profile, global
			if tc.global != "0" {
				filtered, unfiltered = global, profile
			}

			for _, h := range []*NPMHandler{filtered, unfiltered} {
				if w := serveNPM(t, h, http.MethodGet, "/demo", nil, nil); w.Code != http.StatusOK {
					t.Fatalf("metadata: status %d: %s", w.Code, w.Body.String())
				}
			}
			entry, err := db.GetMetadataCache("npm", "demo")
			if err != nil || entry == nil {
				t.Fatalf("cache row: %v, %v", entry, err)
			}
			if entry.ContentType.String != contentTypeJSON {
				t.Errorf("cached packument Content-Type = %q, want %q", entry.ContentType.String, contentTypeJSON)
			}

			fail.Store(true)
			w := serveNPM(t, filtered, http.MethodGet, "/demo", nil, nil)
			if w.Code != http.StatusOK {
				t.Fatalf("metadata while upstream fails: status %d, want the stale full packument", w.Code)
			}
			if strings.Contains(w.Body.String(), "demo-2.0.0.tgz") {
				t.Errorf("stale packument lists a version in cooldown: %s", w.Body.String())
			}
		})
	}
}
