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
		ContentType: sql.NullString{String: "application/json", Valid: true},
		Size:        sql.NullInt64{Int64: 1024, Valid: true},
		FetchedAt:   sql.NullTime{Time: time.Now(), Valid: true},
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
	if !got.ContentType.Valid || got.ContentType.String != "application/json" {
		t.Errorf("content_type = %v, want %q", got.ContentType, "application/json")
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
