package database

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func setupMetadataCacheDB(t *testing.T) *DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := Create(dbPath)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if err := db.MigrateSchema(); err != nil {
		t.Fatalf("MigrateSchema failed: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestUpsertAndGetMetadataCache(t *testing.T) {
	db := setupMetadataCacheDB(t)

	entry := &MetadataCacheEntry{
		Ecosystem:   testEcosystemNPM,
		Name:        "lodash",
		StoragePath: "_metadata/npm/lodash/metadata",
		ETag:        sql.NullString{String: `"abc123"`, Valid: true},
		Link:        sql.NullString{String: `<https://registry.example.test/next>; rel="next"`, Valid: true},
		ContentType: sql.NullString{String: "application/json", Valid: true},
		ContentDigest: sql.NullString{
			String: "sha256:0123456789abcdef",
			Valid:  true,
		},
		Size:      sql.NullInt64{Int64: 1024, Valid: true},
		FetchedAt: sql.NullTime{Time: time.Now(), Valid: true},
	}

	err := db.UpsertMetadataCache(entry)
	if err != nil {
		t.Fatalf("UpsertMetadataCache() error = %v", err)
	}

	got, err := db.GetMetadataCache(testEcosystemNPM, "lodash")
	if err != nil {
		t.Fatalf("GetMetadataCache() error = %v", err)
	}
	if got == nil {
		t.Fatal("GetMetadataCache() returned nil")
	}

	if got.Ecosystem != testEcosystemNPM {
		t.Errorf("ecosystem = %q, want %q", got.Ecosystem, testEcosystemNPM)
	}
	if got.Name != "lodash" {
		t.Errorf("name = %q, want %q", got.Name, "lodash")
	}
	if got.StoragePath != "_metadata/npm/lodash/metadata" {
		t.Errorf("storage_path = %q, want %q", got.StoragePath, "_metadata/npm/lodash/metadata")
	}
	if !got.ETag.Valid || got.ETag.String != `"abc123"` {
		t.Errorf("etag = %v, want %q", got.ETag, `"abc123"`)
	}
	if !got.Link.Valid || got.Link.String != `<https://registry.example.test/next>; rel="next"` {
		t.Errorf("link = %v, want next link", got.Link)
	}
	if !got.ContentType.Valid || got.ContentType.String != "application/json" {
		t.Errorf("content_type = %v, want %q", got.ContentType, "application/json")
	}
	if !got.ContentDigest.Valid || got.ContentDigest.String != "sha256:0123456789abcdef" {
		t.Errorf("content_digest = %v, want %q", got.ContentDigest, "sha256:0123456789abcdef")
	}
	if !got.Size.Valid || got.Size.Int64 != 1024 {
		t.Errorf("size = %v, want 1024", got.Size)
	}
}

func TestGetMetadataCacheMiss(t *testing.T) {
	db := setupMetadataCacheDB(t)

	got, err := db.GetMetadataCache(testEcosystemNPM, "nonexistent")
	if err != nil {
		t.Fatalf("GetMetadataCache() error = %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for cache miss, got %v", got)
	}
}

func TestUpsertMetadataCacheOverwrite(t *testing.T) {
	db := setupMetadataCacheDB(t)

	// First insert
	entry1 := &MetadataCacheEntry{
		Ecosystem:   testEcosystemNPM,
		Name:        "lodash",
		StoragePath: "_metadata/npm/lodash/metadata",
		ETag:        sql.NullString{String: `"v1"`, Valid: true},
		ContentType: sql.NullString{String: "application/json", Valid: true},
		Size:        sql.NullInt64{Int64: 100, Valid: true},
		FetchedAt:   sql.NullTime{Time: time.Now(), Valid: true},
	}
	if err := db.UpsertMetadataCache(entry1); err != nil {
		t.Fatalf("first UpsertMetadataCache() error = %v", err)
	}

	// Second insert (same ecosystem+name, different etag and size)
	entry2 := &MetadataCacheEntry{
		Ecosystem:   testEcosystemNPM,
		Name:        "lodash",
		StoragePath: "_metadata/npm/lodash/metadata",
		ETag:        sql.NullString{String: `"v2"`, Valid: true},
		ContentType: sql.NullString{String: "application/json", Valid: true},
		Size:        sql.NullInt64{Int64: 200, Valid: true},
		FetchedAt:   sql.NullTime{Time: time.Now(), Valid: true},
	}
	if err := db.UpsertMetadataCache(entry2); err != nil {
		t.Fatalf("second UpsertMetadataCache() error = %v", err)
	}

	got, err := db.GetMetadataCache(testEcosystemNPM, "lodash")
	if err != nil {
		t.Fatalf("GetMetadataCache() error = %v", err)
	}
	if got == nil {
		t.Fatal("expected entry after overwrite")
	}
	if got.ETag.String != `"v2"` {
		t.Errorf("etag = %q, want %q", got.ETag.String, `"v2"`)
	}
	if got.Size.Int64 != 200 {
		t.Errorf("size = %d, want 200", got.Size.Int64)
	}
}

func TestUpsertMetadataCacheNullableFields(t *testing.T) {
	db := setupMetadataCacheDB(t)

	entry := &MetadataCacheEntry{
		Ecosystem:   "pypi",
		Name:        "requests",
		StoragePath: "_metadata/pypi/requests/metadata",
	}

	if err := db.UpsertMetadataCache(entry); err != nil {
		t.Fatalf("UpsertMetadataCache() error = %v", err)
	}

	got, err := db.GetMetadataCache("pypi", "requests")
	if err != nil {
		t.Fatalf("GetMetadataCache() error = %v", err)
	}
	if got == nil {
		t.Fatal("expected entry")
	}
	if got.ETag.Valid {
		t.Error("expected null etag")
	}
	if got.ContentType.Valid {
		t.Error("expected null content_type")
	}
	if got.Link.Valid {
		t.Error("expected null link")
	}
	if got.Size.Valid {
		t.Error("expected null size")
	}
}

func TestMetadataCacheTableCreatedByMigration(t *testing.T) {
	// Create a DB without the metadata_cache table, then migrate
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := Create(dbPath)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	defer func() { _ = db.Close() }()

	// MigrateSchema should create the metadata_cache table
	if err := db.MigrateSchema(); err != nil {
		t.Fatalf("MigrateSchema() error = %v", err)
	}

	has, err := db.HasTable("metadata_cache")
	if err != nil {
		t.Fatalf("HasTable() error = %v", err)
	}
	if !has {
		t.Error("metadata_cache table should exist after migration")
	}
}

func TestMetadataCacheContentDigestMigrationPreservesExistingRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := Create(dbPath)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("ALTER TABLE metadata_cache DROP COLUMN content_digest"); err != nil {
		t.Fatalf("dropping content_digest: %v", err)
	}
	if _, err := db.Exec("DELETE FROM migrations WHERE name = ?", "006_add_metadata_content_digest"); err != nil {
		t.Fatalf("resetting digest migration: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO metadata_cache (ecosystem, name, storage_path, content_type, size, fetched_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, "oci-manifest", "cache-key", "_metadata/oci-manifest/cache-key/metadata", "application/json", 2, time.Now(), time.Now(), time.Now()); err != nil {
		t.Fatalf("inserting legacy cache row: %v", err)
	}

	if err := db.MigrateSchema(); err != nil {
		t.Fatalf("MigrateSchema() error = %v", err)
	}
	hasDigest, err := db.HasColumn("metadata_cache", "content_digest")
	if err != nil {
		t.Fatalf("HasColumn() error = %v", err)
	}
	if !hasDigest {
		t.Fatal("metadata_cache.content_digest was not added")
	}

	entry, err := db.GetMetadataCache("oci-manifest", "cache-key")
	if err != nil {
		t.Fatalf("GetMetadataCache() error = %v", err)
	}
	if entry == nil || entry.StoragePath != "_metadata/oci-manifest/cache-key/metadata" {
		t.Fatalf("existing metadata cache row was not preserved: %#v", entry)
	}
	if entry.ContentDigest.Valid {
		t.Errorf("legacy content digest = %q, want NULL", entry.ContentDigest.String)
	}
}

func TestMetadataCacheLinkMigrationPreservesExistingRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := Create(dbPath)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("ALTER TABLE metadata_cache DROP COLUMN link"); err != nil {
		t.Fatalf("dropping link: %v", err)
	}
	if _, err := db.Exec("DELETE FROM migrations WHERE name = ?", "007_add_metadata_link"); err != nil {
		t.Fatalf("resetting link migration: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO metadata_cache (ecosystem, name, storage_path, content_type, size, fetched_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, "oci-tags", "cache-key", "_metadata/oci-tags/cache-key/metadata", "application/json", 2, time.Now(), time.Now(), time.Now()); err != nil {
		t.Fatalf("inserting legacy cache row: %v", err)
	}

	if err := db.MigrateSchema(); err != nil {
		t.Fatalf("MigrateSchema() error = %v", err)
	}
	hasLink, err := db.HasColumn("metadata_cache", "link")
	if err != nil {
		t.Fatalf("HasColumn() error = %v", err)
	}
	if !hasLink {
		t.Fatal("metadata_cache.link was not added")
	}

	entry, err := db.GetMetadataCache("oci-tags", "cache-key")
	if err != nil {
		t.Fatalf("GetMetadataCache() error = %v", err)
	}
	if entry == nil || entry.StoragePath != "_metadata/oci-tags/cache-key/metadata" {
		t.Fatalf("existing metadata cache row was not preserved: %#v", entry)
	}
	if entry.Link.Valid {
		t.Errorf("legacy link = %q, want NULL", entry.Link.String)
	}
}

func TestMetadataCacheContentEncodingMigrationPreservesExistingRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := Create(dbPath)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("ALTER TABLE metadata_cache DROP COLUMN content_encoding"); err != nil {
		t.Fatalf("dropping content_encoding: %v", err)
	}
	if _, err := db.Exec("DELETE FROM migrations WHERE name = ?", "008_add_metadata_content_encoding"); err != nil {
		t.Fatalf("resetting content_encoding migration: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO metadata_cache (ecosystem, name, storage_path, etag, content_type, size, fetched_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, "apk", "cache-key", "_metadata/apk/cache-key/metadata", "\"legacy-etag\"", "application/octet-stream", 2, time.Now(), time.Now(), time.Now()); err != nil {
		t.Fatalf("inserting legacy cache row: %v", err)
	}

	if err := db.MigrateSchema(); err != nil {
		t.Fatalf("MigrateSchema() error = %v", err)
	}
	hasEncoding, err := db.HasColumn("metadata_cache", "content_encoding")
	if err != nil {
		t.Fatalf("HasColumn() error = %v", err)
	}
	if !hasEncoding {
		t.Fatal("metadata_cache.content_encoding was not added")
	}

	entry, err := db.GetMetadataCache("apk", "cache-key")
	if err != nil {
		t.Fatalf("GetMetadataCache() error = %v", err)
	}
	if entry == nil || entry.StoragePath != "_metadata/apk/cache-key/metadata" {
		t.Fatalf("existing metadata cache row was not preserved: %#v", entry)
	}
	if entry.ContentEncoding.Valid {
		t.Errorf("legacy content encoding = %q, want NULL", entry.ContentEncoding.String)
	}
	// The migration must clear the pre-fix validators so the row is refetched
	// once with identity instead of a 304 re-serving the decompressed copy.
	if entry.ETag.Valid {
		t.Errorf("legacy etag = %q, want cleared", entry.ETag.String)
	}
	if entry.FetchedAt.Valid {
		t.Errorf("legacy fetched_at = %v, want cleared", entry.FetchedAt.Time)
	}
}

func TestMetadataCacheRoundTripsContentEncoding(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := Create(dbPath)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	defer func() { _ = db.Close() }()

	entry := &MetadataCacheEntry{
		Ecosystem:       "apk",
		Name:            "index-key",
		StoragePath:     "_metadata/apk/index-key/metadata",
		ContentType:     sql.NullString{String: "application/octet-stream", Valid: true},
		ContentEncoding: sql.NullString{String: "gzip", Valid: true},
		Size:            sql.NullInt64{Int64: 10, Valid: true},
		FetchedAt:       sql.NullTime{Time: time.Now(), Valid: true},
	}
	if err := db.UpsertMetadataCache(entry); err != nil {
		t.Fatalf("UpsertMetadataCache() error = %v", err)
	}

	got, err := db.GetMetadataCache("apk", "index-key")
	if err != nil {
		t.Fatalf("GetMetadataCache() error = %v", err)
	}
	if got == nil || !got.ContentEncoding.Valid || got.ContentEncoding.String != "gzip" {
		t.Fatalf("content encoding round-trip failed: %#v", got)
	}
}

// On Postgres, fetched_at is a TIMESTAMP column without a zone. East of UTC
// it must still read back as the moment it was written, or metadata_ttl
// expires hours late. last_modified is an HTTP date in UTC and must keep it.
func TestMetadataCacheTimesOffUTC(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skipf("time zone data unavailable: %v", err)
	}
	prev := time.Local
	time.Local = tokyo
	t.Cleanup(func() { time.Local = prev })

	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		fetched := time.Now().Add(-90 * time.Minute)
		modified := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
		entry := &MetadataCacheEntry{
			Ecosystem:    testEcosystemNPM,
			Name:         "tz",
			StoragePath:  "_metadata/npm/tz/metadata",
			LastModified: sql.NullTime{Time: modified, Valid: true},
			FetchedAt:    sql.NullTime{Time: fetched, Valid: true},
		}
		if err := db.UpsertMetadataCache(entry); err != nil {
			t.Fatalf("UpsertMetadataCache() error = %v", err)
		}

		got, err := db.GetMetadataCache(testEcosystemNPM, "tz")
		if err != nil || got == nil {
			t.Fatalf("GetMetadataCache() = %v, %v", got, err)
		}
		if diff := got.FetchedAt.Time.Sub(fetched).Abs(); diff > time.Second {
			t.Errorf("fetched_at read back as %v, want %v", got.FetchedAt.Time, fetched)
		}
		if !got.LastModified.Time.Equal(modified) {
			t.Errorf("last_modified read back as %v, want %v", got.LastModified.Time, modified)
		}
	})
}

// TestRefreshMetadataCache checks that a refresh moves only fetched_at on, and
// only while the row still records the digest it was asked for.
func TestRefreshMetadataCache(t *testing.T) {
	db := setupMetadataCacheDB(t)
	fetched := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	entry := &MetadataCacheEntry{
		Ecosystem:     testEcosystemNPM,
		Name:          "lodash",
		StoragePath:   "_metadata/npm/lodash/metadata",
		ETag:          sql.NullString{String: `"abc123"`, Valid: true},
		ContentDigest: sql.NullString{String: "sha256:aaaa", Valid: true},
		Size:          sql.NullInt64{Int64: 1024, Valid: true},
		FetchedAt:     sql.NullTime{Time: fetched, Valid: true},
	}
	if err := db.UpsertMetadataCache(entry); err != nil {
		t.Fatal(err)
	}

	refreshed, err := db.RefreshMetadataCache(testEcosystemNPM, "lodash", "sha256:bbbb", time.Now())
	if err != nil || refreshed {
		t.Fatalf("refresh with another digest = %v, %v, want false", refreshed, err)
	}
	if got, _ := db.GetMetadataCache(testEcosystemNPM, "lodash"); !got.FetchedAt.Time.Equal(fetched) {
		t.Errorf("fetched_at = %v after a refresh with another digest, want %v", got.FetchedAt.Time, fetched)
	}

	now := time.Now().Truncate(time.Second)
	refreshed, err = db.RefreshMetadataCache(testEcosystemNPM, "lodash", "sha256:aaaa", now)
	if err != nil || !refreshed {
		t.Fatalf("refresh = %v, %v, want true", refreshed, err)
	}
	got, err := db.GetMetadataCache(testEcosystemNPM, "lodash")
	if err != nil {
		t.Fatal(err)
	}
	if !got.FetchedAt.Time.Equal(now) {
		t.Errorf("fetched_at = %v, want %v", got.FetchedAt.Time, now)
	}
	if got.ETag.String != `"abc123"` || got.ContentDigest.String != "sha256:aaaa" || got.Size.Int64 != 1024 || got.StoragePath != entry.StoragePath {
		t.Errorf("refresh changed the row: %+v", got)
	}

	if refreshed, err := db.RefreshMetadataCache(testEcosystemNPM, "missing", "sha256:aaaa", now); err != nil || refreshed {
		t.Errorf("refresh of a missing row = %v, %v, want false", refreshed, err)
	}
}
