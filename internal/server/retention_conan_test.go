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

const conanRecipeRevision = "b3b71bfe8dd07abc7b82ff2bd0eac021"

// conanFile is an archive as the Conan 2 client requests it and the name
// the handler caches it under.
type conanFile struct {
	path     string
	filename string
}

var (
	conanZlibSources = conanFile{
		"/v2/conans/zlib/1.3.1/_/_/revisions/" + conanRecipeRevision + "/files/conan_sources.tgz",
		"_/_/" + conanRecipeRevision + "/conan_sources.tgz",
	}
	conanZlibPackage = conanFile{
		"/v2/conans/zlib/1.3.1/_/_/revisions/" + conanRecipeRevision + "/packages/0123abc/revisions/4567def/files/conan_package.tgz",
		"_/_/" + conanRecipeRevision + "/packages/0123abc/4567def/conan_package.tgz",
	}
	conanZlibAcme = conanFile{
		"/v2/conans/zlib/1.2.13/acme/stable/revisions/" + conanRecipeRevision + "/files/conan_export.tgz",
		"acme/stable/" + conanRecipeRevision + "/conan_export.tgz",
	}
	conanOpenSSLSources = conanFile{
		"/v2/conans/openssl/3.3.2/_/_/revisions/" + conanRecipeRevision + "/files/conan_sources.tgz",
		"",
	}
)

// conanRetentionUpstream serves the archives the way a Conan 2 server
// does, counting downloads.
func conanRetentionUpstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var downloads atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, f := range []conanFile{conanZlibSources, conanZlibPackage, conanZlibAcme, conanOpenSSLSources} {
			if r.URL.Path == f.path {
				downloads.Add(1)
				_, _ = io.WriteString(w, "conan archive bytes")
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &downloads
}

func newConanRetentionProxy(t *testing.T, upstream *httptest.Server) (*httptest.Server, *database.DB) {
	t.Helper()
	db, store := setupEvictionTest(t)
	// The upstream is a plain-HTTP loopback server; the default fetcher
	// refuses loopback, a stock client reaches it.
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), discardLogger())
	proxy.HTTPClient = upstream.Client()
	h := handler.NewConanHandlerWithUpstream(proxy, "http://proxy.test", upstream.URL)
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)
	return srv, db
}

func conanGet(t *testing.T, srv *httptest.Server, f conanFile) {
	t.Helper()
	resp, err := http.Get(srv.URL + f.path)
	if err != nil {
		t.Fatalf("GET %s: %v", f.path, err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d %s, want 200", f.path, resp.StatusCode, body)
	}
}

// conanArtifact returns the record of the archive of package that the
// handler cached under filename.
func conanArtifact(t *testing.T, db *database.DB, packagePURL, filename string) database.Artifact {
	t.Helper()
	var art database.Artifact
	query := db.Rebind(`SELECT a.* FROM artifacts a JOIN versions v ON v.purl = a.version_purl
		WHERE a.filename = ? AND v.package_purl = ?`)
	if err := db.Get(&art, query, filename, packagePURL); err != nil {
		t.Fatalf("reading %s of %s: %v", filename, packagePURL, err)
	}
	return art
}

func conanBackdateAll(t *testing.T, db *database.DB, fetched, accessed time.Time) {
	t.Helper()
	query := db.Rebind(`UPDATE artifacts SET fetched_at = ?, last_accessed_at = ?`)
	if _, err := db.Exec(query, fetched, accessed); err != nil {
		t.Fatalf("backdating: %v", err)
	}
}

func TestConanRegistersForRetention(t *testing.T) {
	if _, ok := retention.Default.Lookup("conan"); !ok {
		t.Fatal("conan is not registered for retention")
	}
	if key, ok := retention.Default.KeyForDBEcosystem("conan"); !ok || key != "conan" {
		t.Errorf("KeyForDBEcosystem(conan) = %q, %v, want conan, true", key, ok)
	}
}

// A package archive served from the cache stays while it is used, is
// evicted once its rule runs out, comes back from upstream on the next
// download, and stays after that even with the evicted copy's old access
// time on the record.
func TestConanRetentionEvictsAndRefetches(t *testing.T) {
	upstream, downloads := conanRetentionUpstream(t)
	srv, db := newConanRetentionProxy(t, upstream)
	cached := func() database.Artifact {
		return conanArtifact(t, db, "pkg:conan/zlib", conanZlibPackage.filename)
	}

	conanGet(t, srv, conanZlibPackage)
	conanGet(t, srv, conanZlibPackage)
	if got := downloads.Load(); got != 1 {
		t.Fatalf("upstream downloads = %d, want 1", got)
	}
	if !cached().LastAccessedAt.Valid {
		t.Fatal("cache hit did not record an access")
	}

	rules := retention.Rules{Ecosystems: map[string]time.Duration{"conan": retentionDay}}
	now := time.Now()
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if !cached().StoragePath.Valid {
		t.Fatal("archive used today was evicted")
	}

	old := now.Add(-2 * retentionDay)
	conanBackdateAll(t, db, old, old)
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if cached().StoragePath.Valid {
		t.Fatal("archive unused for two days kept under a one-day rule")
	}

	conanGet(t, srv, conanZlibPackage)
	if got := downloads.Load(); got != 2 {
		t.Errorf("upstream downloads = %d, want 2 after the eviction", got)
	}
	conanBackdateAll(t, db, cached().FetchedAt.Time, now.Add(-30*retentionDay))
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())
	if !cached().StoragePath.Valid {
		t.Error("refetched archive evicted again by the next sweep")
	}
}

// A key with a namespace parses as a conan PURL but can never match a
// stored recipe, so it is refused at validation. A plain key is accepted.
func TestConanRetentionRefusesNamespacedKeys(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Retention.Packages = map[string]string{"pkg:conan/zlib": "0"}
	if _, err := cfg.RetentionRules(retention.Default); err != nil {
		t.Fatalf("RetentionRules(pkg:conan/zlib): %v", err)
	}
	cfg.Storage.Retention.Packages = map[string]string{"pkg:conan/acme/zlib": "0"}
	_, err := cfg.RetentionRules(retention.Default)
	if err == nil || !strings.Contains(err.Error(), "does not match how conan packages are stored") {
		t.Errorf("RetentionRules(pkg:conan/acme/zlib) error = %v, want the key refused", err)
	}
}

// A package rule covers a recipe's sources, its binary packages and its
// builds under another user/channel, while other recipes fall under the
// conan rule.
func TestConanRetentionPackageRule(t *testing.T) {
	upstream, _ := conanRetentionUpstream(t)
	srv, db := newConanRetentionProxy(t, upstream)
	zlib := []conanFile{conanZlibSources, conanZlibPackage, conanZlibAcme}
	for _, f := range append(zlib, conanOpenSSLSources) {
		conanGet(t, srv, f)
	}
	old := time.Now().Add(-30 * retentionDay)
	conanBackdateAll(t, db, old, old)

	cfg := config.Default()
	cfg.Storage.Retention.Ecosystems = map[string]string{"conan": "1d"}
	cfg.Storage.Retention.Packages = map[string]string{"pkg:conan/zlib": "0"}
	rules, err := cfg.RetentionRules(retention.Default)
	if err != nil {
		t.Fatalf("RetentionRules: %v", err)
	}
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())

	for _, f := range zlib {
		if !conanArtifact(t, db, "pkg:conan/zlib", f.filename).StoragePath.Valid {
			t.Errorf("%s evicted despite the never-evict rule for zlib", f.filename)
		}
	}
	if conanArtifact(t, db, "pkg:conan/openssl", "_/_/"+conanRecipeRevision+"/conan_sources.tgz").StoragePath.Valid {
		t.Error("openssl kept past the conan rule")
	}
}
