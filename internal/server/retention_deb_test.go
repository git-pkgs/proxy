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
	debCurlPath    = "/pool/main/c/curl/curl_8.14.1-2_amd64.deb"
	debBusyboxPath = "/pool/main/b/busybox/busybox_1.37.0-6_amd64.deb"
	debCurlFile    = "pool/main/c/curl/curl_8.14.1-2_amd64.deb"
	debBusyboxFile = "pool/main/b/busybox/busybox_1.37.0-6_amd64.deb"
)

// debRetentionUpstream serves package files the way a Debian archive does,
// counting downloads.
func debRetentionUpstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var downloads atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case debCurlPath, debBusyboxPath:
			downloads.Add(1)
			_, _ = io.WriteString(w, "deb bytes")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &downloads
}

// newDebRetentionProxy serves the upstream as the main archive and again as
// a named "security" archive.
func newDebRetentionProxy(t *testing.T, upstream *httptest.Server) (*httptest.Server, *database.DB) {
	t.Helper()
	db, store := setupEvictionTest(t)
	// The upstream is a plain-HTTP loopback server; the default fetcher
	// refuses loopback, a stock client reaches it.
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), discardLogger())
	proxy.HTTPClient = upstream.Client()
	h := handler.NewDebianHandler(proxy, "http://proxy.test", upstream.URL, map[string]string{"security": upstream.URL})
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)
	return srv, db
}

func debGet(t *testing.T, url string) {
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

// debArtifact returns the record of the file cached under filename, which a
// named archive prefixes with its name.
func debArtifact(t *testing.T, db *database.DB, filename string) database.Artifact {
	t.Helper()
	var art database.Artifact
	if err := db.Get(&art, db.Rebind(`SELECT * FROM artifacts WHERE filename = ?`), filename); err != nil {
		t.Fatalf("reading artifact %s: %v", filename, err)
	}
	return art
}

func debBackdate(t *testing.T, db *database.DB, filename string, fetched, accessed time.Time) {
	t.Helper()
	query := db.Rebind(`UPDATE artifacts SET fetched_at = ?, last_accessed_at = ? WHERE filename = ?`)
	if _, err := db.Exec(query, fetched, accessed, filename); err != nil {
		t.Fatalf("backdating %s: %v", filename, err)
	}
}

func TestDebRegistersForRetention(t *testing.T) {
	if _, ok := retention.Default.Lookup("deb"); !ok {
		t.Fatal("deb is not registered for retention")
	}
	if key, ok := retention.Default.KeyForDBEcosystem("deb"); !ok || key != "deb" {
		t.Errorf("KeyForDBEcosystem(deb) = %q, %v, want deb, true", key, ok)
	}
}

// A package served from the cache stays while it is used, is evicted once
// its rule runs out, comes back from upstream on the next download, and
// stays after that even with the evicted copy's old access time on the
// record.
func TestDebRetentionEvictsAndRefetches(t *testing.T) {
	upstream, downloads := debRetentionUpstream(t)
	srv, db := newDebRetentionProxy(t, upstream)

	debGet(t, srv.URL+debCurlPath)
	debGet(t, srv.URL+debCurlPath)
	if got := downloads.Load(); got != 1 {
		t.Fatalf("upstream downloads = %d, want 1", got)
	}
	if !debArtifact(t, db, debCurlFile).LastAccessedAt.Valid {
		t.Fatal("cache hit did not record an access")
	}

	rules := retention.Rules{Ecosystems: map[string]time.Duration{"deb": retentionDay}}
	now := time.Now()
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if !debArtifact(t, db, debCurlFile).StoragePath.Valid {
		t.Fatal("package used today was evicted")
	}

	old := now.Add(-2 * retentionDay)
	debBackdate(t, db, debCurlFile, old, old)
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if debArtifact(t, db, debCurlFile).StoragePath.Valid {
		t.Fatal("package unused for two days kept under a one-day rule")
	}

	debGet(t, srv.URL+debCurlPath)
	if got := downloads.Load(); got != 2 {
		t.Errorf("upstream downloads = %d, want 2 after the eviction", got)
	}
	art := debArtifact(t, db, debCurlFile)
	debBackdate(t, db, debCurlFile, art.FetchedAt.Time, now.Add(-30*retentionDay))
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())
	if !debArtifact(t, db, debCurlFile).StoragePath.Valid {
		t.Error("refetched package evicted again by the next sweep")
	}
}

// Keys that parse as deb PURLs but can never match a stored package are
// refused at validation. Keys with and without a vendor are accepted.
func TestDebRetentionRefusesUnmatchableKeys(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Retention.Packages = map[string]string{"pkg:deb/curl": "0", "pkg:deb/ubuntu/busybox": "0"}
	if _, err := cfg.RetentionRules(retention.Default); err != nil {
		t.Fatalf("RetentionRules: %v", err)
	}
	cfg.Storage.Retention.Packages = map[string]string{"pkg:deb/debian/main/curl": "0"}
	_, err := cfg.RetentionRules(retention.Default)
	if err == nil || !strings.Contains(err.Error(), "does not match how deb packages are stored") {
		t.Errorf("RetentionRules(pkg:deb/debian/main/curl) error = %v, want the key refused", err)
	}
}

// A package rule reaches the package in the main archive and in a named
// one, with or without a vendor.
func TestDebRetentionPackageRules(t *testing.T) {
	upstream, _ := debRetentionUpstream(t)
	srv, db := newDebRetentionProxy(t, upstream)
	files := []struct {
		path, filename string
		kept           bool
	}{
		{debCurlPath, debCurlFile, true},
		{"/security" + debCurlPath, "security/" + debCurlFile, true},
		{debBusyboxPath, debBusyboxFile, false},
	}

	for _, key := range []string{"pkg:deb/curl", "pkg:deb/debian/curl"} {
		old := time.Now().Add(-30 * retentionDay)
		for _, f := range files {
			debGet(t, srv.URL+f.path)
			debBackdate(t, db, f.filename, old, old)
		}
		cfg := config.Default()
		cfg.Storage.Retention.Ecosystems = map[string]string{"deb": "1d"}
		cfg.Storage.Retention.Packages = map[string]string{key: "0"}
		rules, err := cfg.RetentionRules(retention.Default)
		if err != nil {
			t.Fatalf("RetentionRules(%s): %v", key, err)
		}
		sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())

		for _, f := range files {
			if got := debArtifact(t, db, f.filename).StoragePath.Valid; got != f.kept {
				t.Errorf("with %s: %s cached = %v, want %v", key, f.filename, got, f.kept)
			}
		}
	}
}
