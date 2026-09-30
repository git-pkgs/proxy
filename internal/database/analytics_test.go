package database

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/git-pkgs/purl"
)

// seedArtifact inserts a package, a version and a cached artifact so that the
// ecosystem aggregation has a complete join path to walk.
func seedArtifact(t *testing.T, db *DB, ecosystem, name, version string, size, hits int64) {
	t.Helper()

	pkgPURL := "pkg:" + ecosystem + "/" + name
	versionPURL := pkgPURL + "@" + version

	if err := db.UpsertPackage(&Package{PURL: pkgPURL, Ecosystem: ecosystem, Name: name}); err != nil {
		t.Fatalf("UpsertPackage(%s): %v", pkgPURL, err)
	}
	if err := db.UpsertVersion(&Version{PURL: versionPURL, PackagePURL: pkgPURL}); err != nil {
		t.Fatalf("UpsertVersion(%s): %v", versionPURL, err)
	}
	if err := db.UpsertArtifact(&Artifact{
		VersionPURL: versionPURL,
		Filename:    name + "-" + version + ".tgz",
		UpstreamURL: "https://example.test/" + name,
		StoragePath: sql.NullString{String: "objects/" + name, Valid: true},
		Size:        sql.NullInt64{Int64: size, Valid: true},
		FetchedAt:   sql.NullTime{Time: time.Now(), Valid: true},
		HitCount:    hits,
	}); err != nil {
		t.Fatalf("UpsertArtifact(%s): %v", versionPURL, err)
	}
}

func newTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Create(filepath.Join(t.TempDir(), "analytics.db"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestGetEcosystemStats(t *testing.T) {
	db := newTestDB(t)

	// npm: 2 artifacts across 2 packages, 1000*3 + 500*1 = 3500 bytes served.
	seedArtifact(t, db, "npm", "lodash", "4.17.21", 1000, 3)
	seedArtifact(t, db, "npm", "express", "4.18.2", 500, 1)
	// cargo: a single heavily-hit artifact, 200*10 = 2000 bytes served.
	seedArtifact(t, db, "cargo", "serde", "1.0.0", 200, 10)

	stats, err := db.GetEcosystemStats()
	if err != nil {
		t.Fatalf("GetEcosystemStats: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("expected 2 ecosystems, got %d: %+v", len(stats), stats)
	}

	// Ordered by accumulated download volume, so npm (3500) precedes cargo (2000).
	npm, cargo := stats[0], stats[1]
	if npm.Ecosystem != "npm" || cargo.Ecosystem != "cargo" {
		t.Fatalf("expected npm then cargo, got %q then %q", npm.Ecosystem, cargo.Ecosystem)
	}

	if npm.DownloadedBytes != 3500 {
		t.Errorf("npm DownloadedBytes = %d, want 3500", npm.DownloadedBytes)
	}
	if npm.Downloads != 4 {
		t.Errorf("npm Downloads = %d, want 4", npm.Downloads)
	}
	if npm.CacheSize != 1500 {
		t.Errorf("npm CacheSize = %d, want 1500", npm.CacheSize)
	}
	if npm.Artifacts != 2 {
		t.Errorf("npm Artifacts = %d, want 2", npm.Artifacts)
	}
	if npm.Packages != 2 {
		t.Errorf("npm Packages = %d, want 2", npm.Packages)
	}
	if npm.Versions != 2 {
		t.Errorf("npm Versions = %d, want 2", npm.Versions)
	}

	if cargo.DownloadedBytes != 2000 {
		t.Errorf("cargo DownloadedBytes = %d, want 2000", cargo.DownloadedBytes)
	}
	if cargo.Downloads != 10 {
		t.Errorf("cargo Downloads = %d, want 10", cargo.Downloads)
	}
}

func TestGetEcosystemStatsEmptyDatabase(t *testing.T) {
	stats, err := newTestDB(t).GetEcosystemStats()
	if err != nil {
		t.Fatalf("GetEcosystemStats: %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("expected no ecosystems, got %+v", stats)
	}
}

// An ecosystem known from metadata but with nothing cached should still appear,
// so the analytics table can show it as idle rather than omitting it.
func TestGetEcosystemStatsIncludesEcosystemsWithoutArtifacts(t *testing.T) {
	db := newTestDB(t)
	seedArtifact(t, db, "npm", "lodash", "4.17.21", 1000, 2)

	if err := db.UpsertPackage(&Package{PURL: "pkg:gem/rails", Ecosystem: "gem", Name: "rails"}); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}

	stats, err := db.GetEcosystemStats()
	if err != nil {
		t.Fatalf("GetEcosystemStats: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("expected 2 ecosystems, got %d: %+v", len(stats), stats)
	}

	// gem has no download volume, so it sorts last.
	gem := stats[1]
	if gem.Ecosystem != "gem" {
		t.Fatalf("expected gem last, got %q", gem.Ecosystem)
	}
	if gem.Packages != 1 {
		t.Errorf("gem Packages = %d, want 1", gem.Packages)
	}
	if gem.Artifacts != 0 || gem.CacheSize != 0 || gem.DownloadedBytes != 0 {
		t.Errorf("expected gem to report no cached artifacts, got %+v", gem)
	}
}

// Eviction clears storage_path and size, which drops the artifact's historical
// hits out of the accumulated total. Pinning that here so the documented
// behaviour of GetEcosystemStats does not drift.
func TestGetEcosystemStatsExcludesEvictedArtifacts(t *testing.T) {
	db := newTestDB(t)
	seedArtifact(t, db, "npm", "lodash", "4.17.21", 1000, 3)
	seedArtifact(t, db, "npm", "express", "4.18.2", 500, 2)

	if err := db.ClearArtifactCache("pkg:npm/lodash@4.17.21", "lodash-4.17.21.tgz"); err != nil {
		t.Fatalf("ClearArtifactCache: %v", err)
	}

	stats, err := db.GetEcosystemStats()
	if err != nil {
		t.Fatalf("GetEcosystemStats: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("expected 1 ecosystem, got %d: %+v", len(stats), stats)
	}

	// Only express remains cached: 500 * 2 = 1000.
	if got := stats[0].DownloadedBytes; got != 1000 {
		t.Errorf("DownloadedBytes = %d, want 1000 (evicted artifact must not count)", got)
	}
	if got := stats[0].Artifacts; got != 1 {
		t.Errorf("Artifacts = %d, want 1", got)
	}
	// The package row survives eviction, so both packages still count.
	if got := stats[0].Packages; got != 2 {
		t.Errorf("Packages = %d, want 2", got)
	}
}

func TestSortEcosystemStatsTieBreaks(t *testing.T) {
	stats := []EcosystemStats{
		{Ecosystem: "zzz"},
		{Ecosystem: "aaa"},
		{Ecosystem: "mid", CacheSize: 10},
		{Ecosystem: "top", DownloadedBytes: 5},
	}
	sortEcosystemStats(stats)

	want := []string{"top", "mid", "aaa", "zzz"}
	for i, name := range want {
		if stats[i].Ecosystem != name {
			t.Errorf("position %d = %q, want %q (full order: %+v)", i, stats[i].Ecosystem, name, stats)
		}
	}
}

// A cached artifact whose version row is missing must still be counted, so the
// per-ecosystem totals reconcile with GetTotalCacheSize rather than quietly
// coming up short. Nothing at the schema level enforces the link.
func TestGetEcosystemStatsCountsUnattributedArtifacts(t *testing.T) {
	db := newTestDB(t)
	seedArtifact(t, db, "npm", "lodash", "4.17.21", 1000, 2)

	// An artifact pointing at a version that was never recorded.
	if err := db.UpsertArtifact(&Artifact{
		VersionPURL: "pkg:npm/orphan@9.9.9",
		Filename:    "orphan-9.9.9.tgz",
		UpstreamURL: "https://example.test/orphan",
		StoragePath: sql.NullString{String: "objects/orphan", Valid: true},
		Size:        sql.NullInt64{Int64: 500, Valid: true},
		FetchedAt:   sql.NullTime{Time: time.Now(), Valid: true},
		HitCount:    4,
	}); err != nil {
		t.Fatalf("UpsertArtifact: %v", err)
	}

	stats, err := db.GetEcosystemStats()
	if err != nil {
		t.Fatalf("GetEcosystemStats: %v", err)
	}

	var cacheSize, artifacts, downloaded int64
	var sawUnattributed bool
	for _, e := range stats {
		cacheSize += e.CacheSize
		artifacts += e.Artifacts
		downloaded += e.DownloadedBytes
		if e.Ecosystem == unattributedEcosystem {
			sawUnattributed = true
		}
	}

	if !sawUnattributed {
		t.Errorf("the orphaned artifact was dropped; got %+v", stats)
	}

	// These must match what the unjoined queries report, or the UI shows two
	// totals that do not add up.
	wantSize, err := db.GetTotalCacheSize()
	if err != nil {
		t.Fatalf("GetTotalCacheSize: %v", err)
	}
	if cacheSize != wantSize {
		t.Errorf("per-ecosystem cache size sums to %d, but GetTotalCacheSize reports %d", cacheSize, wantSize)
	}

	wantCount, err := db.GetCachedArtifactCount()
	if err != nil {
		t.Fatalf("GetCachedArtifactCount: %v", err)
	}
	if artifacts != wantCount {
		t.Errorf("per-ecosystem artifacts sum to %d, but GetCachedArtifactCount reports %d", artifacts, wantCount)
	}

	// 1000*2 from lodash plus 500*4 from the orphan.
	if downloaded != 4000 {
		t.Errorf("downloaded bytes = %d, want 4000", downloaded)
	}
}

// The proxy writes "gem" and git-pkgs writes "rubygems". A database that has
// seen both must report one ecosystem, not two rows splitting its share.
func TestGetEcosystemStatsMergesAliasedEcosystems(t *testing.T) {
	db := newTestDB(t)
	seedArtifact(t, db, "gem", "colorize", "1.1.0", 1000, 3)
	seedArtifact(t, db, "rubygems", "rails", "7.1.0", 4000, 2)
	seedArtifact(t, db, "npm", "lodash", "4.17.21", 500, 1)

	stats, err := db.GetEcosystemStats()
	if err != nil {
		t.Fatalf("GetEcosystemStats: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("expected gem and rubygems merged into one row alongside npm, got %+v", stats)
	}

	var ruby *EcosystemStats
	for i := range stats {
		if purl.NormalizeEcosystem(stats[i].Ecosystem) == "rubygems" {
			ruby = &stats[i]
		}
	}
	if ruby == nil {
		t.Fatal("no row for the rubygems ecosystem")
	}

	// 1000*3 + 4000*2 = 11000
	if ruby.DownloadedBytes != 11000 {
		t.Errorf("DownloadedBytes = %d, want 11000", ruby.DownloadedBytes)
	}
	if ruby.CacheSize != 5000 {
		t.Errorf("CacheSize = %d, want 5000", ruby.CacheSize)
	}
	if ruby.Artifacts != 2 || ruby.Packages != 2 {
		t.Errorf("Artifacts=%d Packages=%d, want 2 and 2", ruby.Artifacts, ruby.Packages)
	}

	// The surviving row keeps the spelling with the most cached bytes, because
	// the UI filters and links by it and the packages table stores it raw.
	if ruby.Ecosystem != "rubygems" {
		t.Errorf("Ecosystem = %q, want the dominant raw spelling %q", ruby.Ecosystem, "rubygems")
	}
}

// A version whose package row is missing must be bucketed like an orphaned
// artifact, so the per-ecosystem figures reconcile with GetCacheStats.
func TestGetEcosystemStatsCountsUnattributedVersions(t *testing.T) {
	db := newTestDB(t)
	seedArtifact(t, db, "npm", "lodash", "4.17.21", 1000, 1)
	if err := db.UpsertVersion(&Version{PURL: "pkg:npm/ghost@1.0.0", PackagePURL: "pkg:npm/ghost"}); err != nil {
		t.Fatalf("UpsertVersion: %v", err)
	}

	stats, err := db.GetEcosystemStats()
	if err != nil {
		t.Fatalf("GetEcosystemStats: %v", err)
	}

	var versions int64
	for _, e := range stats {
		versions += e.Versions
	}

	cacheStats, err := db.GetCacheStats()
	if err != nil {
		t.Fatalf("GetCacheStats: %v", err)
	}
	if versions != cacheStats.TotalVersions {
		t.Errorf("per-ecosystem versions sum to %d, but GetCacheStats reports %d",
			versions, cacheStats.TotalVersions)
	}
}

// The surviving spelling must be the one with the most cached bytes whatever
// order the rows arrive in. GetEcosystemStats builds its input by ranging a
// map, so an implementation that compared against the running total instead of
// each row's own size would pick a different name between refreshes — flipping
// the analytics row's badge and its /ui/packages filter link.
func TestMergeAliasedEcosystemsPicksLargestWhateverTheOrder(t *testing.T) {
	rows := []EcosystemStats{
		{Ecosystem: "gem", CacheSize: 5, Artifacts: 1},
		{Ecosystem: "rubygems", CacheSize: 4, Artifacts: 1},
		{Ecosystem: "RubyGems", CacheSize: 6, Artifacts: 1},
	}

	for _, order := range [][]int{
		{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0},
	} {
		in := make([]EcosystemStats, 0, len(order))
		for _, i := range order {
			in = append(in, rows[i])
		}

		got := mergeAliasedEcosystems(in)
		if len(got) != 1 {
			t.Fatalf("order %v: got %d rows, want 1", order, len(got))
		}
		if got[0].Ecosystem != "RubyGems" {
			t.Errorf("order %v: Ecosystem = %q, want the largest spelling %q",
				order, got[0].Ecosystem, "RubyGems")
		}
		if got[0].CacheSize != 15 {
			t.Errorf("order %v: CacheSize = %d, want 15", order, got[0].CacheSize)
		}
	}
}

// Equal sizes still have to resolve to one answer, for the same reason.
func TestMergeAliasedEcosystemsBreaksSizeTiesByName(t *testing.T) {
	a := []EcosystemStats{{Ecosystem: "gem", CacheSize: 5}, {Ecosystem: "rubygems", CacheSize: 5}}
	b := []EcosystemStats{{Ecosystem: "rubygems", CacheSize: 5}, {Ecosystem: "gem", CacheSize: 5}}

	first := mergeAliasedEcosystems(a)[0].Ecosystem
	second := mergeAliasedEcosystems(b)[0].Ecosystem
	if first != second {
		t.Errorf("input order changed the surviving spelling: %q vs %q", first, second)
	}
}
