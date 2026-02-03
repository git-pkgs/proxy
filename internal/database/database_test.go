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
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		pkg := &Package{
			PURL:        "pkg:npm/lodash",
			Ecosystem:   "npm",
			Name:        "lodash",
			RegistryURL: sql.NullString{String: "https://registry.npmjs.org/lodash", Valid: true},
			Description: sql.NullString{String: "Lodash library", Valid: true},
		}

		err := db.UpsertPackage(pkg)
		if err != nil {
			t.Fatalf("UpsertPackage failed: %v", err)
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
		err = db.UpsertPackage(pkg)
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
	})
}

func TestVersionCRUD(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		pkg := &Package{
			PURL:        "pkg:npm/lodash",
			Ecosystem:   "npm",
			Name:        "lodash",
			RegistryURL: sql.NullString{String: "https://registry.npmjs.org/lodash", Valid: true},
		}
		err := db.UpsertPackage(pkg)
		if err != nil {
			t.Fatalf("UpsertPackage failed: %v", err)
		}

		v := &Version{
			PURL:        "pkg:npm/lodash@4.17.21",
			PackagePURL: "pkg:npm/lodash",
			Integrity:   sql.NullString{String: "sha512-abc123", Valid: true},
		}

		err = db.UpsertVersion(v)
		if err != nil {
			t.Fatalf("UpsertVersion failed: %v", err)
		}

		got, err := db.GetVersionByPURL("pkg:npm/lodash@4.17.21")
		if err != nil {
			t.Fatalf("GetVersionByPURL failed: %v", err)
		}
		if got == nil {
			t.Fatal("expected version, got nil")
		}
		if got.Version() != "4.17.21" {
			t.Errorf("expected version 4.17.21, got %s", got.Version())
		}

		versions, err := db.GetVersionsByPackagePURL("pkg:npm/lodash")
		if err != nil {
			t.Fatalf("GetVersionsByPackagePURL failed: %v", err)
		}
		if len(versions) != 1 {
			t.Errorf("expected 1 version, got %d", len(versions))
		}
	})
}

func TestArtifactCRUD(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		pkg := &Package{
			PURL:        "pkg:npm/lodash",
			Ecosystem:   "npm",
			Name:        "lodash",
			RegistryURL: sql.NullString{String: "https://registry.npmjs.org/lodash", Valid: true},
		}
		_ = db.UpsertPackage(pkg)

		versionPURL := "pkg:npm/lodash@4.17.21"
		v := &Version{
			PURL:        versionPURL,
			PackagePURL: "pkg:npm/lodash",
		}
		_ = db.UpsertVersion(v)

		a := &Artifact{
			VersionPURL: versionPURL,
			Filename:    "lodash-4.17.21.tgz",
			UpstreamURL: "https://registry.npmjs.org/lodash/-/lodash-4.17.21.tgz",
		}

		err := db.UpsertArtifact(a)
		if err != nil {
			t.Fatalf("UpsertArtifact failed: %v", err)
		}

		got, err := db.GetArtifact(versionPURL, "lodash-4.17.21.tgz")
		if err != nil {
			t.Fatalf("GetArtifact failed: %v", err)
		}
		if got == nil {
			t.Fatal("expected artifact, got nil")
		}
		if got.IsCached() {
			t.Error("expected artifact to not be cached yet")
		}

		err = db.MarkArtifactCached(versionPURL, "lodash-4.17.21.tgz", "/cache/npm/lodash-4.17.21.tgz", "sha256-abc", 12345, "application/gzip")
		if err != nil {
			t.Fatalf("MarkArtifactCached failed: %v", err)
		}

		got, err = db.GetArtifact(versionPURL, "lodash-4.17.21.tgz")
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

		err = db.RecordArtifactHit(versionPURL, "lodash-4.17.21.tgz")
		if err != nil {
			t.Fatalf("RecordArtifactHit failed: %v", err)
		}

		got, err = db.GetArtifact(versionPURL, "lodash-4.17.21.tgz")
		if err != nil {
			t.Fatalf("GetArtifact after hit failed: %v", err)
		}
		if got.HitCount != 1 {
			t.Errorf("expected hit count 1, got %d", got.HitCount)
		}
	})
}

func TestCacheManagement(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		pkg := &Package{
			PURL:        "pkg:npm/test",
			Ecosystem:   "npm",
			Name:        "test",
			RegistryURL: sql.NullString{String: "https://registry.npmjs.org/test", Valid: true},
		}
		_ = db.UpsertPackage(pkg)

		for i := 1; i <= 3; i++ {
			versionPURL := "pkg:npm/test@1.0." + string(rune('0'+i))
			v := &Version{
				PURL:        versionPURL,
				PackagePURL: "pkg:npm/test",
			}
			_ = db.UpsertVersion(v)

			a := &Artifact{
				VersionPURL: versionPURL,
				Filename:    "test.tgz",
				UpstreamURL: "https://example.com/test.tgz",
				StoragePath: sql.NullString{String: "/cache/test" + string(rune('0'+i)) + ".tgz", Valid: true},
				Size:        sql.NullInt64{Int64: int64(i * 1000), Valid: true},
				FetchedAt:   sql.NullTime{Time: time.Now(), Valid: true},
			}
			_ = db.UpsertArtifact(a)
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
	})
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
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		stats, err := db.GetCacheStats()
		if err != nil {
			t.Fatalf("GetCacheStats failed: %v", err)
		}
		if stats.TotalPackages != 0 {
			t.Errorf("expected 0 packages, got %d", stats.TotalPackages)
		}

		for _, eco := range []string{"npm", "cargo"} {
			for i := 1; i <= 2; i++ {
				name := eco + "-pkg" + string(rune('0'+i))
				pkgPURL := "pkg:" + eco + "/" + name
				pkg := &Package{
					PURL:        pkgPURL,
					Ecosystem:   eco,
					Name:        name,
					RegistryURL: sql.NullString{String: "https://example.com/" + name, Valid: true},
				}
				_ = db.UpsertPackage(pkg)

				versionPURL := pkgPURL + "@1.0.0"
				v := &Version{
					PURL:        versionPURL,
					PackagePURL: pkgPURL,
				}
				_ = db.UpsertVersion(v)

				a := &Artifact{
					VersionPURL: versionPURL,
					Filename:    name + ".tgz",
					UpstreamURL: "https://example.com/" + name + ".tgz",
					StoragePath: sql.NullString{String: "/cache/" + name + ".tgz", Valid: true},
					Size:        sql.NullInt64{Int64: 1000, Valid: true},
					HitCount:    int64(i),
					FetchedAt:   sql.NullTime{Time: time.Now(), Valid: true},
				}
				_ = db.UpsertArtifact(a)
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
	})
}

func TestGetMostPopularPackages(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		for i := 1; i <= 3; i++ {
			pkgPURL := "pkg:npm/pkg" + string(rune('0'+i))
			pkg := &Package{
				PURL:        pkgPURL,
				Ecosystem:   "npm",
				Name:        "pkg" + string(rune('0'+i)),
				RegistryURL: sql.NullString{String: "https://example.com", Valid: true},
			}
			_ = db.UpsertPackage(pkg)

			versionPURL := pkgPURL + "@1.0.0"
			v := &Version{
				PURL:        versionPURL,
				PackagePURL: pkgPURL,
			}
			_ = db.UpsertVersion(v)

			a := &Artifact{
				VersionPURL: versionPURL,
				Filename:    "test.tgz",
				UpstreamURL: "https://example.com/test.tgz",
				StoragePath: sql.NullString{String: "/cache/test" + string(rune('0'+i)), Valid: true},
				Size:        sql.NullInt64{Int64: int64(i * 100), Valid: true},
				HitCount:    int64(i * 10),
			}
			_ = db.UpsertArtifact(a)
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
	})
}

func TestGetRecentlyCachedPackages(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		now := time.Now()
		for i := 1; i <= 3; i++ {
			pkgPURL := "pkg:npm/recent" + string(rune('0'+i))
			pkg := &Package{
				PURL:        pkgPURL,
				Ecosystem:   "npm",
				Name:        "recent" + string(rune('0'+i)),
				RegistryURL: sql.NullString{String: "https://example.com", Valid: true},
			}
			_ = db.UpsertPackage(pkg)

			versionPURL := pkgPURL + "@1.0.0"
			v := &Version{
				PURL:        versionPURL,
				PackagePURL: pkgPURL,
			}
			_ = db.UpsertVersion(v)

			a := &Artifact{
				VersionPURL: versionPURL,
				Filename:    "test.tgz",
				UpstreamURL: "https://example.com/test.tgz",
				StoragePath: sql.NullString{String: "/cache/recent" + string(rune('0'+i)), Valid: true},
				Size:        sql.NullInt64{Int64: 1000, Valid: true},
				FetchedAt:   sql.NullTime{Time: now.Add(time.Duration(-i) * time.Hour), Valid: true},
			}
			_ = db.UpsertArtifact(a)
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
	})
}

func TestPostgresConnection(t *testing.T) {
	url := os.Getenv("PROXY_DATABASE_URL")
	if url == "" {
		t.Skip("PROXY_DATABASE_URL not set, skipping postgres connection test")
	}

	db, err := OpenPostgresOrCreate(url)
	if err != nil {
		t.Fatalf("OpenPostgresOrCreate failed: %v", err)
	}
	defer func() { _ = db.Close() }()

	if db.Dialect() != DialectPostgres {
		t.Errorf("expected postgres dialect, got %s", db.Dialect())
	}

	version, err := db.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion failed: %v", err)
	}
	if version != SchemaVersion {
		t.Errorf("expected schema version %d, got %d", SchemaVersion, version)
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

func createTestPostgresDB(t *testing.T) *DB {
	t.Helper()
	url := os.Getenv("PROXY_DATABASE_URL")
	if url == "" {
		return nil
	}

	db, err := OpenPostgres(url)
	if err != nil {
		t.Fatalf("OpenPostgres failed: %v", err)
	}

	// Drop and recreate tables for clean test state
	tables := []string{"artifacts", "versions", "packages", "schema_info"}
	for _, table := range tables {
		_, _ = db.Exec("DROP TABLE IF EXISTS " + table + " CASCADE")
	}

	if err := db.CreateSchema(); err != nil {
		_ = db.Close()
		t.Fatalf("CreateSchema failed: %v", err)
	}

	return db
}

func runWithBothDatabases(t *testing.T, testFunc func(t *testing.T, db *DB)) {
	t.Run("sqlite", func(t *testing.T) {
		db := createTestDB(t)
		defer func() { _ = db.Close() }()
		testFunc(t, db)
	})

	t.Run("postgres", func(t *testing.T) {
		db := createTestPostgresDB(t)
		if db == nil {
			t.Skip("PROXY_DATABASE_URL not set, skipping postgres test")
		}
		defer func() { _ = db.Close() }()
		testFunc(t, db)
	})
}

// TestMigrationFromOldSchema tests that we can migrate from an old schema
// that's missing columns like enriched_at, registry_url, etc.
func TestMigrationFromOldSchema(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "old.db")

	// Create a database with old schema (missing new columns)
	sqlDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}

	oldSchema := `
	CREATE TABLE packages (
		id INTEGER PRIMARY KEY,
		purl TEXT NOT NULL,
		ecosystem TEXT NOT NULL,
		name TEXT NOT NULL,
		latest_version TEXT,
		license TEXT,
		description TEXT,
		homepage TEXT,
		repository_url TEXT,
		created_at DATETIME,
		updated_at DATETIME
	);
	CREATE UNIQUE INDEX idx_packages_purl ON packages(purl);

	CREATE TABLE versions (
		id INTEGER PRIMARY KEY,
		purl TEXT NOT NULL,
		package_purl TEXT NOT NULL,
		license TEXT,
		published_at DATETIME,
		created_at DATETIME,
		updated_at DATETIME
	);
	CREATE UNIQUE INDEX idx_versions_purl ON versions(purl);

	CREATE TABLE artifacts (
		id INTEGER PRIMARY KEY,
		version_purl TEXT NOT NULL,
		filename TEXT NOT NULL,
		upstream_url TEXT NOT NULL,
		storage_path TEXT,
		content_hash TEXT,
		size INTEGER,
		content_type TEXT,
		fetched_at DATETIME,
		hit_count INTEGER DEFAULT 0,
		last_accessed_at DATETIME,
		created_at DATETIME,
		updated_at DATETIME
	);
	CREATE UNIQUE INDEX idx_artifacts_version_filename ON artifacts(version_purl, filename);

	CREATE TABLE schema_info (version INTEGER NOT NULL);
	INSERT INTO schema_info (version) VALUES (1);
	`

	if _, err := sqlDB.Exec(oldSchema); err != nil {
		t.Fatalf("failed to create old schema: %v", err)
	}

	// Insert test data
	now := time.Now()
	_, err = sqlDB.Exec(`
		INSERT INTO packages (purl, ecosystem, name, latest_version, license, created_at, updated_at)
		VALUES ('pkg:npm/test-package', 'npm', 'test-package', '1.0.0', 'MIT', ?, ?)
	`, now, now)
	if err != nil {
		t.Fatalf("failed to insert test package: %v", err)
	}

	_, err = sqlDB.Exec(`
		INSERT INTO versions (purl, package_purl, license, created_at, updated_at)
		VALUES ('pkg:npm/test-package@1.0.0', 'pkg:npm/test-package', 'MIT', ?, ?)
	`, now, now)
	if err != nil {
		t.Fatalf("failed to insert test version: %v", err)
	}

	_, err = sqlDB.Exec(`
		INSERT INTO artifacts (version_purl, filename, upstream_url, storage_path, size, fetched_at, hit_count, created_at, updated_at)
		VALUES ('pkg:npm/test-package@1.0.0', 'test-package-1.0.0.tgz', 'https://registry.npmjs.org/test-package/-/test-package-1.0.0.tgz', '/path/to/artifact', 1024, ?, 5, ?, ?)
	`, now, now, now)
	if err != nil {
		t.Fatalf("failed to insert test artifact: %v", err)
	}

	if err := sqlDB.Close(); err != nil {
		t.Fatalf("failed to close database: %v", err)
	}

	// Open with our DB wrapper
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Try to run queries that require new columns - these should fail without migration
	t.Run("queries should fail without migration", func(t *testing.T) {
		_, err := db.GetEnrichmentStats()
		if err == nil {
			t.Error("GetEnrichmentStats: expected error querying enriched_at column, got nil")
		}

		_, err = db.GetPackageByEcosystemName("npm", "test-package")
		if err == nil {
			t.Error("GetPackageByEcosystemName: expected error querying registry_url column, got nil")
		}

		// SearchPackages should work even with old schema because it uses sql.NullString
		// for nullable columns, which can handle NULL values properly
		_, err = db.SearchPackages("test", "", 10, 0)
		if err != nil {
			t.Errorf("SearchPackages: unexpected error with old schema: %v", err)
		}
	})

	// Run migration
	t.Run("migrate schema", func(t *testing.T) {
		if err := db.MigrateSchema(); err != nil {
			t.Fatalf("MigrateSchema failed: %v", err)
		}
	})

	// Verify queries work after migration
	t.Run("queries should work after migration", func(t *testing.T) {
		stats, err := db.GetEnrichmentStats()
		if err != nil {
			t.Errorf("GetEnrichmentStats failed after migration: %v", err)
		}
		if stats == nil {
			t.Error("GetEnrichmentStats returned nil after migration")
		}

		pkg, err := db.GetPackageByEcosystemName("npm", "test-package")
		if err != nil {
			t.Errorf("GetPackageByEcosystemName failed after migration: %v", err)
		}
		if pkg == nil {
			t.Error("GetPackageByEcosystemName returned nil after migration")
		}
		if pkg.Name != "test-package" {
			t.Errorf("expected package name test-package, got %s", pkg.Name)
		}

		// Note: SearchPackages not tested here because old timestamp data
		// stored as strings can't be scanned into time.Time. This is a data
		// migration issue, not a schema migration issue.
	})
}

func TestConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Create(dbPath)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	defer func() { _ = db.Close() }()

	pkg := &Package{
		PURL:      "pkg:npm/test",
		Ecosystem: "npm",
		Name:      "test",
	}
	if err := db.UpsertPackage(pkg); err != nil {
		t.Fatalf("UpsertPackage failed: %v", err)
	}

	ver := &Version{
		PURL:        "pkg:npm/test@1.0.0",
		PackagePURL: pkg.PURL,
	}
	if err := db.UpsertVersion(ver); err != nil {
		t.Fatalf("UpsertVersion failed: %v", err)
	}

	done := make(chan error, 10)
	for i := 0; i < 10; i++ {
		go func() {
			artifact := &Artifact{
				VersionPURL: ver.PURL,
				Filename:    "test.tgz",
				UpstreamURL: "https://example.com/test.tgz",
			}
			if err := db.UpsertArtifact(artifact); err != nil {
				done <- err
				return
			}
			done <- nil
		}()
	}

	for i := 0; i < 10; i++ {
		if err := <-done; err != nil {
			t.Errorf("concurrent write %d failed: %v", i, err)
		}
	}
}

func TestSearchPackagesWithNulls(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Create(dbPath)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	defer func() { _ = db.Close() }()

	pkg := &Package{
		PURL:      "pkg:npm/test-package",
		Ecosystem: "npm",
		Name:      "test-package",
	}
	if err := db.UpsertPackage(pkg); err != nil {
		t.Fatalf("UpsertPackage failed: %v", err)
	}

	ver := &Version{
		PURL:        "pkg:npm/test-package@1.0.0",
		PackagePURL: pkg.PURL,
	}
	if err := db.UpsertVersion(ver); err != nil {
		t.Fatalf("UpsertVersion failed: %v", err)
	}

	artifact := &Artifact{
		VersionPURL: ver.PURL,
		Filename:    "test-package-1.0.0.tgz",
		UpstreamURL: "https://registry.npmjs.org/test-package/-/test-package-1.0.0.tgz",
		StoragePath: sql.NullString{String: "./cache/test.tgz", Valid: true},
		Size:        sql.NullInt64{Int64: 1024, Valid: true},
		FetchedAt:   sql.NullTime{Time: time.Now(), Valid: true},
		HitCount:    5,
	}
	if err := db.UpsertArtifact(artifact); err != nil {
		t.Fatalf("UpsertArtifact failed: %v", err)
	}

	results, err := db.SearchPackages("test", "", 10, 0)
	if err != nil {
		t.Fatalf("SearchPackages failed: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	result := results[0]
	if result.Ecosystem != "npm" {
		t.Errorf("expected ecosystem npm, got %s", result.Ecosystem)
	}
	if result.Name != "test-package" {
		t.Errorf("expected name test-package, got %s", result.Name)
	}
	if result.LatestVersion.Valid {
		t.Errorf("expected LatestVersion to be null, got %s", result.LatestVersion.String)
	}
	if result.License.Valid {
		t.Errorf("expected License to be null, got %s", result.License.String)
	}
	if result.Hits != 5 {
		t.Errorf("expected 5 hits, got %d", result.Hits)
	}
}

func TestSearchPackagesWithValues(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := Create(dbPath)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	defer func() { _ = db.Close() }()

	pkg := &Package{
		PURL:          "pkg:npm/licensed-package",
		Ecosystem:     "npm",
		Name:          "licensed-package",
		LatestVersion: sql.NullString{String: "2.0.0", Valid: true},
		License:       sql.NullString{String: "MIT", Valid: true},
	}
	if err := db.UpsertPackage(pkg); err != nil {
		t.Fatalf("UpsertPackage failed: %v", err)
	}

	ver := &Version{
		PURL:        "pkg:npm/licensed-package@1.0.0",
		PackagePURL: pkg.PURL,
	}
	if err := db.UpsertVersion(ver); err != nil {
		t.Fatalf("UpsertVersion failed: %v", err)
	}

	artifact := &Artifact{
		VersionPURL: ver.PURL,
		Filename:    "licensed-package-1.0.0.tgz",
		UpstreamURL: "https://registry.npmjs.org/licensed-package/-/licensed-package-1.0.0.tgz",
		StoragePath: sql.NullString{String: "./cache/licensed.tgz", Valid: true},
		Size:        sql.NullInt64{Int64: 2048, Valid: true},
		FetchedAt:   sql.NullTime{Time: time.Now(), Valid: true},
		HitCount:    10,
	}
	if err := db.UpsertArtifact(artifact); err != nil {
		t.Fatalf("UpsertArtifact failed: %v", err)
	}

	results, err := db.SearchPackages("licensed", "", 10, 0)
	if err != nil {
		t.Fatalf("SearchPackages failed: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	result := results[0]
	if result.Ecosystem != "npm" {
		t.Errorf("expected ecosystem npm, got %s", result.Ecosystem)
	}
	if result.Name != "licensed-package" {
		t.Errorf("expected name licensed-package, got %s", result.Name)
	}
	if !result.LatestVersion.Valid || result.LatestVersion.String != "2.0.0" {
		t.Errorf("expected LatestVersion 2.0.0, got valid=%v value=%s", result.LatestVersion.Valid, result.LatestVersion.String)
	}
	if !result.License.Valid || result.License.String != "MIT" {
		t.Errorf("expected License MIT, got valid=%v value=%s", result.License.Valid, result.License.String)
	}
	if result.Hits != 10 {
		t.Errorf("expected 10 hits, got %d", result.Hits)
	}
}
