package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
)

func seedCachedArtifact(t *testing.T, db *database.DB, ecosystem, name, version string, size, hits int64) {
	t.Helper()

	pkgPURL := "pkg:" + ecosystem + "/" + name
	versionPURL := pkgPURL + "@" + version

	if err := db.UpsertPackage(&database.Package{PURL: pkgPURL, Ecosystem: ecosystem, Name: name}); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}
	if err := db.UpsertVersion(&database.Version{PURL: versionPURL, PackagePURL: pkgPURL}); err != nil {
		t.Fatalf("UpsertVersion: %v", err)
	}
	if err := db.UpsertArtifact(&database.Artifact{
		VersionPURL: versionPURL,
		Filename:    name + "-" + version + ".tgz",
		UpstreamURL: "https://example.test/" + name,
		StoragePath: sql.NullString{String: "objects/" + name, Valid: true},
		Size:        sql.NullInt64{Int64: size, Valid: true},
		FetchedAt:   sql.NullTime{Time: time.Now(), Valid: true},
		HitCount:    hits,
	}); err != nil {
		t.Fatalf("UpsertArtifact: %v", err)
	}
}

func getStats(t *testing.T, ts *testServer) StatsResponse {
	t.Helper()

	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, httptest.NewRequest("GET", "/stats", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /stats = %d, want 200", w.Code)
	}

	var stats StatsResponse
	if err := json.NewDecoder(w.Body).Decode(&stats); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return stats
}

func TestStatsReportsEcosystemBreakdown(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	seedCachedArtifact(t, ts.db, "npm", "lodash", "4.17.21", 1000, 3)
	seedCachedArtifact(t, ts.db, "cargo", "serde", "1.0.0", 500, 2)

	stats := getStats(t, ts)

	if stats.DownloadedBytes != 4000 {
		t.Errorf("downloaded_bytes = %d, want 4000", stats.DownloadedBytes)
	}
	if stats.Downloads != 5 {
		t.Errorf("downloads = %d, want 5", stats.Downloads)
	}
	if stats.DownloadedBytesHuman == "" {
		t.Error("downloaded is empty; the human-readable total is part of the shape")
	}
	if stats.StatsUnavailable {
		t.Error("stats_unavailable set on a healthy aggregation")
	}

	byEcosystem := make(map[string]EcosystemStatsEntry, len(stats.Ecosystems))
	for _, e := range stats.Ecosystems {
		byEcosystem[e.Ecosystem] = e
	}
	npm, ok := byEcosystem["npm"]
	if !ok {
		t.Fatalf("no npm row in %v", byEcosystem)
	}
	if npm.DownloadedBytes != 3000 || npm.Downloads != 3 || npm.CacheSize != 1000 {
		t.Errorf("npm row = %+v, want 3000 bytes over 3 downloads and 1000 cached", npm)
	}
}

// A failed aggregation with nothing to fall back on must not read as an idle
// proxy: the totals are zero either way, so the flag is the only thing
// telling a consumer which of the two it is looking at.
func TestStatsFlagsUnavailableFigures(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	// GetEcosystemStats counts packages first; the artifact count and cache
	// size the endpoint already reported do not touch that table.
	if _, err := ts.db.Exec(`DROP TABLE packages`); err != nil {
		t.Fatalf("DROP TABLE packages: %v", err)
	}

	stats := getStats(t, ts)

	if !stats.StatsUnavailable {
		t.Error("stats_unavailable not set after the aggregation failed with no snapshot")
	}
	if len(stats.Ecosystems) != 0 {
		t.Errorf("ecosystems = %v, want empty", stats.Ecosystems)
	}
	if stats.TotalSizeHuman == "" {
		t.Error("the rest of the response must still be served")
	}
}

// A retained snapshot is served on with no flag: the figures are stale, not
// absent, and the page is where staleness is surfaced to a human.
func TestStatsServesRetainedSnapshot(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	seedCachedArtifact(t, ts.db, "npm", "lodash", "4.17.21", 1000, 3)
	if got := getStats(t, ts).DownloadedBytes; got != 3000 {
		t.Fatalf("downloaded_bytes = %d, want 3000 before the failure", got)
	}

	if _, err := ts.db.Exec(`DROP TABLE packages`); err != nil {
		t.Fatalf("DROP TABLE packages: %v", err)
	}
	ts.server.ecoStats.lastAt = time.Time{}

	stats := getStats(t, ts)
	if stats.StatsUnavailable {
		t.Error("stats_unavailable set although a snapshot was retained")
	}
	if stats.DownloadedBytes != 3000 {
		t.Errorf("downloaded_bytes = %d, want the retained 3000", stats.DownloadedBytes)
	}
}
