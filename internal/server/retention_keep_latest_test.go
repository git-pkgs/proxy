package server

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/retention"
	"github.com/git-pkgs/proxy/internal/storage"
)

// keepLatestRegistry registers npm with keep_latest support and gem without.
func keepLatestRegistry() *retention.Registry {
	reg := retention.NewRegistry()
	reg.Register(retention.Spec{Key: "npm", CanonicalPackage: retention.DefaultCanonical("npm"), CompareVersions: retention.VersionOrder("npm")})
	reg.Register(retention.Spec{Key: "gem", CanonicalPackage: retention.DefaultCanonical("gem")})
	return reg
}

// seedAgedVersion stores one cached file of pkg:{ecosystem}/{name}@{version},
// fetched and last accessed at the given time.
func seedAgedVersion(t *testing.T, db *database.DB, store storage.Storage, ecosystem, name, version, filename string, at time.Time) {
	t.Helper()
	pkgPURL := "pkg:" + ecosystem + "/" + name
	versionPURL := pkgPURL + "@" + version
	if err := db.UpsertPackage(&database.Package{PURL: pkgPURL, Ecosystem: ecosystem, Name: name}); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}
	if err := db.UpsertVersion(&database.Version{PURL: versionPURL, PackagePURL: pkgPURL}); err != nil {
		t.Fatalf("UpsertVersion: %v", err)
	}
	path := ecosystem + "/" + name + "/" + version + "/" + filename
	size, hash, err := store.Store(context.Background(), path, strings.NewReader("artifact"))
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if err := db.UpsertArtifact(&database.Artifact{
		VersionPURL:    versionPURL,
		Filename:       filename,
		UpstreamURL:    "https://example.com/" + filename,
		StoragePath:    sql.NullString{String: path, Valid: true},
		ContentHash:    sql.NullString{String: hash, Valid: true},
		Size:           sql.NullInt64{Int64: size, Valid: true},
		FetchedAt:      sql.NullTime{Time: at, Valid: true},
		LastAccessedAt: sql.NullTime{Time: at, Valid: true},
	}); err != nil {
		t.Fatalf("UpsertArtifact: %v", err)
	}
}

// cachedVersions returns the versions of pkg:{ecosystem}/{name} that still
// have a cached file, sorted as text.
func cachedVersions(t *testing.T, db *database.DB, ecosystem, name string) []string {
	t.Helper()
	pkgPURL := "pkg:" + ecosystem + "/" + name
	purls, err := db.CachedVersionPURLs(pkgPURL)
	if err != nil {
		t.Fatalf("CachedVersionPURLs: %v", err)
	}
	versions := make([]string, 0, len(purls))
	for _, p := range purls {
		versions = append(versions, strings.TrimPrefix(p, pkgPURL+"@"))
	}
	slices.Sort(versions)
	return versions
}

// With keep_latest, the highest cached versions of a package outlive their
// rule, whichever was fetched first, and the rest are evicted as before.
func TestSweepRetentionKeepsNewestVersions(t *testing.T) {
	db, store := setupEvictionTest(t)
	now := time.Now()
	ago := func(days int) time.Time { return now.Add(-time.Duration(days) * retentionDay) }

	// 1.10.0 is the highest version and the one fetched longest ago.
	seedAgedVersion(t, db, store, "npm", "lib", "1.10.0", "lib-1.10.0.tgz", ago(90))
	seedAgedVersion(t, db, store, "npm", "lib", "1.9.0", "lib-1.9.0.tgz", ago(60))
	seedAgedVersion(t, db, store, "npm", "lib", "1.2.0", "lib-1.2.0.tgz", ago(30))
	seedAgedVersion(t, db, store, "npm", "lib", "1.1.0", "lib-1.1.0.tgz", ago(20))

	rules := retention.Rules{Ecosystems: map[string]time.Duration{"npm": 7 * retentionDay}, KeepLatest: 2}
	sweepRetention(context.Background(), db, discardLogger(), keepLatestRegistry(), rules, now)

	if got, want := cachedVersions(t, db, "npm", "lib"), []string{"1.10.0", "1.9.0"}; !slices.Equal(got, want) {
		t.Errorf("cached versions = %v, want %v", got, want)
	}
}

// A version still within its rule counts as one of the newest, so it takes a
// place an expired version would otherwise keep.
func TestSweepRetentionKeepLatestCountsUnexpiredVersions(t *testing.T) {
	db, store := setupEvictionTest(t)
	now := time.Now()
	ago := func(days int) time.Time { return now.Add(-time.Duration(days) * retentionDay) }

	seedAgedVersion(t, db, store, "npm", "lib", "3.0.0", "lib-3.0.0.tgz", ago(1))
	seedAgedVersion(t, db, store, "npm", "lib", "2.0.0", "lib-2.0.0.tgz", ago(30))
	seedAgedVersion(t, db, store, "npm", "lib", "1.0.0", "lib-1.0.0.tgz", ago(30))

	rules := retention.Rules{Ecosystems: map[string]time.Duration{"npm": 7 * retentionDay}, KeepLatest: 2}
	sweepRetention(context.Background(), db, discardLogger(), keepLatestRegistry(), rules, now)

	if got, want := cachedVersions(t, db, "npm", "lib"), []string{"2.0.0", "3.0.0"}; !slices.Equal(got, want) {
		t.Errorf("cached versions = %v, want %v", got, want)
	}
}

// Every file of a kept version stays, and an older version unused for less
// than the rule is not evicted for being outside the newest.
func TestSweepRetentionKeepLatestKeepsAllFilesOfAVersion(t *testing.T) {
	db, store := setupEvictionTest(t)
	now := time.Now()
	old := now.Add(-30 * retentionDay)

	seedAgedVersion(t, db, store, "npm", "lib", "2.0.0", "lib-2.0.0.tgz", old)
	seedAgedVersion(t, db, store, "npm", "lib", "2.0.0", "lib-2.0.0-extra.tgz", old)
	seedAgedVersion(t, db, store, "npm", "lib", "1.0.0", "lib-1.0.0.tgz", now.Add(-retentionDay))

	rules := retention.Rules{Ecosystems: map[string]time.Duration{"npm": 7 * retentionDay}, KeepLatest: 1}
	sweepRetention(context.Background(), db, discardLogger(), keepLatestRegistry(), rules, now)

	for _, f := range []struct{ version, filename string }{
		{"2.0.0", "lib-2.0.0.tgz"}, {"2.0.0", "lib-2.0.0-extra.tgz"}, {"1.0.0", "lib-1.0.0.tgz"},
	} {
		art, err := db.GetArtifact("pkg:npm/lib@"+f.version, f.filename)
		if err != nil {
			t.Fatalf("GetArtifact(%s): %v", f.filename, err)
		}
		if !art.StoragePath.Valid {
			t.Errorf("%s was evicted", f.filename)
		}
	}
}

// A version evicted earlier no longer counts: the newest versions are the
// newest that are still cached.
func TestSweepRetentionKeepLatestIgnoresUncachedVersions(t *testing.T) {
	db, store := setupEvictionTest(t)
	now := time.Now()
	old := now.Add(-30 * retentionDay)

	seedAgedVersion(t, db, store, "npm", "lib", "3.0.0", "lib-3.0.0.tgz", old)
	seedAgedVersion(t, db, store, "npm", "lib", "2.0.0", "lib-2.0.0.tgz", old)
	seedAgedVersion(t, db, store, "npm", "lib", "1.0.0", "lib-1.0.0.tgz", old)
	if _, err := db.ClearArtifactCache("pkg:npm/lib@3.0.0", "lib-3.0.0.tgz", "npm/lib/3.0.0/lib-3.0.0.tgz"); err != nil {
		t.Fatalf("ClearArtifactCache: %v", err)
	}

	rules := retention.Rules{Ecosystems: map[string]time.Duration{"npm": 7 * retentionDay}, KeepLatest: 1}
	sweepRetention(context.Background(), db, discardLogger(), keepLatestRegistry(), rules, now)

	if got, want := cachedVersions(t, db, "npm", "lib"), []string{"2.0.0"}; !slices.Equal(got, want) {
		t.Errorf("cached versions = %v, want %v", got, want)
	}
}

// keep_latest counts per package, and an ecosystem that does not support it
// is evicted by age alone.
func TestSweepRetentionKeepLatestIsPerPackageAndEcosystem(t *testing.T) {
	db, store := setupEvictionTest(t)
	now := time.Now()
	old := now.Add(-30 * retentionDay)

	seedAgedVersion(t, db, store, "npm", "a", "1.0.0", "a-1.0.0.tgz", old)
	seedAgedVersion(t, db, store, "npm", "a", "2.0.0", "a-2.0.0.tgz", old)
	seedAgedVersion(t, db, store, "npm", "b", "0.1.0", "b-0.1.0.tgz", old)
	seedAgedVersion(t, db, store, "gem", "rails", "7.0.0", "rails-7.0.0.gem", old)
	seedAgedVersion(t, db, store, "gem", "rails", "8.0.0", "rails-8.0.0.gem", old)

	rules := retention.Rules{Default: 7 * retentionDay, KeepLatest: 1}
	sweepRetention(context.Background(), db, discardLogger(), keepLatestRegistry(), rules, now)

	for _, c := range []struct {
		ecosystem, name string
		want            []string
	}{
		{"npm", "a", []string{"2.0.0"}},
		{"npm", "b", []string{"0.1.0"}},
		{"gem", "rails", []string{}},
	} {
		if got := cachedVersions(t, db, c.ecosystem, c.name); !slices.Equal(got, c.want) {
			t.Errorf("%s/%s cached versions = %v, want %v", c.ecosystem, c.name, got, c.want)
		}
	}
}

// Without keep_latest the sweep evicts every expired version, as before.
func TestSweepRetentionWithoutKeepLatestEvictsNewestToo(t *testing.T) {
	db, store := setupEvictionTest(t)
	now := time.Now()
	old := now.Add(-30 * retentionDay)

	seedAgedVersion(t, db, store, "npm", "lib", "2.0.0", "lib-2.0.0.tgz", old)
	seedAgedVersion(t, db, store, "npm", "lib", "1.0.0", "lib-1.0.0.tgz", old)

	rules := retention.Rules{Ecosystems: map[string]time.Duration{"npm": 7 * retentionDay}}
	sweepRetention(context.Background(), db, discardLogger(), keepLatestRegistry(), rules, now)

	if got := cachedVersions(t, db, "npm", "lib"); len(got) != 0 {
		t.Errorf("cached versions = %v, want none", got)
	}
}

// The ecosystems whose handlers store plain release versions support
// keep_latest, and so does oci, which keeps by image.
func TestKeepLatestEcosystems(t *testing.T) {
	want := []string{"cargo", "golang", "npm", "oci", "pypi"}
	if got := retention.Default.KeepLatestKeys(); !slices.Equal(got, want) {
		t.Errorf("KeepLatestKeys() = %v, want %v", got, want)
	}
}
