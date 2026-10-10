package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/retention"
	"github.com/git-pkgs/registries/fetch"
)

const (
	// pypiProxyPrefix is the route the proxy rewrites download links to: it
	// keeps the upstream path, /packages/... included, after /packages.
	pypiProxyPrefix = "/packages"
	pypiWheelPath   = "/packages/ab/cd/0123/Typing_Extensions-4.0.0-py3-none-any.whl"
	pypiSdistPath   = "/packages/ef/01/4567/requests-2.31.0.tar.gz"
)

// pypiRetentionUpstream serves a wheel and an sdist, counting downloads.
func pypiRetentionUpstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var downloads atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pypiWheelPath, pypiSdistPath:
			downloads.Add(1)
			_, _ = io.WriteString(w, "distribution bytes")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	return upstream, &downloads
}

func newPyPIRetentionProxy(t *testing.T, upstream *httptest.Server) (*httptest.Server, *database.DB) {
	t.Helper()
	db, store := setupEvictionTest(t)
	// The upstream's own client reaches the loopback test server, which the
	// default fetcher refuses.
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), discardLogger())
	proxy.HTTPClient = upstream.Client()
	h := handler.NewPyPIHandlerWithUpstreams(proxy, "http://proxy.test", upstream.URL, upstream.URL)
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)
	return srv, db
}

func pypiGet(t *testing.T, url string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d %s, want 200", url, resp.StatusCode, body)
	}
}

// pypiArtifact returns the record of the file named filename.
func pypiArtifact(t *testing.T, db *database.DB, filename string) database.Artifact {
	t.Helper()
	var art database.Artifact
	err := db.Get(&art, db.Rebind(`SELECT * FROM artifacts WHERE filename = ?`), filename)
	if err != nil {
		t.Fatalf("reading artifact %s: %v", filename, err)
	}
	return art
}

func pypiBackdate(t *testing.T, db *database.DB, filename string, fetched, accessed time.Time) {
	t.Helper()
	query := db.Rebind(`UPDATE artifacts SET fetched_at = ?, last_accessed_at = ? WHERE filename = ?`)
	if _, err := db.Exec(query, fetched, accessed, filename); err != nil {
		t.Fatalf("backdating %s: %v", filename, err)
	}
}

func TestPyPIRegistersForRetention(t *testing.T) {
	if _, ok := retention.Default.Lookup("pypi"); !ok {
		t.Fatal("pypi is not registered for retention")
	}
	if key, ok := retention.Default.KeyForDBEcosystem("pypi"); !ok || key != "pypi" {
		t.Errorf("KeyForDBEcosystem(pypi) = %q, %v, want pypi, true", key, ok)
	}
}

// A PyPI file served from the cache stays while it is being used, is
// evicted once its rule runs out, comes back from upstream on the next
// download, and the fresh fetch keeps it even though the evicted copy's old
// access time is still on the record.
func TestPyPIRetentionEvictsAndRefetches(t *testing.T) {
	upstream, downloads := pypiRetentionUpstream(t)
	srv, db := newPyPIRetentionProxy(t, upstream)
	const filename = "requests-2.31.0.tar.gz"

	pypiGet(t, srv.URL+pypiProxyPrefix+pypiSdistPath)
	pypiGet(t, srv.URL+pypiProxyPrefix+pypiSdistPath)
	if got := downloads.Load(); got != 1 {
		t.Fatalf("upstream downloads = %d, want 1 (second from cache)", got)
	}
	if art := pypiArtifact(t, db, filename); !art.LastAccessedAt.Valid {
		t.Fatal("cache hit did not record an access")
	}

	rules := retention.Rules{Ecosystems: map[string]time.Duration{"pypi": retentionDay}}
	now := time.Now()
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if !pypiArtifact(t, db, filename).StoragePath.Valid {
		t.Fatal("file used today was evicted")
	}

	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now.Add(2*retentionDay))
	if pypiArtifact(t, db, filename).StoragePath.Valid {
		t.Fatal("file unused for two days kept under a one-day rule")
	}

	pypiGet(t, srv.URL+pypiProxyPrefix+pypiSdistPath)
	if got := downloads.Load(); got != 2 {
		t.Errorf("upstream downloads = %d, want 2 after eviction", got)
	}
	old := time.Now().Add(-30 * retentionDay)
	art := pypiArtifact(t, db, filename)
	pypiBackdate(t, db, filename, art.FetchedAt.Time, old)
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())
	if !pypiArtifact(t, db, filename).StoragePath.Valid {
		t.Error("refetched file evicted again by the next sweep")
	}
}

// Wheel names use underscores and the publisher's casing; a package rule
// written in any of the spellings PyPI treats as the same project has to
// reach the stored package.
func TestPyPIRetentionPackageRuleMatchesStoredPackage(t *testing.T) {
	upstream, _ := pypiRetentionUpstream(t)
	srv, db := newPyPIRetentionProxy(t, upstream)
	pypiGet(t, srv.URL+pypiProxyPrefix+pypiWheelPath)
	pypiGet(t, srv.URL+pypiProxyPrefix+pypiSdistPath)

	wheel := pypiArtifact(t, db, "Typing_Extensions-4.0.0-py3-none-any.whl")
	var stored string
	if err := db.Get(&stored, db.Rebind(`SELECT package_purl FROM versions WHERE purl = ?`), wheel.VersionPURL); err != nil {
		t.Fatalf("reading stored package PURL: %v", err)
	}

	for _, key := range []string{"pkg:pypi/typing-extensions", "pkg:pypi/Typing_Extensions", "pkg:pypi/typing_extensions"} {
		cfg := config.Default()
		cfg.Storage.Retention.Packages = map[string]string{key: "0"}
		rules, err := cfg.RetentionRules(retention.Default)
		if err != nil {
			t.Fatalf("RetentionRules(%s): %v", key, err)
		}
		if _, ok := rules.Packages[stored]; !ok {
			t.Errorf("rule %s resolved to %v, want the stored package %s", key, rules.Packages, stored)
		}
	}

	// With both files long unused, the "0" rule keeps the wheel while the
	// ecosystem rule evicts the requests sdist.
	old := time.Now().Add(-30 * retentionDay)
	for _, filename := range []string{"Typing_Extensions-4.0.0-py3-none-any.whl", "requests-2.31.0.tar.gz"} {
		pypiBackdate(t, db, filename, old, old)
	}
	cfg := config.Default()
	cfg.Storage.Retention.Ecosystems = map[string]string{"pypi": "1d"}
	cfg.Storage.Retention.Packages = map[string]string{"pkg:pypi/Typing_Extensions": "0"}
	rules, err := cfg.RetentionRules(retention.Default)
	if err != nil {
		t.Fatalf("RetentionRules: %v", err)
	}
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())

	if !pypiArtifact(t, db, "Typing_Extensions-4.0.0-py3-none-any.whl").StoragePath.Valid {
		t.Error("typing-extensions evicted despite its never-evict rule")
	}
	if pypiArtifact(t, db, "requests-2.31.0.tar.gz").StoragePath.Valid {
		t.Error("requests kept past the pypi rule")
	}
	if !strings.HasPrefix(stored, "pkg:pypi/") {
		t.Errorf("stored package PURL = %q, want a pkg:pypi PURL", stored)
	}
}
