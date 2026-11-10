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
	rpmCurlPath = "/updates/40/Everything/x86_64/Packages/c/curl-8.6.0-10.fc40.x86_64.rpm"
	rpmNMPath   = "/updates/40/Everything/x86_64/Packages/n/NetworkManager-1.46.2-1.fc40.x86_64.rpm"
	rpmCurlFile = "updates/40/Everything/x86_64/Packages/c/curl-8.6.0-10.fc40.x86_64.rpm"
	rpmNMFile   = "updates/40/Everything/x86_64/Packages/n/NetworkManager-1.46.2-1.fc40.x86_64.rpm"
)

// rpmRetentionUpstream serves package files the way a Fedora mirror does,
// counting downloads.
func rpmRetentionUpstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var downloads atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case rpmCurlPath, rpmNMPath:
			downloads.Add(1)
			_, _ = io.WriteString(w, "rpm bytes")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &downloads
}

func newRPMRetentionProxy(t *testing.T, upstream *httptest.Server) (*httptest.Server, *database.DB) {
	t.Helper()
	db, store := setupEvictionTest(t)
	// The upstream is a plain-HTTP loopback server; the default fetcher
	// refuses loopback, a stock client reaches it.
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), discardLogger())
	proxy.HTTPClient = upstream.Client()
	h := handler.NewRPMHandlerWithUpstream(proxy, "http://proxy.test", upstream.URL)
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)
	return srv, db
}

func rpmGet(t *testing.T, url string) {
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

// rpmArtifact returns the record of the package file named filename.
func rpmArtifact(t *testing.T, db *database.DB, filename string) database.Artifact {
	t.Helper()
	var art database.Artifact
	if err := db.Get(&art, db.Rebind(`SELECT * FROM artifacts WHERE filename = ?`), filename); err != nil {
		t.Fatalf("reading artifact %s: %v", filename, err)
	}
	return art
}

func rpmBackdate(t *testing.T, db *database.DB, filename string, fetched, accessed time.Time) {
	t.Helper()
	query := db.Rebind(`UPDATE artifacts SET fetched_at = ?, last_accessed_at = ? WHERE filename = ?`)
	if _, err := db.Exec(query, fetched, accessed, filename); err != nil {
		t.Fatalf("backdating %s: %v", filename, err)
	}
}

func TestRPMRegistersForRetention(t *testing.T) {
	if _, ok := retention.Default.Lookup("rpm"); !ok {
		t.Fatal("rpm is not registered for retention")
	}
	if key, ok := retention.Default.KeyForDBEcosystem("rpm"); !ok || key != "rpm" {
		t.Errorf("KeyForDBEcosystem(rpm) = %q, %v, want rpm, true", key, ok)
	}
}

// A package served from the cache stays while it is used, is evicted once
// its rule runs out, comes back from upstream on the next download, and
// stays after that even with the evicted copy's old access time on the
// record.
func TestRPMRetentionEvictsAndRefetches(t *testing.T) {
	upstream, downloads := rpmRetentionUpstream(t)
	srv, db := newRPMRetentionProxy(t, upstream)

	rpmGet(t, srv.URL+rpmCurlPath)
	rpmGet(t, srv.URL+rpmCurlPath)
	if got := downloads.Load(); got != 1 {
		t.Fatalf("upstream downloads = %d, want 1", got)
	}
	if !rpmArtifact(t, db, rpmCurlFile).LastAccessedAt.Valid {
		t.Fatal("cache hit did not record an access")
	}

	rules := retention.Rules{Ecosystems: map[string]time.Duration{"rpm": retentionDay}}
	now := time.Now()
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if !rpmArtifact(t, db, rpmCurlFile).StoragePath.Valid {
		t.Fatal("package used today was evicted")
	}

	old := now.Add(-2 * retentionDay)
	rpmBackdate(t, db, rpmCurlFile, old, old)
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if rpmArtifact(t, db, rpmCurlFile).StoragePath.Valid {
		t.Fatal("package unused for two days kept under a one-day rule")
	}

	rpmGet(t, srv.URL+rpmCurlPath)
	if got := downloads.Load(); got != 2 {
		t.Errorf("upstream downloads = %d, want 2 after the eviction", got)
	}
	art := rpmArtifact(t, db, rpmCurlFile)
	rpmBackdate(t, db, rpmCurlFile, art.FetchedAt.Time, now.Add(-30*retentionDay))
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())
	if !rpmArtifact(t, db, rpmCurlFile).StoragePath.Valid {
		t.Error("refetched package evicted again by the next sweep")
	}
}

// Keys that parse as rpm PURLs but can never match a stored package are
// refused at validation. Keys with and without a vendor are accepted.
func TestRPMRetentionRefusesUnmatchableKeys(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Retention.Packages = map[string]string{"pkg:rpm/curl": "0", "pkg:rpm/fedora/NetworkManager": "0"}
	if _, err := cfg.RetentionRules(retention.Default); err != nil {
		t.Fatalf("RetentionRules: %v", err)
	}
	cfg.Storage.Retention.Packages = map[string]string{"pkg:rpm/fedora/40/curl": "0"}
	_, err := cfg.RetentionRules(retention.Default)
	if err == nil || !strings.Contains(err.Error(), "does not match how rpm packages are stored") {
		t.Errorf("RetentionRules(pkg:rpm/fedora/40/curl) error = %v, want the key refused", err)
	}
}

// A package rule reaches the package with or without a vendor, and matches
// the name's case: pkg:rpm/networkmanager doesn't cover NetworkManager.
func TestRPMRetentionPackageRules(t *testing.T) {
	upstream, _ := rpmRetentionUpstream(t)
	srv, db := newRPMRetentionProxy(t, upstream)

	tests := []struct {
		key    string
		keepNM bool
	}{
		{"pkg:rpm/NetworkManager", true},
		{"pkg:rpm/fedora/NetworkManager", true},
		{"pkg:rpm/networkmanager", false},
	}
	for _, tt := range tests {
		old := time.Now().Add(-30 * retentionDay)
		for _, path := range []string{rpmCurlPath, rpmNMPath} {
			rpmGet(t, srv.URL+path)
		}
		rpmBackdate(t, db, rpmCurlFile, old, old)
		rpmBackdate(t, db, rpmNMFile, old, old)
		cfg := config.Default()
		cfg.Storage.Retention.Ecosystems = map[string]string{"rpm": "1d"}
		cfg.Storage.Retention.Packages = map[string]string{tt.key: "0"}
		rules, err := cfg.RetentionRules(retention.Default)
		if err != nil {
			t.Fatalf("RetentionRules(%s): %v", tt.key, err)
		}
		sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())

		if got := rpmArtifact(t, db, rpmNMFile).StoragePath.Valid; got != tt.keepNM {
			t.Errorf("with %s: NetworkManager cached = %v, want %v", tt.key, got, tt.keepNM)
		}
		if rpmArtifact(t, db, rpmCurlFile).StoragePath.Valid {
			t.Errorf("with %s: curl kept past the rpm rule", tt.key)
		}
	}
}
