package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/retention"
	"github.com/git-pkgs/registries/fetch"
)

const genericReleaseAssetPath = "/jqlang/jq/releases/download/jq-1.7.1/jq-linux-amd64"

func genericRetentionUpstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var downloads atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != genericReleaseAssetPath {
			http.NotFound(w, r)
			return
		}
		downloads.Add(1)
		_, _ = io.WriteString(w, "release asset")
	}))
	t.Cleanup(srv.Close)
	return srv, &downloads
}

func newGenericRetentionProxy(t *testing.T, upstream *httptest.Server) (*httptest.Server, *database.DB) {
	t.Helper()
	db, store := setupEvictionTest(t)
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), discardLogger())
	proxy.HTTPClient = upstream.Client()
	routes := handler.NewGenericHandler(proxy, map[string]string{"github": upstream.URL}).Routes()
	srv := httptest.NewServer(http.StripPrefix("/generic", routes))
	t.Cleanup(srv.Close)
	return srv, db
}

func genericReleaseArtifact(t *testing.T, db *database.DB) database.Artifact {
	t.Helper()
	const versionPURL = "pkg:generic/jqlang%2Fjq@jq-1.7.1"
	var art database.Artifact
	query := db.Rebind(`SELECT * FROM artifacts WHERE version_purl = ?`)
	if err := db.Get(&art, query, versionPURL); err != nil {
		t.Fatalf("reading release artifact %s: %v", versionPURL, err)
	}
	return art
}

func TestGenericRetentionRequiresExplicitRule(t *testing.T) {
	const packagePURL = "pkg:generic/jqlang%2Fjq"
	tests := []struct {
		name       string
		ecosystems map[string]string
		packages   map[string]string
		wantCached bool
	}{
		{"global default keeps generic", nil, nil, true},
		{"ecosystem opts in", map[string]string{"generic": "1d"}, nil, false},
		{"package opts in", nil, map[string]string{packagePURL: "1d"}, false},
		{"other package stays cached", nil, map[string]string{"pkg:generic/other%2Frepo": "1d"}, true},
		{"package disables ecosystem rule", map[string]string{"generic": "1d"}, map[string]string{packagePURL: "0"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream, _ := genericRetentionUpstream(t)
			srv, db := newGenericRetentionProxy(t, upstream)
			getOK(t, srv.URL+"/generic/github"+genericReleaseAssetPath)
			versionPURL := packagePURL + "@jq-1.7.1"
			now := time.Now()
			old := now.Add(-30 * retentionDay)
			backdate(t, db, "fetched_at", versionPURL, old)
			backdate(t, db, "last_accessed_at", versionPURL, old)

			cfg := config.Default()
			cfg.Storage.Retention.Default = "1d"
			cfg.Storage.Retention.Ecosystems = tt.ecosystems
			cfg.Storage.Retention.Packages = tt.packages
			rules, err := cfg.RetentionRules(retention.Default)
			if err != nil {
				t.Fatalf("RetentionRules() error = %v", err)
			}
			sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
			if art := genericReleaseArtifact(t, db); art.StoragePath.Valid != tt.wantCached {
				t.Errorf("release cached = %v, want %v", art.StoragePath.Valid, tt.wantCached)
			}
		})
	}
}

func TestGenericRetentionEvictsAndRefetchesReleaseAsset(t *testing.T) {
	upstream, downloads := genericRetentionUpstream(t)
	srv, db := newGenericRetentionProxy(t, upstream)
	const (
		packagePURL = "pkg:generic/jqlang%2Fjq"
		versionPURL = packagePURL + "@jq-1.7.1"
	)
	assetURL := srv.URL + "/generic/github" + genericReleaseAssetPath

	getOK(t, assetURL)
	getOK(t, assetURL)
	if got := downloads.Load(); got != 1 {
		t.Fatalf("upstream downloads = %d, want 1 (second request from cache)", got)
	}
	art := genericReleaseArtifact(t, db)
	if !art.LastAccessedAt.Valid {
		t.Fatalf("cached release artifact = %+v; want a recorded cache hit", art)
	}

	cfg := config.Default()
	cfg.Storage.Retention.Packages = map[string]string{packagePURL: "1d"}
	rules, err := cfg.RetentionRules(retention.Default)
	if err != nil {
		t.Fatalf("RetentionRules() error = %v", err)
	}
	now := time.Now()
	old := now.Add(-30 * retentionDay)
	backdate(t, db, "fetched_at", versionPURL, old)
	backdate(t, db, "last_accessed_at", versionPURL, now)
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	art = genericReleaseArtifact(t, db)
	if !art.StoragePath.Valid {
		t.Fatalf("recently accessed release was evicted: %+v", art)
	}

	backdate(t, db, "last_accessed_at", versionPURL, old)
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	art = genericReleaseArtifact(t, db)
	if art.StoragePath.Valid {
		t.Fatalf("unused release was not evicted: %+v", art)
	}

	getOK(t, assetURL)
	if got := downloads.Load(); got != 2 {
		t.Errorf("upstream downloads = %d, want 2 after retention eviction", got)
	}
	art = genericReleaseArtifact(t, db)
	if !art.StoragePath.Valid {
		t.Fatalf("refetched release artifact = %+v; want cached artifact", art)
	}
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())
	art = genericReleaseArtifact(t, db)
	if !art.StoragePath.Valid {
		t.Errorf("refetched release was evicted using its old access time: %+v", art)
	}
}
