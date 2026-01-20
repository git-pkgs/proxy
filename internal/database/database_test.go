package database

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCreateAndOpen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Create(dbPath)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	version, err := db.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion failed: %v", err)
	}
	if version != SchemaVersion {
		t.Errorf("expected schema version %d, got %d", SchemaVersion, version)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	db, err = Open(dbPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer func() { _ = db.Close() }()

	version, err = db.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion after reopen failed: %v", err)
	}
	if version != SchemaVersion {
		t.Errorf("expected schema version %d after reopen, got %d", SchemaVersion, version)
	}
}

func TestOpenOrCreate(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := OpenOrCreate(dbPath)
	if err != nil {
		t.Fatalf("OpenOrCreate (create) failed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	db, err = OpenOrCreate(dbPath)
	if err != nil {
		t.Fatalf("OpenOrCreate (open) failed: %v", err)
	}
	defer func() { _ = db.Close() }()
}

func TestPackageCRUD(t *testing.T) {
	db := createTestDB(t)
	defer func() { _ = db.Close() }()

	pkg := &Package{
		PURL:        "pkg:npm/lodash",
		Ecosystem:   "npm",
		Name:        "lodash",
		UpstreamURL: "https://registry.npmjs.org/lodash",
		Description: sql.NullString{String: "Lodash library", Valid: true},
	}

	id, err := db.UpsertPackage(pkg)
	if err != nil {
		t.Fatalf("UpsertPackage failed: %v", err)
	}
	if id == 0 {
		t.Error("expected non-zero ID")
	}

	got, err := db.GetPackageByPURL("pkg:npm/lodash")
	if err != nil {
		t.Fatalf("GetPackageByPURL failed: %v", err)
	}
	if got == nil {
		t.Fatal("expected package, got nil")
	}
	if got.Name != "lodash" {
		t.Errorf("expected name lodash, got %s", got.Name)
	}
	if got.Description.String != "Lodash library" {
		t.Errorf("expected description 'Lodash library', got %s", got.Description.String)
	}

	got, err = db.GetPackageByEcosystemName("npm", "lodash")
	if err != nil {
		t.Fatalf("GetPackageByEcosystemName failed: %v", err)
	}
	if got == nil {
		t.Fatal("expected package, got nil")
	}

	pkg.Description = sql.NullString{String: "Updated description", Valid: true}
	_, err = db.UpsertPackage(pkg)
	if err != nil {
		t.Fatalf("UpsertPackage (update) failed: %v", err)
	}

	got, err = db.GetPackageByPURL("pkg:npm/lodash")
	if err != nil {
		t.Fatalf("GetPackageByPURL after update failed: %v", err)
	}
	if got.Description.String != "Updated description" {
		t.Errorf("expected updated description, got %s", got.Description.String)
	}
}

func TestVersionCRUD(t *testing.T) {
	db := createTestDB(t)
	defer func() { _ = db.Close() }()

	pkg := &Package{
		PURL:        "pkg:npm/lodash",
		Ecosystem:   "npm",
		Name:        "lodash",
		UpstreamURL: "https://registry.npmjs.org/lodash",
	}
	pkgID, err := db.UpsertPackage(pkg)
	if err != nil {
		t.Fatalf("UpsertPackage failed: %v", err)
	}

	v := &Version{
		PURL:      "pkg:npm/lodash@4.17.21",
		PackageID: pkgID,
		Version:   "4.17.21",
		Integrity: sql.NullString{String: "sha512-abc123", Valid: true},
	}

	versionID, err := db.UpsertVersion(v)
	if err != nil {
		t.Fatalf("UpsertVersion failed: %v", err)
	}
	if versionID == 0 {
		t.Error("expected non-zero version ID")
	}

	got, err := db.GetVersionByPURL("pkg:npm/lodash@4.17.21")
	if err != nil {
		t.Fatalf("GetVersionByPURL failed: %v", err)
	}
	if got == nil {
		t.Fatal("expected version, got nil")
	}
	if got.Version != "4.17.21" {
		t.Errorf("expected version 4.17.21, got %s", got.Version)
	}

	got, err = db.GetVersionByPackageAndVersion(pkgID, "4.17.21")
	if err != nil {
		t.Fatalf("GetVersionByPackageAndVersion failed: %v", err)
	}
	if got == nil {
		t.Fatal("expected version, got nil")
	}

	versions, err := db.GetVersionsByPackageID(pkgID)
	if err != nil {
		t.Fatalf("GetVersionsByPackageID failed: %v", err)
	}
	if len(versions) != 1 {
		t.Errorf("expected 1 version, got %d", len(versions))
	}
}

func TestArtifactCRUD(t *testing.T) {
	db := createTestDB(t)
	defer func() { _ = db.Close() }()

	pkg := &Package{
		PURL:        "pkg:npm/lodash",
		Ecosystem:   "npm",
		Name:        "lodash",
		UpstreamURL: "https://registry.npmjs.org/lodash",
	}
	pkgID, _ := db.UpsertPackage(pkg)

	v := &Version{
		PURL:      "pkg:npm/lodash@4.17.21",
		PackageID: pkgID,
		Version:   "4.17.21",
	}
	versionID, _ := db.UpsertVersion(v)

	a := &Artifact{
		VersionID:   versionID,
		Filename:    "lodash-4.17.21.tgz",
		UpstreamURL: "https://registry.npmjs.org/lodash/-/lodash-4.17.21.tgz",
	}

	artifactID, err := db.UpsertArtifact(a)
	if err != nil {
		t.Fatalf("UpsertArtifact failed: %v", err)
	}
	if artifactID == 0 {
		t.Error("expected non-zero artifact ID")
	}

	got, err := db.GetArtifact(versionID, "lodash-4.17.21.tgz")
	if err != nil {
		t.Fatalf("GetArtifact failed: %v", err)
	}
	if got == nil {
		t.Fatal("expected artifact, got nil")
	}
	if got.IsCached() {
		t.Error("expected artifact to not be cached yet")
	}

	err = db.MarkArtifactCached(artifactID, "/cache/npm/lodash-4.17.21.tgz", "sha256-abc", 12345, "application/gzip")
	if err != nil {
		t.Fatalf("MarkArtifactCached failed: %v", err)
	}

	got, err = db.GetArtifact(versionID, "lodash-4.17.21.tgz")
	if err != nil {
		t.Fatalf("GetArtifact after cache failed: %v", err)
	}
	if !got.IsCached() {
		t.Error("expected artifact to be cached")
	}
	if got.Size.Int64 != 12345 {
		t.Errorf("expected size 12345, got %d", got.Size.Int64)
	}

	got, err = db.GetArtifactByPath("/cache/npm/lodash-4.17.21.tgz")
	if err != nil {
		t.Fatalf("GetArtifactByPath failed: %v", err)
	}
	if got == nil {
		t.Fatal("expected artifact by path, got nil")
	}

	err = db.RecordArtifactHit(artifactID)
	if err != nil {
		t.Fatalf("RecordArtifactHit failed: %v", err)
	}

	got, err = db.GetArtifact(versionID, "lodash-4.17.21.tgz")
	if err != nil {
		t.Fatalf("GetArtifact after hit failed: %v", err)
	}
	if got.HitCount != 1 {
		t.Errorf("expected hit count 1, got %d", got.HitCount)
	}
}

func TestCacheManagement(t *testing.T) {
	db := createTestDB(t)
	defer func() { _ = db.Close() }()

	pkg := &Package{
		PURL:        "pkg:npm/test",
		Ecosystem:   "npm",
		Name:        "test",
		UpstreamURL: "https://registry.npmjs.org/test",
	}
	pkgID, _ := db.UpsertPackage(pkg)

	for i := 1; i <= 3; i++ {
		v := &Version{
			PURL:      "pkg:npm/test@1.0." + string(rune('0'+i)),
			PackageID: pkgID,
			Version:   "1.0." + string(rune('0'+i)),
		}
		vID, _ := db.UpsertVersion(v)

		a := &Artifact{
			VersionID:   vID,
			Filename:    "test.tgz",
			UpstreamURL: "https://example.com/test.tgz",
			StoragePath: sql.NullString{String: "/cache/test" + string(rune('0'+i)) + ".tgz", Valid: true},
			Size:        sql.NullInt64{Int64: int64(i * 1000), Valid: true},
			FetchedAt:   sql.NullTime{Time: time.Now(), Valid: true},
		}
		_, _ = db.UpsertArtifact(a)
	}

	total, err := db.GetTotalCacheSize()
	if err != nil {
		t.Fatalf("GetTotalCacheSize failed: %v", err)
	}
	if total != 6000 {
		t.Errorf("expected total size 6000, got %d", total)
	}

	count, err := db.GetCachedArtifactCount()
	if err != nil {
		t.Fatalf("GetCachedArtifactCount failed: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 cached artifacts, got %d", count)
	}

	lru, err := db.GetLeastRecentlyUsedArtifacts(2)
	if err != nil {
		t.Fatalf("GetLeastRecentlyUsedArtifacts failed: %v", err)
	}
	if len(lru) != 2 {
		t.Errorf("expected 2 LRU artifacts, got %d", len(lru))
	}
}

func createTestDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Create(dbPath)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	return db
}

func TestExists(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	if Exists(dbPath) {
		t.Error("expected file to not exist")
	}

	f, _ := os.Create(dbPath)
	_ = f.Close()

	if !Exists(dbPath) {
		t.Error("expected file to exist")
	}
}

func TestGetCacheStats(t *testing.T) {
	db := createTestDB(t)
	defer func() { _ = db.Close() }()

	// Empty database
	stats, err := db.GetCacheStats()
	if err != nil {
		t.Fatalf("GetCacheStats failed: %v", err)
	}
	if stats.TotalPackages != 0 {
		t.Errorf("expected 0 packages, got %d", stats.TotalPackages)
	}

	// Add some data
	for _, eco := range []string{"npm", "cargo"} {
		for i := 1; i <= 2; i++ {
			name := eco + "-pkg" + string(rune('0'+i))
			pkg := &Package{
				PURL:        "pkg:" + eco + "/" + name,
				Ecosystem:   eco,
				Name:        name,
				UpstreamURL: "https://example.com/" + name,
			}
			pkgID, _ := db.UpsertPackage(pkg)

			v := &Version{
				PURL:      pkg.PURL + "@1.0.0",
				PackageID: pkgID,
				Version:   "1.0.0",
			}
			vID, _ := db.UpsertVersion(v)

			a := &Artifact{
				VersionID:   vID,
				Filename:    name + ".tgz",
				UpstreamURL: "https://example.com/" + name + ".tgz",
				StoragePath: sql.NullString{String: "/cache/" + name + ".tgz", Valid: true},
				Size:        sql.NullInt64{Int64: 1000, Valid: true},
				HitCount:    int64(i),
				FetchedAt:   sql.NullTime{Time: time.Now(), Valid: true},
			}
			_, _ = db.UpsertArtifact(a)
		}
	}

	stats, err = db.GetCacheStats()
	if err != nil {
		t.Fatalf("GetCacheStats failed: %v", err)
	}
	if stats.TotalPackages != 4 {
		t.Errorf("expected 4 packages, got %d", stats.TotalPackages)
	}
	if stats.TotalVersions != 4 {
		t.Errorf("expected 4 versions, got %d", stats.TotalVersions)
	}
	if stats.TotalArtifacts != 4 {
		t.Errorf("expected 4 artifacts, got %d", stats.TotalArtifacts)
	}
	if stats.TotalSize != 4000 {
		t.Errorf("expected size 4000, got %d", stats.TotalSize)
	}
	if stats.TotalHits != 6 {
		t.Errorf("expected 6 hits, got %d", stats.TotalHits)
	}
	if stats.EcosystemCounts["npm"] != 2 {
		t.Errorf("expected 2 npm packages, got %d", stats.EcosystemCounts["npm"])
	}
	if stats.EcosystemCounts["cargo"] != 2 {
		t.Errorf("expected 2 cargo packages, got %d", stats.EcosystemCounts["cargo"])
	}
}

func TestGetMostPopularPackages(t *testing.T) {
	db := createTestDB(t)
	defer func() { _ = db.Close() }()

	// Create packages with different hit counts
	for i := 1; i <= 3; i++ {
		pkg := &Package{
			PURL:        "pkg:npm/pkg" + string(rune('0'+i)),
			Ecosystem:   "npm",
			Name:        "pkg" + string(rune('0'+i)),
			UpstreamURL: "https://example.com",
		}
		pkgID, _ := db.UpsertPackage(pkg)

		v := &Version{
			PURL:      pkg.PURL + "@1.0.0",
			PackageID: pkgID,
			Version:   "1.0.0",
		}
		vID, _ := db.UpsertVersion(v)

		a := &Artifact{
			VersionID:   vID,
			Filename:    "test.tgz",
			UpstreamURL: "https://example.com/test.tgz",
			StoragePath: sql.NullString{String: "/cache/test" + string(rune('0'+i)), Valid: true},
			Size:        sql.NullInt64{Int64: int64(i * 100), Valid: true},
			HitCount:    int64(i * 10),
		}
		_, _ = db.UpsertArtifact(a)
	}

	popular, err := db.GetMostPopularPackages(2)
	if err != nil {
		t.Fatalf("GetMostPopularPackages failed: %v", err)
	}
	if len(popular) != 2 {
		t.Fatalf("expected 2 packages, got %d", len(popular))
	}
	if popular[0].Hits != 30 {
		t.Errorf("expected first package to have 30 hits, got %d", popular[0].Hits)
	}
	if popular[1].Hits != 20 {
		t.Errorf("expected second package to have 20 hits, got %d", popular[1].Hits)
	}
}

func TestGetRecentlyCachedPackages(t *testing.T) {
	db := createTestDB(t)
	defer func() { _ = db.Close() }()

	now := time.Now()
	for i := 1; i <= 3; i++ {
		pkg := &Package{
			PURL:        "pkg:npm/recent" + string(rune('0'+i)),
			Ecosystem:   "npm",
			Name:        "recent" + string(rune('0'+i)),
			UpstreamURL: "https://example.com",
		}
		pkgID, _ := db.UpsertPackage(pkg)

		v := &Version{
			PURL:      pkg.PURL + "@1.0.0",
			PackageID: pkgID,
			Version:   "1.0.0",
		}
		vID, _ := db.UpsertVersion(v)

		a := &Artifact{
			VersionID:   vID,
			Filename:    "test.tgz",
			UpstreamURL: "https://example.com/test.tgz",
			StoragePath: sql.NullString{String: "/cache/recent" + string(rune('0'+i)), Valid: true},
			Size:        sql.NullInt64{Int64: 1000, Valid: true},
			FetchedAt:   sql.NullTime{Time: now.Add(time.Duration(-i) * time.Hour), Valid: true},
		}
		_, _ = db.UpsertArtifact(a)
	}

	recent, err := db.GetRecentlyCachedPackages(2)
	if err != nil {
		t.Fatalf("GetRecentlyCachedPackages failed: %v", err)
	}
	if len(recent) != 2 {
		t.Fatalf("expected 2 packages, got %d", len(recent))
	}
	if recent[0].Name != "recent1" {
		t.Errorf("expected first recent package to be recent1, got %s", recent[0].Name)
	}
}
