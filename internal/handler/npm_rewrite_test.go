package handler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/proxy/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestMetadataServesRewriteWithoutReadingStorage checks that a repeat
// request inside the metadata TTL is answered from the rewrite cache by the
// stored digest alone, without reading the cached document back.
func TestMetadataServesRewriteWithoutReadingStorage(t *testing.T) {
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/left-pad":
			_, _ = w.Write([]byte(`{"name":"left-pad","versions":{"1.3.0":{"dist":{"tarball":"https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz"}}}}`))
		case "/p2/vendor/pkg.json":
			_, _ = w.Write([]byte(`{"packages":{"vendor/pkg":[{"version":"1.0.0","dist":{"type":"zip","url":"https://example.com/pkg-1.0.0.zip"}}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	cases := []struct {
		ecosystem, name, path string
		handler               func(*Proxy) http.Handler
	}{
		{"npm", "left-pad", "/left-pad", func(p *Proxy) http.Handler {
			return NewNPMHandler(p, "http://proxy.example", upstream.URL).Routes()
		}},
		{"composer", "vendor/pkg", "/p2/vendor/pkg.json", func(p *Proxy) http.Handler {
			return NewComposerHandlerWithUpstreams(p, "http://proxy.example", upstream.URL, upstream.URL).Routes()
		}},
	}
	for _, c := range cases {
		t.Run(c.ecosystem, func(t *testing.T) {
			upstreamCalls.Store(0)
			proxy, db, store, _ := setupTestProxy(t)
			proxy.HTTPClient = upstream.Client()
			proxy.CacheMetadata = true
			proxy.MetadataTTL = time.Hour
			proxy.SetMetadataRewriteCacheSize(1 << 20)
			handler := c.handler(proxy)

			get := func() string {
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
				if rec.Code != http.StatusOK {
					t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
				}
				return rec.Body.String()
			}

			first := get()
			entry, err := db.GetMetadataCache(c.ecosystem, c.name)
			if err != nil || entry == nil || !strings.HasPrefix(entry.ContentDigest.String, "sha256:") {
				t.Fatalf("metadata row = %+v, %v; want a sha256 content digest", entry, err)
			}

			store.mu.Lock()
			store.openErr = errors.New("storage must not be read")
			store.mu.Unlock()

			if second := get(); second != first {
				t.Errorf("second response %q differs from first %q", second, first)
			}
			if got := upstreamCalls.Load(); got != 1 {
				t.Errorf("upstream calls = %d, want 1", got)
			}
		})
	}
}

// TestNPMStoredRewriteSkippedWhenStale checks that the digest fast path is
// only taken inside the metadata TTL.
func TestNPMStoredRewriteSkippedWhenStale(t *testing.T) {
	proxy, db, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.SetMetadataRewriteCacheSize(1 << 20)

	body := []byte(`{}`)
	proxy.cacheMetadataBlob(t.Context(), "npm", "demo", metadataStoragePath("npm", "demo"), &upstreamMetadata{body: body})
	key := rewriteCacheKey("npm", "http://proxy", "demo", body)
	if _, err := proxy.rewrites.rewrite(t.Context(), key, body, 0, func(b []byte) ([]byte, error) { return b, nil }); err != nil {
		t.Fatal(err)
	}
	if _, ok := proxy.storedRewrite("npm", "demo", "http://proxy", "demo"); !ok {
		t.Fatal("fresh row: expected a stored rewrite")
	}

	entry, _ := db.GetMetadataCache("npm", "demo")
	entry.FetchedAt.Time = time.Now().Add(-2 * time.Hour)
	if err := db.UpsertMetadataCache(entry); err != nil {
		t.Fatal(err)
	}
	if _, ok := proxy.storedRewrite("npm", "demo", "http://proxy", "demo"); ok {
		t.Error("stale row: expected no stored rewrite")
	}

	proxy.Cooldown = &cooldown.Config{Default: "3d"}
	entry.FetchedAt.Time = time.Now()
	_ = db.UpsertMetadataCache(entry)
	if _, ok := proxy.storedRewrite("npm", "demo", "http://proxy", "demo"); !ok {
		t.Error("cooldown on, fresh row: expected the stored rewrite")
	}
}

// TestNPMStoredRewriteRecordsMetadataHit checks the digest fast path counts as
// a metadata cache hit with the bytes it serves, never as an artifact hit.
func TestNPMStoredRewriteRecordsMetadataHit(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.SetMetadataRewriteCacheSize(1 << 20)

	body := []byte(`{"name":"demo"}`)
	proxy.cacheMetadataBlob(t.Context(), "npm", "hit-demo", metadataStoragePath("npm", "hit-demo"), &upstreamMetadata{body: body})
	key := rewriteCacheKey("npm", "http://proxy", "hit-demo", body)
	if _, err := proxy.rewrites.rewrite(t.Context(), key, body, 0, func(b []byte) ([]byte, error) { return b, nil }); err != nil {
		t.Fatal(err)
	}

	metadataHits := func() float64 {
		return testutil.ToFloat64(metrics.CacheHits.WithLabelValues("npm", metrics.CacheKindMetadata))
	}
	artifactHits := func() float64 {
		return testutil.ToFloat64(metrics.CacheHits.WithLabelValues("npm", metrics.CacheKindArtifact))
	}
	served := func() float64 { return testutil.ToFloat64(metrics.MetadataServedBytes.WithLabelValues("npm")) }
	metadataBefore, artifactBefore, servedBefore := metadataHits(), artifactHits(), served()

	m, ok := proxy.storedRewrite("npm", "hit-demo", "http://proxy", "hit-demo")
	if !ok {
		t.Fatal("expected a stored rewrite")
	}

	if diff := metadataHits() - metadataBefore; diff != 1 {
		t.Errorf("metadata hits delta = %.0f, want 1", diff)
	}
	if diff := artifactHits() - artifactBefore; diff != 0 {
		t.Errorf("artifact hits delta = %.0f, want 0", diff)
	}
	if diff := served() - servedBefore; diff != float64(len(m.content())) {
		t.Errorf("metadata served bytes delta = %.0f, want %d", diff, len(m.content()))
	}
}
