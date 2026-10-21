package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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

const (
	golangTestifyZip = "/github.com/stretchr/testify/@v/v1.8.4.zip"
	// golangTOMLZip is github.com/BurntSushi/toml in the escaped form the
	// go command requests it in.
	golangTOMLZip    = "/github.com/!burnt!sushi/toml/@v/v1.3.2.zip"
	golangBackoffZip = "/github.com/cenkalti/backoff/v4/@v/v4.3.0.zip"
)

// golangRetentionUpstream serves module zips the way proxy.golang.org does,
// counting downloads.
func golangRetentionUpstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var downloads atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case golangTestifyZip, golangTOMLZip, golangBackoffZip:
			downloads.Add(1)
			_, _ = io.WriteString(w, "module zip bytes")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &downloads
}

func newGolangRetentionProxy(t *testing.T, upstream *httptest.Server) (*httptest.Server, *database.DB) {
	t.Helper()
	db, store := setupEvictionTest(t)
	// The upstream is a plain-HTTP loopback server; the default fetcher
	// refuses loopback, a stock client reaches it.
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), discardLogger())
	proxy.HTTPClient = upstream.Client()
	h := handler.NewGoHandlerWithUpstream(proxy, "http://proxy.test", upstream.URL)
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)
	return srv, db
}

func golangGet(t *testing.T, url string) {
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

// golangArtifact returns the record of the zip named filename.
// golangFilenameMatch matches a zip's cached filename: the plain name, or,
// for a module path with capitals, the plain name with a hash of the path.
const golangFilenameMatch = `(filename = ? OR filename LIKE ?)`

func golangFilenameArgs(filename string) []any {
	return []any{filename, strings.TrimSuffix(filename, ".zip") + "~%.zip"}
}

func golangArtifact(t *testing.T, db *database.DB, filename string) database.Artifact {
	t.Helper()
	var art database.Artifact
	if err := db.Get(&art, db.Rebind(`SELECT * FROM artifacts WHERE `+golangFilenameMatch), golangFilenameArgs(filename)...); err != nil {
		t.Fatalf("reading artifact %s: %v", filename, err)
	}
	return art
}

func golangBackdate(t *testing.T, db *database.DB, filename string, fetched, accessed time.Time) {
	t.Helper()
	query := db.Rebind(`UPDATE artifacts SET fetched_at = ?, last_accessed_at = ? WHERE ` + golangFilenameMatch)
	if _, err := db.Exec(query, append([]any{fetched, accessed}, golangFilenameArgs(filename)...)...); err != nil {
		t.Fatalf("backdating %s: %v", filename, err)
	}
}

func TestGolangRegistersForRetention(t *testing.T) {
	if _, ok := retention.Default.Lookup("golang"); !ok {
		t.Fatal("golang is not registered for retention")
	}
	if key, ok := retention.Default.KeyForDBEcosystem("golang"); !ok || key != "golang" {
		t.Errorf("KeyForDBEcosystem(golang) = %q, %v, want golang, true", key, ok)
	}
}

// A module zip served from the cache stays while it is used, is evicted
// once its rule runs out, comes back from upstream on the next download, and
// stays after that even with the evicted copy's old access time on the
// record.
func TestGolangRetentionEvictsAndRefetches(t *testing.T) {
	upstream, downloads := golangRetentionUpstream(t)
	srv, db := newGolangRetentionProxy(t, upstream)
	const zip = "testify@v1.8.4.zip"

	golangGet(t, srv.URL+golangTestifyZip)
	golangGet(t, srv.URL+golangTestifyZip)
	if got := downloads.Load(); got != 1 {
		t.Fatalf("upstream downloads = %d, want 1", got)
	}
	if !golangArtifact(t, db, zip).LastAccessedAt.Valid {
		t.Fatal("cache hit did not record an access")
	}

	rules := retention.Rules{Ecosystems: map[string]time.Duration{"golang": retentionDay}}
	now := time.Now()
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if !golangArtifact(t, db, zip).StoragePath.Valid {
		t.Fatal("zip used today was evicted")
	}

	old := now.Add(-2 * retentionDay)
	golangBackdate(t, db, zip, old, old)
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if golangArtifact(t, db, zip).StoragePath.Valid {
		t.Fatal("zip unused for two days kept under a one-day rule")
	}

	golangGet(t, srv.URL+golangTestifyZip)
	if got := downloads.Load(); got != 2 {
		t.Errorf("upstream downloads = %d, want 2 after the eviction", got)
	}
	art := golangArtifact(t, db, zip)
	golangBackdate(t, db, zip, art.FetchedAt.Time, now.Add(-30*retentionDay))
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())
	if !golangArtifact(t, db, zip).StoragePath.Valid {
		t.Error("refetched zip evicted again by the next sweep")
	}
}

// Keys in the escaped form, written out or percent-encoded, parse as Go
// PURLs but can never match a stored module, so they are refused at
// validation instead of becoming rules that do nothing. The same module
// written as in go.mod is accepted.
func TestGolangRetentionRefusesEscapedKeys(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Retention.Packages = map[string]string{"pkg:golang/github.com/BurntSushi/toml": "0"}
	if _, err := cfg.RetentionRules(retention.Default); err != nil {
		t.Fatalf("RetentionRules(pkg:golang/github.com/BurntSushi/toml): %v", err)
	}
	for _, key := range []string{"pkg:golang/github.com/!burnt!sushi/toml", "pkg:golang/github.com/%21burnt%21sushi/toml"} {
		cfg := config.Default()
		cfg.Storage.Retention.Packages = map[string]string{key: "0"}
		_, err := cfg.RetentionRules(retention.Default)
		if err == nil || !strings.Contains(err.Error(), "does not match how golang packages are stored") {
			t.Errorf("RetentionRules(%s) error = %v, want the key refused", key, err)
		}
	}
}

// A package rule reaches a module whose path has capitals, whatever case
// the rule is written in, and a rule for a module does not cover its /v4
// major version.
func TestGolangRetentionPackageRules(t *testing.T) {
	upstream, _ := golangRetentionUpstream(t)
	srv, db := newGolangRetentionProxy(t, upstream)
	golangGet(t, srv.URL+golangTOMLZip)
	golangGet(t, srv.URL+golangTestifyZip)
	golangGet(t, srv.URL+golangBackoffZip)
	const toml, testify, backoff = "toml@v1.3.2.zip", "testify@v1.8.4.zip", "v4@v4.3.0.zip"

	var stored string
	zip := golangArtifact(t, db, toml)
	if err := db.Get(&stored, db.Rebind(`SELECT package_purl FROM versions WHERE purl = ?`), zip.VersionPURL); err != nil {
		t.Fatalf("reading stored package PURL: %v", err)
	}
	if stored != "pkg:golang/github.com/burntsushi/toml" {
		t.Errorf("stored package PURL = %q, want pkg:golang/github.com/burntsushi/toml", stored)
	}

	for _, keys := range []map[string]string{
		{"pkg:golang/github.com/BurntSushi/toml": "0", "pkg:golang/github.com/cenkalti/backoff": "0"},
		{"pkg:golang/github.com/burntsushi/toml": "0", "pkg:golang/github.com/cenkalti/backoff": "0"},
	} {
		old := time.Now().Add(-30 * retentionDay)
		for _, filename := range []string{toml, testify, backoff} {
			golangBackdate(t, db, filename, old, old)
		}
		cfg := config.Default()
		cfg.Storage.Retention.Ecosystems = map[string]string{"golang": "1d"}
		cfg.Storage.Retention.Packages = keys
		rules, err := cfg.RetentionRules(retention.Default)
		if err != nil {
			t.Fatalf("RetentionRules(%v): %v", keys, err)
		}
		sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())

		if !golangArtifact(t, db, toml).StoragePath.Valid {
			t.Errorf("toml evicted despite its never-evict rule %v", keys)
		}
		if golangArtifact(t, db, testify).StoragePath.Valid {
			t.Error("testify kept past the golang rule")
		}
		if golangArtifact(t, db, backoff).StoragePath.Valid {
			t.Error("backoff/v4 kept by the rule for backoff")
		}
		golangGet(t, srv.URL+golangTestifyZip)
		golangGet(t, srv.URL+golangBackoffZip)
	}
}

// golangRewriteTransport sends every request to the test upstream, so the
// mirror command's fixed proxy.golang.org URLs reach it.
type golangRewriteTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (r golangRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = r.target.Scheme
	req.URL.Host = r.target.Host
	req.Host = r.target.Host
	return r.base.RoundTrip(req)
}

// A module preloaded with the mirror command falls under the golang rule.
func TestGolangRetentionCoversMirroredModules(t *testing.T) {
	upstream, downloads := golangRetentionUpstream(t)
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	db, store := setupEvictionTest(t)
	client := &http.Client{Transport: golangRewriteTransport{target: target, base: upstream.Client().Transport}}
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(client), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), discardLogger())

	m := mirror.New(proxy, db, store, discardLogger(), 1)
	progress, err := m.Run(context.Background(), &mirror.PURLSource{PURLs: []string{"pkg:golang/github.com/stretchr/testify@v1.8.4"}})
	if err != nil || progress.Failed != 0 || downloads.Load() != 1 {
		t.Fatalf("mirror run = %+v, %v, downloads %d; want one successful download", progress, err, downloads.Load())
	}

	const zip = "testify@v1.8.4.zip"
	if golangArtifact(t, db, zip).LastAccessedAt.Valid {
		t.Fatal("mirrored module has an access recorded before anyone downloaded it")
	}
	query := db.Rebind(`UPDATE artifacts SET fetched_at = ? WHERE filename = ?`)
	if _, err := db.Exec(query, time.Now().Add(-30*retentionDay), zip); err != nil {
		t.Fatalf("backdating %s: %v", zip, err)
	}
	rules := retention.Rules{Ecosystems: map[string]time.Duration{"golang": retentionDay}}
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())
	if golangArtifact(t, db, zip).StoragePath.Valid {
		t.Error("never-served mirrored module kept past the golang rule")
	}
}
