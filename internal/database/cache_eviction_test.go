package database

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

func upsertTestMetadata(t *testing.T, db *DB, name string, size int64) {
	t.Helper()
	if err := db.UpsertMetadataCache(&MetadataCacheEntry{
		Ecosystem:   testEcosystemNPM,
		Name:        name,
		StoragePath: "_metadata/npm/" + name + "/metadata",
		Size:        sql.NullInt64{Int64: size, Valid: true},
		FetchedAt:   sql.NullTime{Time: time.Now(), Valid: true},
	}); err != nil {
		t.Fatalf("UpsertMetadataCache: %v", err)
	}
}

func setTestMetadataAccess(t *testing.T, db *DB, name string, at time.Time) {
	t.Helper()
	query := db.Rebind(`UPDATE metadata_cache SET last_accessed_at = ? WHERE ecosystem = ? AND name = ?`)
	if _, err := db.Exec(query, at.UTC(), testEcosystemNPM, name); err != nil {
		t.Fatalf("setting last access: %v", err)
	}
}

func TestUpsertMetadataCacheMarksAccess(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		before := time.Now().Add(-time.Second)
		upsertTestMetadata(t, db, "lodash", 10)

		got, err := db.GetMetadataCache(testEcosystemNPM, "lodash")
		if err != nil || got == nil {
			t.Fatalf("GetMetadataCache: %v, %v", got, err)
		}
		if !got.LastAccessedAt.Valid || got.LastAccessedAt.Time.Before(before) {
			t.Errorf("last_accessed_at = %v, want the time of the write", got.LastAccessedAt)
		}
	})
}

func TestGetEvictableCacheSizeCountsMetadata(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		upsertTestMetadata(t, db, "a", 300)
		upsertTestMetadata(t, db, "b", 200)

		total, err := db.GetEvictableCacheSize()
		if err != nil {
			t.Fatalf("GetEvictableCacheSize: %v", err)
		}
		if total != 500 {
			t.Errorf("evictable size = %d, want 500", total)
		}
		// The size shown in stats stays the artifacts' total.
		artifacts, err := db.GetTotalCacheSize()
		if err != nil || artifacts != 0 {
			t.Errorf("GetTotalCacheSize = %d, %v, want 0", artifacts, err)
		}
	})
}

func TestGetLeastRecentlyUsedCacheEntriesOrdersMetadataAndArtifacts(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		now := time.Now()
		upsertTestMetadata(t, db, "old", 1)
		setTestMetadataAccess(t, db, "old", now.Add(-3*time.Hour))
		upsertTestMetadata(t, db, "new", 1)
		setTestMetadataAccess(t, db, "new", now)
		if err := db.UpsertArtifact(&Artifact{
			VersionPURL:    "pkg:npm/mid@1.0.0",
			Filename:       "mid-1.0.0.tgz",
			UpstreamURL:    "https://example.test/mid-1.0.0.tgz",
			StoragePath:    sql.NullString{String: "npm/mid/1.0.0/mid-1.0.0.tgz", Valid: true},
			Size:           sql.NullInt64{Int64: 1, Valid: true},
			LastAccessedAt: sql.NullTime{Time: now.Add(-time.Hour), Valid: true},
		}); err != nil {
			t.Fatalf("UpsertArtifact: %v", err)
		}

		entries, err := db.GetLeastRecentlyUsedCacheEntries(10)
		if err != nil {
			t.Fatalf("GetLeastRecentlyUsedCacheEntries: %v", err)
		}
		var got []string
		for _, e := range entries {
			got = append(got, e.Kind+":"+e.Name)
		}
		want := []string{"metadata:old", "artifact:mid-1.0.0.tgz", "metadata:new"}
		if len(got) != len(want) {
			t.Fatalf("entries = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("entries = %v, want %v", got, want)
			}
		}
	})
}

func TestEvictMetadataCacheKeepsRowUsedSinceRead(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		upsertTestMetadata(t, db, "lodash", 10)
		setTestMetadataAccess(t, db, "lodash", time.Now().Add(-time.Hour))
		read, _ := db.GetMetadataCache(testEcosystemNPM, "lodash")

		if err := db.RecordMetadataHit(testEcosystemNPM, "lodash"); err != nil {
			t.Fatalf("RecordMetadataHit: %v", err)
		}
		evicted, err := db.EvictMetadataCache(testEcosystemNPM, "lodash", read.StoragePath, read.LastAccessedAt)
		if err != nil || evicted {
			t.Fatalf("EvictMetadataCache = %v, %v, want the row kept", evicted, err)
		}

		current, _ := db.GetMetadataCache(testEcosystemNPM, "lodash")
		evicted, err = db.EvictMetadataCache(testEcosystemNPM, "lodash", current.StoragePath, current.LastAccessedAt)
		if err != nil || !evicted {
			t.Fatalf("EvictMetadataCache = %v, %v, want the row evicted", evicted, err)
		}
		if gone, _ := db.GetMetadataCache(testEcosystemNPM, "lodash"); gone != nil {
			t.Error("row still present after eviction")
		}
	})
}

func TestRecordMetadataHitBatched(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		upsertTestMetadata(t, db, "lodash", 10)
		old := time.Now().Add(-time.Hour)
		setTestMetadataAccess(t, db, "lodash", old)
		db.BatchHits(time.Hour, nil)

		if err := db.RecordMetadataHit(testEcosystemNPM, "lodash"); err != nil {
			t.Fatalf("RecordMetadataHit: %v", err)
		}
		if got, _ := db.GetMetadataCache(testEcosystemNPM, "lodash"); got.LastAccessedAt.Time.After(old.Add(time.Minute)) {
			t.Error("batched hit was written before the flush")
		}
		if err := db.flushHits(); err != nil {
			t.Fatalf("flushHits: %v", err)
		}
		if got, _ := db.GetMetadataCache(testEcosystemNPM, "lodash"); !got.LastAccessedAt.Time.After(old.Add(time.Minute)) {
			t.Errorf("last_accessed_at = %v after flush, want the hit's time", got.LastAccessedAt)
		}
	})
}

func TestPendingDeleteSkipsPathMetadataPointsAt(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		upsertTestMetadata(t, db, "lodash", 10)
		path := "_metadata/npm/lodash/metadata"
		if err := db.QueuePendingDelete(path); err != nil {
			t.Fatalf("QueuePendingDelete: %v", err)
		}

		due, err := db.GetDuePendingDeletes(time.Now().Add(time.Hour), 10)
		if err != nil {
			t.Fatalf("GetDuePendingDeletes: %v", err)
		}
		if len(due) != 0 {
			t.Errorf("due = %v, want the path a metadata row uses skipped", due)
		}
	})
}

// TestGetLeastRecentlyUsedCacheEntriesCountsFetchAsUse checks that an
// artifact not served since its download ranks by when it was fetched, not
// ahead of every metadata document.
func TestGetLeastRecentlyUsedCacheEntriesCountsFetchAsUse(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		now := time.Now()
		upsertTestMetadata(t, db, "old", 1)
		setTestMetadataAccess(t, db, "old", now.Add(-time.Hour))
		if err := db.UpsertArtifact(&Artifact{
			VersionPURL: "pkg:npm/fresh@1.0.0",
			Filename:    "fresh-1.0.0.tgz",
			UpstreamURL: "https://example.test/fresh-1.0.0.tgz",
			StoragePath: sql.NullString{String: "npm/fresh/1.0.0/fresh-1.0.0.tgz", Valid: true},
			Size:        sql.NullInt64{Int64: 1, Valid: true},
			FetchedAt:   sql.NullTime{Time: now, Valid: true},
		}); err != nil {
			t.Fatalf("UpsertArtifact: %v", err)
		}

		entries, err := db.GetLeastRecentlyUsedCacheEntries(1)
		if err != nil || len(entries) != 1 {
			t.Fatalf("GetLeastRecentlyUsedCacheEntries: %v, %v", entries, err)
		}
		if entries[0].Kind != CacheEntryMetadata {
			t.Errorf("least recently used = %+v, want the metadata used an hour ago", entries[0])
		}
	})
}

// TestLeastRecentlyUsedQueriesUseIndexes checks on SQLite that eviction reads
// both tables off an index rather than sorting them whole.
func TestLeastRecentlyUsedQueriesUseIndexes(t *testing.T) {
	db := setupMetadataCacheDB(t)
	for _, q := range []string{
		`SELECT version_purl FROM artifacts WHERE storage_path IS NOT NULL AND source <> 'local'
		 ORDER BY COALESCE(last_accessed_at, fetched_at) ASC NULLS FIRST LIMIT 50`,
		`SELECT name FROM metadata_cache ORDER BY COALESCE(last_accessed_at, updated_at) ASC NULLS FIRST LIMIT 50`,
	} {
		rows, err := db.Query("EXPLAIN QUERY PLAN " + q)
		if err != nil {
			t.Fatalf("explain: %v", err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatalf("scan: %v", err)
			}
			plan = append(plan, detail)
		}
		_ = rows.Close()
		for _, step := range plan {
			if strings.Contains(step, "TEMP B-TREE") {
				t.Errorf("query sorts instead of using an index: %v\n%s", plan, q)
			}
		}
	}
}

// insertLegacyMetadata adds a row as the proxy wrote it before
// last_accessed_at existed: write times in local time, no access time.
func insertLegacyMetadata(t *testing.T, db *DB, name string, written time.Time) {
	t.Helper()
	if _, err := db.Exec(db.Rebind(`
		INSERT INTO metadata_cache (ecosystem, name, storage_path, size, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`), testEcosystemNPM, name, "_metadata/npm/"+name+"/metadata", 1, written, written); err != nil {
		t.Fatalf("inserting legacy row: %v", err)
	}
}

func TestMetadataLastAccessedMigrationLeavesBackfillToBackground(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		for _, q := range []string{
			"DROP INDEX idx_metadata_last_used",
			"DROP INDEX idx_metadata_storage_path",
			"DROP INDEX idx_artifacts_last_used",
			"ALTER TABLE metadata_cache DROP COLUMN last_accessed_at",
			"DELETE FROM migrations WHERE name = '014_add_metadata_last_accessed'",
		} {
			if _, err := db.Exec(q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		written := time.Date(2026, 1, 2, 3, 4, 5, 0, time.Local)
		insertLegacyMetadata(t, db, "legacy", written)

		if err := db.MigrateSchema(); err != nil {
			t.Fatalf("MigrateSchema: %v", err)
		}
		if got, _ := db.GetMetadataCache(testEcosystemNPM, "legacy"); got == nil || got.LastAccessedAt.Valid {
			t.Fatalf("row after migration = %+v, want it kept with no access time", got)
		}
		if n, err := db.BackfillMetadataLastAccessed(10); err != nil || n != 1 {
			t.Fatalf("BackfillMetadataLastAccessed = %d, %v, want 1", n, err)
		}
		got, _ := db.GetMetadataCache(testEcosystemNPM, "legacy")
		if got == nil || !got.LastAccessedAt.Valid || !got.LastAccessedAt.Time.Equal(written) {
			t.Errorf("backfilled last_accessed_at = %+v, want %v", got, written.UTC())
		}
	})
}

// TestBackfillMetadataLastAccessedResumes runs the backfill in batches, as a
// restart after an interruption would, and leaves rows served meanwhile alone.
func TestBackfillMetadataLastAccessedResumes(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		written := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
		for _, name := range []string{"a", "b", "c"} {
			insertLegacyMetadata(t, db, name, written)
		}
		if n, err := db.BackfillMetadataLastAccessed(2); err != nil || n != 2 {
			t.Fatalf("first batch = %d, %v, want 2", n, err)
		}
		var left string
		if err := db.Get(&left, `SELECT name FROM metadata_cache WHERE last_accessed_at IS NULL`); err != nil {
			t.Fatalf("finding the row left: %v", err)
		}
		if err := db.RecordMetadataHit(testEcosystemNPM, left); err != nil {
			t.Fatalf("RecordMetadataHit: %v", err)
		}
		if n, err := db.BackfillMetadataLastAccessed(2); err != nil || n != 0 {
			t.Fatalf("second batch = %d, %v, want 0", n, err)
		}
		got, _ := db.GetMetadataCache(testEcosystemNPM, left)
		if !got.LastAccessedAt.Time.After(written.Add(time.Hour)) {
			t.Errorf("served row last_accessed_at = %v, want the hit kept", got.LastAccessedAt)
		}
	})
}

// TestLeastRecentlyUsedOrdersUnbackfilledMetadataByWriteTime checks that rows
// still waiting for the backfill are not all evicted first.
func TestLeastRecentlyUsedOrdersUnbackfilledMetadataByWriteTime(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		now := time.Now()
		insertLegacyMetadata(t, db, "recent", now)
		if err := db.UpsertArtifact(&Artifact{
			VersionPURL:    "pkg:npm/old@1.0.0",
			Filename:       "old-1.0.0.tgz",
			UpstreamURL:    "https://example.test/old-1.0.0.tgz",
			StoragePath:    sql.NullString{String: "npm/old/1.0.0/old-1.0.0.tgz", Valid: true},
			Size:           sql.NullInt64{Int64: 1, Valid: true},
			LastAccessedAt: sql.NullTime{Time: now.Add(-time.Hour), Valid: true},
		}); err != nil {
			t.Fatalf("UpsertArtifact: %v", err)
		}

		entries, err := db.GetLeastRecentlyUsedCacheEntries(1)
		if err != nil || len(entries) != 1 {
			t.Fatalf("GetLeastRecentlyUsedCacheEntries: %v, %v", entries, err)
		}
		if entries[0].Kind != CacheEntryArtifact {
			t.Errorf("least recently used = %+v, want the artifact used an hour ago", entries[0])
		}
	})
}
