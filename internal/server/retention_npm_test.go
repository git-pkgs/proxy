package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/mirror"
	"github.com/git-pkgs/proxy/internal/retention"
	"github.com/git-pkgs/registries/fetch"
)

// npmRetentionUpstream serves a packument and a tarball for lodash and
// @babel/core, counting tarball downloads.
func npmRetentionUpstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var tarballs atomic.Int64
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/lodash", "/@babel%2fcore", "/@babel%2Fcore", "/@babel/core":
			name := "lodash"
			tarball := upstream.URL + "/lodash/-/lodash-1.0.0.tgz"
			if r.URL.Path != "/lodash" {
				name = "@babel/core"
				tarball = upstream.URL + "/@babel/core/-/core-1.0.0.tgz"
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"name":"`+name+`","versions":{"1.0.0":{"name":"`+name+
				`","version":"1.0.0","dist":{"tarball":"`+tarball+`"}}}}`)
		case "/lodash/-/lodash-1.0.0.tgz", "/@babel/core/-/core-1.0.0.tgz":
			tarballs.Add(1)
			_, _ = io.WriteString(w, "tarball bytes")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	return upstream, &tarballs
}

func newNPMRetentionProxy(t *testing.T, upstream *httptest.Server) (*httptest.Server, *database.DB) {
	t.Helper()
	db, store := setupEvictionTest(t)
	// The upstream's own client reaches the loopback test server, which the
	// default fetcher refuses.
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), discardLogger())
	proxy.HTTPClient = upstream.Client()
	srv := httptest.NewServer(handler.NewNPMHandler(proxy, "http://proxy.test", upstream.URL).Routes())
	t.Cleanup(srv.Close)
	return srv, db
}

func getOK(t *testing.T, url string) {
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

func TestNPMRegistersForRetention(t *testing.T) {
	if _, ok := retention.Default.Lookup("npm"); !ok {
		t.Fatal("npm is not registered for retention")
	}
	if key, ok := retention.Default.KeyForDBEcosystem("npm"); !ok || key != "npm" {
		t.Errorf("KeyForDBEcosystem(npm) = %q, %v, want npm, true", key, ok)
	}
}

// An npm tarball served from the cache stays while it is being used, is
// evicted once its rule runs out, and comes back from upstream on the next
// download.
func TestNPMRetentionEvictsAndRefetches(t *testing.T) {
	upstream, tarballs := npmRetentionUpstream(t)
	srv, db := newNPMRetentionProxy(t, upstream)
	const versionPURL, filename = "pkg:npm/lodash@1.0.0", "lodash-1.0.0.tgz"
	tarballURL := srv.URL + "/lodash/-/" + filename

	getOK(t, tarballURL)
	getOK(t, tarballURL)
	if got := tarballs.Load(); got != 1 {
		t.Fatalf("upstream tarball downloads = %d, want 1 (second from cache)", got)
	}
	art, err := db.GetArtifact(versionPURL, filename)
	if err != nil || !art.LastAccessedAt.Valid {
		t.Fatalf("cache hit did not record an access: %+v, %v", art, err)
	}
	path := art.StoragePath.String

	rules := retention.Rules{Ecosystems: map[string]time.Duration{"npm": retentionDay}}
	now := time.Now()
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if !isCached(t, db, "npm", "lodash") {
		t.Fatal("artifact used today was evicted")
	}

	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now.Add(2*retentionDay))
	if isCached(t, db, "npm", "lodash") {
		t.Fatal("artifact unused for two days kept under a one-day rule")
	}
	if got := queuedPaths(t, db); len(got) != 1 || got[0] != path {
		t.Errorf("queue = %v, want [%s]", got, path)
	}

	getOK(t, tarballURL)
	if got := tarballs.Load(); got != 2 {
		t.Errorf("upstream tarball downloads = %d, want 2 after eviction", got)
	}
	// The refetched record keeps the access time of the evicted copy; age it
	// past the rule so only the fresh fetch time can keep it.
	backdate(t, db, "last_accessed_at", versionPURL, time.Now().Add(-30*retentionDay))
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())
	if !isCached(t, db, "npm", "lodash") {
		t.Error("refetched artifact evicted again by the next sweep")
	}
}

// backdate sets one timestamp column of an artifact's records.
func backdate(t *testing.T, db *database.DB, column, versionPURL string, at time.Time) {
	t.Helper()
	query := db.Rebind(`UPDATE artifacts SET ` + column + ` = ? WHERE version_purl = ?`)
	if _, err := db.Exec(query, at, versionPURL); err != nil {
		t.Fatalf("backdating %s: %v", column, err)
	}
}

// A package rule written the way people usually write npm PURLs has to name
// the package PURL the handler stores, scoped packages included.
func TestNPMRetentionPackageRuleMatchesStoredPackage(t *testing.T) {
	upstream, _ := npmRetentionUpstream(t)
	srv, db := newNPMRetentionProxy(t, upstream)
	getOK(t, srv.URL+"/@babel/core/-/core-1.0.0.tgz")

	var stored string
	if err := db.Get(&stored, `SELECT package_purl FROM versions WHERE purl LIKE 'pkg:npm/%core@1.0.0'`); err != nil {
		t.Fatalf("reading stored package PURL: %v", err)
	}

	for _, key := range []string{"pkg:npm/@babel/core", "pkg:npm/%40babel/core"} {
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

	// With both packages long unused, the "0" rule keeps @babel/core while
	// the ecosystem rule evicts lodash.
	getOK(t, srv.URL+"/lodash/-/lodash-1.0.0.tgz")
	old := time.Now().Add(-30 * retentionDay)
	for _, versionPURL := range []string{"pkg:npm/lodash@1.0.0", "pkg:npm/%40babel/core@1.0.0"} {
		backdate(t, db, "fetched_at", versionPURL, old)
		backdate(t, db, "last_accessed_at", versionPURL, old)
	}
	cfg := config.Default()
	cfg.Storage.Retention.Ecosystems = map[string]string{"npm": "1d"}
	cfg.Storage.Retention.Packages = map[string]string{"pkg:npm/@babel/core": "0"}
	rules, err := cfg.RetentionRules(retention.Default)
	if err != nil {
		t.Fatalf("RetentionRules: %v", err)
	}
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())

	babel, err := db.GetArtifact("pkg:npm/%40babel/core@1.0.0", "core-1.0.0.tgz")
	if err != nil {
		t.Fatalf("GetArtifact(@babel/core): %v", err)
	}
	if !babel.StoragePath.Valid {
		t.Error("@babel/core evicted despite its never-evict rule")
	}
	if isCached(t, db, "npm", "lodash") {
		t.Error("lodash kept past the npm rule")
	}
}

// rewriteToUpstream sends every request to the test upstream, so the mirror
// command's fixed registry.npmjs.org tarball URLs reach it.
type rewriteToUpstream struct {
	target *url.URL
	base   http.RoundTripper
}

func (r rewriteToUpstream) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = r.target.Scheme
	req.URL.Host = r.target.Host
	req.Host = r.target.Host
	return r.base.RoundTrip(req)
}

// A package preloaded with the mirror command falls under the npm rule:
// the mirror records it under its own ecosystem spelling and filename.
func TestNPMRetentionCoversMirroredPackages(t *testing.T) {
	upstream, tarballs := npmRetentionUpstream(t)
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	db, store := setupEvictionTest(t)
	client := &http.Client{Transport: rewriteToUpstream{target: target, base: upstream.Client().Transport}}
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(client), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), discardLogger())

	m := mirror.New(proxy, db, store, discardLogger(), 1)
	progress, err := m.Run(context.Background(), &mirror.PURLSource{PURLs: []string{"pkg:npm/lodash@1.0.0"}})
	if err != nil || progress.Failed != 0 || tarballs.Load() != 1 {
		t.Fatalf("mirror run = %+v, %v, tarball downloads %d; want one successful download", progress, err, tarballs.Load())
	}

	backdate(t, db, "fetched_at", "pkg:npm/lodash@1.0.0", time.Now().Add(-30*retentionDay))
	rules := retention.Rules{Ecosystems: map[string]time.Duration{"npm": retentionDay}}
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())
	if isCached(t, db, "npm", "lodash") {
		t.Error("never-served mirrored npm artifact kept past its rule")
	}
}
