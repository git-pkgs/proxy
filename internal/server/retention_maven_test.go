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
	mavenGuavaJar = "/com/google/guava/guava/32.1.3-jre/guava-32.1.3-jre.jar"
	mavenGuavaPom = "/com/google/guava/guava/32.1.3-jre/guava-32.1.3-jre.pom"
	mavenLang3Jar = "/org/apache/commons/commons-lang3/3.14.0/commons-lang3-3.14.0.jar"
	// mavenPluginJar exists only on the plugin portal, so it is fetched
	// through the handler's fallback.
	mavenPluginJar = "/com/example/plugin/example-plugin/1.0/example-plugin-1.0.jar"
)

// mavenRetentionUpstreams returns a Maven Central stand-in and a plugin
// portal stand-in, counting downloads from either.
func mavenRetentionUpstreams(t *testing.T) (central, portal *httptest.Server, downloads *atomic.Int64) {
	t.Helper()
	downloads = &atomic.Int64{}
	serve := func(paths ...string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for _, p := range paths {
				if r.URL.Path == p {
					downloads.Add(1)
					_, _ = io.WriteString(w, "maven file bytes")
					return
				}
			}
			http.NotFound(w, r)
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	central = serve(mavenGuavaJar, mavenGuavaPom, mavenLang3Jar)
	portal = serve(mavenPluginJar)
	return central, portal, downloads
}

func newMavenRetentionProxy(t *testing.T, central, portal *httptest.Server) (*httptest.Server, *database.DB) {
	t.Helper()
	db, store := setupEvictionTest(t)
	// Both stand-ins are plain-HTTP loopback servers; the default fetcher
	// refuses loopback, a stock client reaches either.
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(central.Client()), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), discardLogger())
	proxy.HTTPClient = central.Client()
	h := handler.NewMavenHandler(proxy, "http://proxy.test", central.URL, portal.URL)
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)
	return srv, db
}

func mavenGet(t *testing.T, url string) {
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

// mavenArtifact returns the record of the file named filename.
func mavenArtifact(t *testing.T, db *database.DB, filename string) database.Artifact {
	t.Helper()
	var art database.Artifact
	if err := db.Get(&art, db.Rebind(`SELECT * FROM artifacts WHERE filename = ?`), filename); err != nil {
		t.Fatalf("reading artifact %s: %v", filename, err)
	}
	return art
}

func mavenBackdate(t *testing.T, db *database.DB, filename string, fetched, accessed time.Time) {
	t.Helper()
	query := db.Rebind(`UPDATE artifacts SET fetched_at = ?, last_accessed_at = ? WHERE filename = ?`)
	if _, err := db.Exec(query, fetched, accessed, filename); err != nil {
		t.Fatalf("backdating %s: %v", filename, err)
	}
}

func TestMavenRegistersForRetention(t *testing.T) {
	if _, ok := retention.Default.Lookup("maven"); !ok {
		t.Fatal("maven is not registered for retention")
	}
	if key, ok := retention.Default.KeyForDBEcosystem("maven"); !ok || key != "maven" {
		t.Errorf("KeyForDBEcosystem(maven) = %q, %v, want maven, true", key, ok)
	}
}

// A Maven file served from the cache stays while it is used, is evicted
// once its rule runs out, comes back from upstream on the next download, and
// stays after that even with the evicted copy's old access time on the
// record. The jar and the pom of one version age independently.
func TestMavenRetentionEvictsAndRefetches(t *testing.T) {
	central, portal, downloads := mavenRetentionUpstreams(t)
	srv, db := newMavenRetentionProxy(t, central, portal)
	const jar, pom = "guava-32.1.3-jre.jar", "guava-32.1.3-jre.pom"

	mavenGet(t, srv.URL+mavenGuavaJar)
	mavenGet(t, srv.URL+mavenGuavaJar)
	mavenGet(t, srv.URL+mavenGuavaPom)
	if got := downloads.Load(); got != 2 {
		t.Fatalf("upstream downloads = %d, want 2 (jar once, pom once)", got)
	}
	if !mavenArtifact(t, db, jar).LastAccessedAt.Valid {
		t.Fatal("cache hit did not record an access")
	}

	rules := retention.Rules{Ecosystems: map[string]time.Duration{"maven": retentionDay}}
	now := time.Now()
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if !mavenArtifact(t, db, jar).StoragePath.Valid {
		t.Fatal("jar used today was evicted")
	}

	// Only the pom goes unused for two days.
	old := now.Add(-2 * retentionDay)
	mavenBackdate(t, db, pom, old, old)
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if mavenArtifact(t, db, pom).StoragePath.Valid {
		t.Fatal("pom unused for two days kept under a one-day rule")
	}
	if !mavenArtifact(t, db, jar).StoragePath.Valid {
		t.Fatal("jar of the same version evicted along with the pom")
	}

	mavenGet(t, srv.URL+mavenGuavaPom)
	if got := downloads.Load(); got != 3 {
		t.Errorf("upstream downloads = %d, want 3 after the pom's eviction", got)
	}
	art := mavenArtifact(t, db, pom)
	mavenBackdate(t, db, pom, art.FetchedAt.Time, now.Add(-30*retentionDay))
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())
	if !mavenArtifact(t, db, pom).StoragePath.Valid {
		t.Error("refetched pom evicted again by the next sweep")
	}
}

// Keys that parse as Maven PURLs but can never match a stored package are
// refused at validation instead of becoming rules that do nothing.
func TestMavenRetentionRefusesUnmatchableKeys(t *testing.T) {
	for _, key := range []string{"pkg:maven/com/google/guava/guava", "pkg:maven/guava"} {
		cfg := config.Default()
		cfg.Storage.Retention.Packages = map[string]string{key: "0"}
		_, err := cfg.RetentionRules(retention.Default)
		if err == nil || !strings.Contains(err.Error(), "does not match how maven packages are stored") {
			t.Errorf("RetentionRules(%s) error = %v, want the key refused", key, err)
		}
	}
}

// A package rule written as a Maven PURL reaches the stored package, and a
// file fetched through the Gradle Plugin Portal fallback falls under the
// maven rule like one from Maven Central.
func TestMavenRetentionPackageRuleAndPluginPortal(t *testing.T) {
	central, portal, _ := mavenRetentionUpstreams(t)
	srv, db := newMavenRetentionProxy(t, central, portal)
	mavenGet(t, srv.URL+mavenGuavaJar)
	mavenGet(t, srv.URL+mavenLang3Jar)
	mavenGet(t, srv.URL+mavenPluginJar)

	var stored string
	jar := mavenArtifact(t, db, "guava-32.1.3-jre.jar")
	if err := db.Get(&stored, db.Rebind(`SELECT package_purl FROM versions WHERE purl = ?`), jar.VersionPURL); err != nil {
		t.Fatalf("reading stored package PURL: %v", err)
	}
	if stored != "pkg:maven/com.google.guava/guava" {
		t.Errorf("stored package PURL = %q, want pkg:maven/com.google.guava/guava", stored)
	}

	old := time.Now().Add(-30 * retentionDay)
	for _, filename := range []string{"guava-32.1.3-jre.jar", "commons-lang3-3.14.0.jar", "example-plugin-1.0.jar"} {
		mavenBackdate(t, db, filename, old, old)
	}
	cfg := config.Default()
	cfg.Storage.Retention.Ecosystems = map[string]string{"maven": "1d"}
	cfg.Storage.Retention.Packages = map[string]string{"pkg:maven/com.google.guava/guava": "0"}
	rules, err := cfg.RetentionRules(retention.Default)
	if err != nil {
		t.Fatalf("RetentionRules: %v", err)
	}
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())

	if !mavenArtifact(t, db, "guava-32.1.3-jre.jar").StoragePath.Valid {
		t.Error("guava evicted despite its never-evict rule")
	}
	if mavenArtifact(t, db, "commons-lang3-3.14.0.jar").StoragePath.Valid {
		t.Error("commons-lang3 kept past the maven rule")
	}
	if mavenArtifact(t, db, "example-plugin-1.0.jar").StoragePath.Valid {
		t.Error("plugin portal artifact kept past the maven rule")
	}
}
