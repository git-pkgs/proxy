package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/git-pkgs/proxy/internal/retention"
	"github.com/git-pkgs/purl"
	"github.com/git-pkgs/registries/fetch"
)

// ociBlob is a blob of one repository, addressed by its digest.
type ociBlob struct {
	repository string
	content    string
}

func (b ociBlob) digest() string {
	sum := sha256.Sum256([]byte(b.content))
	return "sha256:" + hex.EncodeToString(sum[:])
}

var (
	ociNginxBlob = ociBlob{"library/nginx", "nginx layer"}
	ociWgetBlob  = ociBlob{"homebrew/core/wget", "wget bottle"}
	ociAppBlob   = ociBlob{"owner/app", "app layer"}
)

// ociRetentionRegistry serves the given blobs the way a registry does,
// counting downloads.
func ociRetentionRegistry(t *testing.T, downloads *atomic.Int64, blobs ...ociBlob) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, b := range blobs {
			if r.URL.Path == "/v2/"+b.repository+"/blobs/"+b.digest() {
				downloads.Add(1)
				_, _ = io.WriteString(w, b.content)
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newOCIRetentionProxy serves a default registry holding nginx and the
// Homebrew bottle, and a named upstream "ghcr" holding owner/app.
func newOCIRetentionProxy(t *testing.T) (srv *httptest.Server, db *database.DB, ghcrHost string, downloads *atomic.Int64) {
	t.Helper()
	downloads = &atomic.Int64{}
	defaultRegistry := ociRetentionRegistry(t, downloads, ociNginxBlob, ociWgetBlob)
	ghcr := ociRetentionRegistry(t, downloads, ociAppBlob)

	db, store := setupEvictionTest(t)
	// Both registries are plain-HTTP loopback servers; the default fetcher
	// refuses loopback, a stock client reaches either.
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(defaultRegistry.Client()), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), discardLogger())
	proxy.HTTPClient = defaultRegistry.Client()
	h := handler.NewContainerHandlerWithRegistry(proxy, "http://proxy.test", defaultRegistry.URL, map[string]string{"ghcr": ghcr.URL})
	handler.RegisterHomebrewArtifacts(h, defaultRegistry.URL)
	srv = httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)

	u, err := url.Parse(ghcr.URL)
	if err != nil {
		t.Fatal(err)
	}
	return srv, db, u.Host, downloads
}

func ociGet(t *testing.T, url string) {
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

// ociArtifact returns the record of the blob stored under cacheName.
func ociArtifact(t *testing.T, db *database.DB, cacheName string, b ociBlob) database.Artifact {
	t.Helper()
	art, err := db.GetArtifact(purl.MakePURLString("oci", cacheName, b.digest()), b.digest())
	if err != nil || art == nil {
		t.Fatalf("reading blob of %s: %v", cacheName, err)
	}
	return *art
}

func ociBackdate(t *testing.T, db *database.DB, b ociBlob, fetched, accessed time.Time) {
	t.Helper()
	query := db.Rebind(`UPDATE artifacts SET fetched_at = ?, last_accessed_at = ? WHERE filename = ?`)
	if _, err := db.Exec(query, fetched, accessed, b.digest()); err != nil {
		t.Fatalf("backdating %s: %v", b.repository, err)
	}
}

func TestOCIRegistersForRetention(t *testing.T) {
	if _, ok := retention.Default.Lookup("oci"); !ok {
		t.Fatal("oci is not registered for retention")
	}
	if key, ok := retention.Default.KeyForDBEcosystem("oci"); !ok || key != "oci" {
		t.Errorf("KeyForDBEcosystem(oci) = %q, %v, want oci, true", key, ok)
	}
}

// A blob served from the cache stays while it is used, is evicted once its
// rule runs out, comes back from the registry on the next pull, and stays
// after that even with the evicted copy's old access time on the record.
func TestOCIRetentionEvictsAndRefetches(t *testing.T) {
	srv, db, _, downloads := newOCIRetentionProxy(t)
	blobURL := srv.URL + "/library/nginx/blobs/" + ociNginxBlob.digest()

	ociGet(t, blobURL)
	ociGet(t, blobURL)
	if got := downloads.Load(); got != 1 {
		t.Fatalf("registry downloads = %d, want 1", got)
	}
	if !ociArtifact(t, db, "library/nginx", ociNginxBlob).LastAccessedAt.Valid {
		t.Fatal("cache hit did not record an access")
	}

	rules := retention.Rules{Ecosystems: map[string]time.Duration{"oci": retentionDay}}
	now := time.Now()
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if !ociArtifact(t, db, "library/nginx", ociNginxBlob).StoragePath.Valid {
		t.Fatal("blob used today was evicted")
	}

	old := now.Add(-2 * retentionDay)
	ociBackdate(t, db, ociNginxBlob, old, old)
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if ociArtifact(t, db, "library/nginx", ociNginxBlob).StoragePath.Valid {
		t.Fatal("blob unused for two days kept under a one-day rule")
	}

	ociGet(t, blobURL)
	if got := downloads.Load(); got != 2 {
		t.Errorf("registry downloads = %d, want 2 after the eviction", got)
	}
	art := ociArtifact(t, db, "library/nginx", ociNginxBlob)
	ociBackdate(t, db, ociNginxBlob, art.FetchedAt.Time, now.Add(-30*retentionDay))
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())
	if !ociArtifact(t, db, "library/nginx", ociNginxBlob).StoragePath.Valid {
		t.Error("refetched blob evicted again by the next sweep")
	}
}

// Keys that parse as OCI PURLs but name no repository a blob is stored
// under are refused at validation. A path-style key is accepted.
func TestOCIRetentionRefusesUnmatchableKeys(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Retention.Packages = map[string]string{"pkg:oci/library/nginx": "0"}
	if _, err := cfg.RetentionRules(retention.Default); err != nil {
		t.Fatalf("RetentionRules(pkg:oci/library/nginx): %v", err)
	}
	for _, key := range []string{"pkg:oci/upstream/ghcr", "pkg:oci/library%2F%2Fnginx"} {
		cfg.Storage.Retention.Packages = map[string]string{key: "0"}
		_, err := cfg.RetentionRules(retention.Default)
		if err == nil || !strings.Contains(err.Error(), "does not match how oci packages are stored") {
			t.Errorf("RetentionRules(%s) error = %v, want the key refused", key, err)
		}
	}
}

// Package rules reach a Docker Hub repository, a Homebrew bottle and a
// named upstream's repository, the last also when containerd pulls it
// through ns instead of the upstream/ prefix.
func TestOCIRetentionPackageRules(t *testing.T) {
	srv, db, ghcrHost, _ := newOCIRetentionProxy(t)
	pull := func() {
		ociGet(t, srv.URL+"/library/nginx/blobs/"+ociNginxBlob.digest())
		ociGet(t, srv.URL+"/homebrew/core/wget/blobs/"+ociWgetBlob.digest())
		ociGet(t, srv.URL+"/owner/app/blobs/"+ociAppBlob.digest()+"?ns="+ghcrHost)
	}
	pull()

	var stored string
	art := ociArtifact(t, db, "upstream/ghcr/owner/app", ociAppBlob)
	if err := db.Get(&stored, db.Rebind(`SELECT package_purl FROM versions WHERE purl = ?`), art.VersionPURL); err != nil {
		t.Fatalf("reading stored package PURL: %v", err)
	}
	if stored != "pkg:oci/upstream%2Fghcr%2Fowner%2Fapp" {
		t.Errorf("stored package PURL = %q, want pkg:oci/upstream%%2Fghcr%%2Fowner%%2Fapp", stored)
	}

	blobs := []struct {
		cacheName string
		blob      ociBlob
	}{
		{"library/nginx", ociNginxBlob},
		{"homebrew/core/wget", ociWgetBlob},
		{"upstream/ghcr/owner/app", ociAppBlob},
	}
	for _, kept := range []string{"pkg:oci/library/nginx", "pkg:oci/homebrew/core/wget", "pkg:oci/upstream/ghcr/owner/app"} {
		pull()
		old := time.Now().Add(-30 * retentionDay)
		for _, b := range blobs {
			ociBackdate(t, db, b.blob, old, old)
		}
		cfg := config.Default()
		cfg.Storage.Retention.Ecosystems = map[string]string{"oci": "1d"}
		cfg.Storage.Retention.Packages = map[string]string{kept: "0"}
		rules, err := cfg.RetentionRules(retention.Default)
		if err != nil {
			t.Fatalf("RetentionRules(%s): %v", kept, err)
		}
		sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())

		for _, b := range blobs {
			want := kept == "pkg:oci/"+b.cacheName
			if got := ociArtifact(t, db, b.cacheName, b.blob).StoragePath.Valid; got != want {
				t.Errorf("with %s: %s cached = %v, want %v", kept, b.cacheName, got, want)
			}
		}
	}
}
