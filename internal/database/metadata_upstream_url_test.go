package database

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestMigrationAddsMetadataUpstreamURL(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "existing.db")
	sqlDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := sqlDB.Exec(schemaSQLite); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if _, err := sqlDB.Exec(`ALTER TABLE metadata_cache DROP COLUMN upstream_url`); err != nil {
		t.Fatalf("drop upstream_url column: %v", err)
	}
	if _, err := sqlDB.Exec(`DELETE FROM migrations WHERE name = '020_add_metadata_upstream_url'`); err != nil {
		t.Fatalf("forget migration: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.MigrateSchema(); err != nil {
		t.Fatalf("MigrateSchema: %v", err)
	}
	has, err := db.HasColumn("metadata_cache", "upstream_url")
	if err != nil || !has {
		t.Errorf("metadata_cache.upstream_url after migration = %v, %v", has, err)
	}
}

func upsertMetadataURL(t *testing.T, db *DB, ecosystem, name, url string) {
	t.Helper()
	entry := &MetadataCacheEntry{Ecosystem: ecosystem, Name: name, StoragePath: "_metadata/" + name}
	if url != "" {
		entry.UpstreamURL = sql.NullString{String: url, Valid: true}
	}
	if err := db.UpsertMetadataCache(entry); err != nil {
		t.Fatal(err)
	}
}

func TestUpsertMetadataCacheKeepsUpstreamURL(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		upsertMetadataURL(t, db, "apk", "k1", "https://dl-cdn.alpinelinux.org/alpine/v3.20/main/aarch64/APKINDEX.tar.gz")
		// A writer that doesn't know the URL leaves it as it was.
		upsertMetadataURL(t, db, "apk", "k1", "")
		got, err := db.GetMetadataCache("apk", "k1")
		if err != nil || got == nil {
			t.Fatalf("GetMetadataCache: %+v, %v", got, err)
		}
		if got.UpstreamURL.String != "https://dl-cdn.alpinelinux.org/alpine/v3.20/main/aarch64/APKINDEX.tar.gz" {
			t.Errorf("upstream_url = %q", got.UpstreamURL.String)
		}
	})
}

func TestRecentMetadataDocuments(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		upsertMetadataURL(t, db, "apk", "k1", "https://a.test/v3.20/main/aarch64/APKINDEX.tar.gz")
		upsertMetadataURL(t, db, "apk", "k2", "https://a.test/v3.19/main/aarch64/APKINDEX.tar.gz")
		upsertMetadataURL(t, db, "apk", "k3", "")
		upsertMetadataURL(t, db, "debian", "k4", "https://d.test/dists/bookworm/InRelease")
		old := time.Now().Add(-30 * 24 * time.Hour).UTC()
		if _, err := db.Exec(db.Rebind("UPDATE metadata_cache SET last_accessed_at = ?, updated_at = ? WHERE name = ?"), old, old, "k2"); err != nil {
			t.Fatal(err)
		}

		got, err := db.RecentMetadataDocuments("apk", time.Now().Add(-14*24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		want := []MetadataDocument{{Name: "k1", UpstreamURL: "https://a.test/v3.20/main/aarch64/APKINDEX.tar.gz"}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("RecentMetadataDocuments = %+v, want %+v", got, want)
		}
	})
}
