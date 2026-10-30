package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/denylist"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/retention"
	"github.com/git-pkgs/registries/fetch"
)

// swiftRetentionUpstream serves release metadata and source archives for
// apple/swift-nio 2.65.0 and apple/swift-log 1.5.4 the way a Swift package
// registry does, counting archive downloads.
func swiftRetentionUpstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var downloads atomic.Int64
	archives := map[string]string{
		"/apple/swift-nio/2.65.0": "swift-nio archive",
		"/apple/swift-log/1.5.4":  "swift-log archive",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if archive, ok := archives[strings.TrimSuffix(r.URL.Path, ".zip")]; ok {
			if strings.HasSuffix(r.URL.Path, ".zip") {
				downloads.Add(1)
				w.Header().Set("Content-Type", "application/zip")
				_, _ = io.WriteString(w, archive)
				return
			}
			sum := sha256.Sum256([]byte(archive))
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Version", "1")
			_, _ = fmt.Fprintf(w, `{"resources":[{"name":"source-archive","type":"application/zip","checksum":%q}]}`, hex.EncodeToString(sum[:]))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &downloads
}

func newSwiftRetentionProxy(t *testing.T, upstream *httptest.Server) (*httptest.Server, *database.DB) {
	t.Helper()
	db, store := setupEvictionTest(t)
	// The upstream is a plain-HTTP loopback server; the default fetcher
	// refuses loopback, a stock client reaches it.
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), discardLogger())
	proxy.HTTPClient = upstream.Client()
	h := handler.NewSwiftHandler(proxy, "http://proxy.test", upstream.URL)
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)
	return srv, db
}

func swiftGet(t *testing.T, url string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/vnd.swift.registry.v1+zip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d %s, want 200", url, resp.StatusCode, body)
	}
}

// swiftArtifact returns the record of the archive named filename.
func swiftArtifact(t *testing.T, db *database.DB, filename string) database.Artifact {
	t.Helper()
	var art database.Artifact
	if err := db.Get(&art, db.Rebind(`SELECT * FROM artifacts WHERE filename = ?`), filename); err != nil {
		t.Fatalf("reading artifact %s: %v", filename, err)
	}
	return art
}

func swiftBackdate(t *testing.T, db *database.DB, filename string, fetched, accessed time.Time) {
	t.Helper()
	query := db.Rebind(`UPDATE artifacts SET fetched_at = ?, last_accessed_at = ? WHERE filename = ?`)
	if _, err := db.Exec(query, fetched, accessed, filename); err != nil {
		t.Fatalf("backdating %s: %v", filename, err)
	}
}

func TestSwiftRegistersForRetention(t *testing.T) {
	if _, ok := retention.Default.Lookup("swift"); !ok {
		t.Fatal("swift is not registered for retention")
	}
	if key, ok := retention.Default.KeyForDBEcosystem("swift"); !ok || key != "swift" {
		t.Errorf("KeyForDBEcosystem(swift) = %q, %v, want swift, true", key, ok)
	}
}

// An archive served from the cache stays while it is used, is evicted once
// its rule runs out, comes back from upstream on the next download, and
// stays after that even with the evicted copy's old access time on the
// record.
func TestSwiftRetentionEvictsAndRefetches(t *testing.T) {
	upstream, downloads := swiftRetentionUpstream(t)
	srv, db := newSwiftRetentionProxy(t, upstream)
	const archive = "swift-nio-2.65.0.zip"

	swiftGet(t, srv.URL+"/apple/swift-nio/2.65.0.zip")
	swiftGet(t, srv.URL+"/apple/swift-nio/2.65.0.zip")
	if got := downloads.Load(); got != 1 {
		t.Fatalf("upstream downloads = %d, want 1", got)
	}
	if !swiftArtifact(t, db, archive).LastAccessedAt.Valid {
		t.Fatal("cache hit did not record an access")
	}

	rules := retention.Rules{Ecosystems: map[string]time.Duration{"swift": retentionDay}}
	now := time.Now()
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if !swiftArtifact(t, db, archive).StoragePath.Valid {
		t.Fatal("archive used today was evicted")
	}

	old := now.Add(-2 * retentionDay)
	swiftBackdate(t, db, archive, old, old)
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if swiftArtifact(t, db, archive).StoragePath.Valid {
		t.Fatal("archive unused for two days kept under a one-day rule")
	}

	swiftGet(t, srv.URL+"/apple/swift-nio/2.65.0.zip")
	if got := downloads.Load(); got != 2 {
		t.Errorf("upstream downloads = %d, want 2 after the eviction", got)
	}
	art := swiftArtifact(t, db, archive)
	swiftBackdate(t, db, archive, art.FetchedAt.Time, now.Add(-30*retentionDay))
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())
	if !swiftArtifact(t, db, archive).StoragePath.Valid {
		t.Error("refetched archive evicted again by the next sweep")
	}
}

// A source repository key is accepted at validation and kept pending until
// it is resolved through the upstream. A scope/name key resolves offline.
func TestSwiftRetentionSourceKeysPending(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Retention.Packages = map[string]string{
		"pkg:swift/apple/swift-nio":            "0",
		"pkg:swift/github.com/apple/swift-log": "0",
	}
	rules, err := cfg.RetentionRules(retention.Default)
	if err != nil {
		t.Fatalf("RetentionRules: %v", err)
	}
	if _, ok := rules.Packages["pkg:generic/swift-registry/apple.swift-nio"]; !ok {
		t.Errorf("scope/name key not resolved offline: %v", rules.Packages)
	}
	if _, ok := rules.Sources["swift"]["pkg:swift/github.com/apple/swift-log"]; !ok || !rules.Pending("swift") {
		t.Errorf("source key not pending: %v", rules.Sources)
	}
}

// swiftIdentifiers answers /identifiers for the swift-nio and swift-log
// repositories while up is set, and fails otherwise.
func swiftIdentifiers(up *atomic.Bool, next http.Handler) http.Handler {
	ids := map[string]string{
		"https://github.com/apple/swift-nio": "apple.swift-nio",
		"https://github.com/apple/swift-log": "apple.swift-log",
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/identifiers" {
			next.ServeHTTP(w, r)
			return
		}
		id, ok := ids[r.URL.Query().Get("url")]
		switch {
		case !up.Load():
			http.Error(w, "down", http.StatusBadGateway)
		case !ok:
			http.NotFound(w, r)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"identifiers":[%q]}`, id)
		}
	})
}

// A never-evict rule keyed by the source repository keeps Swift from being
// swept while the upstream can't resolve it, and applies once it can.
func TestSwiftRetentionSourceRuleDefersSweep(t *testing.T) {
	archives, _ := swiftRetentionUpstream(t)
	var up atomic.Bool
	identifiers := httptest.NewServer(swiftIdentifiers(&up, http.NotFoundHandler()))
	t.Cleanup(identifiers.Close)
	srv, db := newSwiftRetentionProxy(t, archives)
	const nio, log = "swift-nio-2.65.0.zip", "swift-log-1.5.4.zip"
	swiftGet(t, srv.URL+"/apple/swift-nio/2.65.0.zip")
	swiftGet(t, srv.URL+"/apple/swift-log/1.5.4.zip")
	old := time.Now().Add(-30 * retentionDay)
	swiftBackdate(t, db, nio, old, old)
	swiftBackdate(t, db, log, old, old)

	cfg := config.Default()
	cfg.Storage.Retention.Ecosystems = map[string]string{"swift": "1d"}
	cfg.Storage.Retention.Packages = map[string]string{"pkg:swift/github.com/apple/swift-nio": "0"}
	rules, err := cfg.RetentionRules(retention.Default)
	if err != nil {
		t.Fatal(err)
	}
	proxy := handler.NewProxy(db, nil, nil, fetch.NewResolver(), discardLogger())
	proxy.HTTPClient = identifiers.Client()
	resolvers := map[string]sourceResolver{
		"swift": swiftRetentionResolver(handler.NewSwiftHandler(proxy, "http://proxy.test", identifiers.URL), retention.Default),
	}

	resolveRetentionSources(context.Background(), discardLogger(), resolvers, rules)
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())
	if !swiftArtifact(t, db, nio).StoragePath.Valid || !swiftArtifact(t, db, log).StoragePath.Valid {
		t.Fatal("swift swept while a package rule was unresolved")
	}

	up.Store(true)
	resolveRetentionSources(context.Background(), discardLogger(), resolvers, rules)
	if rules.Pending("swift") {
		t.Fatalf("rule still pending once the upstream answers: %v", rules.Sources)
	}
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())
	if !swiftArtifact(t, db, nio).StoragePath.Valid {
		t.Error("swift-nio evicted despite its never-evict rule")
	}
	if swiftArtifact(t, db, log).StoragePath.Valid {
		t.Error("swift-log kept past the swift rule")
	}
}

// Denylist entries naming a source repository are resolved to the registry
// identity with their version; one the upstream can't resolve is an error.
func TestResolveDenylistSources(t *testing.T) {
	var up atomic.Bool
	up.Store(true)
	identifiers := httptest.NewServer(swiftIdentifiers(&up, http.NotFoundHandler()))
	t.Cleanup(identifiers.Close)
	db, store := setupEvictionTest(t)
	proxy := handler.NewProxy(db, store, nil, fetch.NewResolver(), discardLogger())
	proxy.HTTPClient = identifiers.Client()
	h := handler.NewSwiftHandler(proxy, "http://proxy.test", identifiers.URL)

	got, err := resolveDenylistSources(context.Background(), h, []string{
		"pkg:npm/left-pad@1.3.0",
		"pkg:swift/github.com/apple/swift-nio@2.62.0",
		"pkg:swift/apple/swift-log@1.5.4",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"pkg:npm/left-pad@1.3.0", "pkg:swift/apple/swift-nio@2.62.0", "pkg:swift/apple/swift-log@1.5.4"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("resolved %v, want %v", got, want)
	}
	policy, err := denylist.New(got)
	if err != nil {
		t.Fatal(err)
	}
	if !policy.Denied("pkg:generic/swift-registry/apple.swift-nio@2.62.0") {
		t.Error("the resolved entry doesn't deny the stored archive")
	}

	for _, entry := range []string{"pkg:swift/github.com/unknown/repo@1.0.0"} {
		if _, err := resolveDenylistSources(context.Background(), h, []string{entry}); err == nil {
			t.Errorf("%s: resolved a repository the upstream doesn't know", entry)
		}
	}
	up.Store(false)
	if _, err := resolveDenylistSources(context.Background(), h, []string{"pkg:swift/github.com/apple/swift-log@1.5.4"}); err == nil {
		t.Error("resolved with the upstream down")
	}
}

// A package rule reaches the stored archive in either documented form and
// whatever case it is written in, even when the client asked in another case.
func TestSwiftRetentionPackageRules(t *testing.T) {
	upstream, _ := swiftRetentionUpstream(t)
	srv, db := newSwiftRetentionProxy(t, upstream)
	const nio, log = "swift-nio-2.65.0.zip", "swift-log-1.5.4.zip"

	swiftGet(t, srv.URL+"/Apple/Swift-NIO/2.65.0.zip")
	var stored string
	art := swiftArtifact(t, db, nio)
	if err := db.Get(&stored, db.Rebind(`SELECT package_purl FROM versions WHERE purl = ?`), art.VersionPURL); err != nil {
		t.Fatalf("reading stored package PURL: %v", err)
	}
	if stored != "pkg:generic/swift-registry/apple.swift-nio" {
		t.Errorf("stored package PURL = %q, want pkg:generic/swift-registry/apple.swift-nio", stored)
	}

	for _, key := range []string{"pkg:swift/Apple/Swift-NIO", "pkg:generic/swift-registry/apple.swift-nio"} {
		swiftGet(t, srv.URL+"/apple/swift-nio/2.65.0.zip")
		swiftGet(t, srv.URL+"/apple/swift-log/1.5.4.zip")
		old := time.Now().Add(-30 * retentionDay)
		swiftBackdate(t, db, nio, old, old)
		swiftBackdate(t, db, log, old, old)

		cfg := config.Default()
		cfg.Storage.Retention.Ecosystems = map[string]string{"swift": "1d"}
		cfg.Storage.Retention.Packages = map[string]string{key: "0"}
		rules, err := cfg.RetentionRules(retention.Default)
		if err != nil {
			t.Fatalf("RetentionRules(%s): %v", key, err)
		}
		sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())

		if !swiftArtifact(t, db, nio).StoragePath.Valid {
			t.Errorf("with %s: swift-nio evicted despite its never-evict rule", key)
		}
		if swiftArtifact(t, db, log).StoragePath.Valid {
			t.Errorf("with %s: swift-log kept past the swift rule", key)
		}
	}
}
