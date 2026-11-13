package database

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

// lastUsedIndexDefinition returns the SQL idx_artifacts_last_used was
// created with, or "" if there is no such index.
func lastUsedIndexDefinition(t *testing.T, db *DB) string {
	t.Helper()
	query := `SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'idx_artifacts_last_used'`
	if db.dialect == DialectPostgres {
		query = `SELECT indexdef FROM pg_indexes WHERE schemaname = current_schema() AND indexname = 'idx_artifacts_last_used'`
	}
	var def sql.NullString
	if err := db.Get(&def, query); err != nil && err != sql.ErrNoRows {
		t.Fatalf("reading the index: %v", err)
	}
	return def.String
}

// isPartialOnStoredArtifacts reports whether an index definition leaves
// out artifacts that are not in storage.
func isPartialOnStoredArtifacts(def string) bool {
	_, where, ok := strings.Cut(strings.ToLower(def), " where ")
	return ok && strings.Contains(where, "storage_path is not null")
}

// TestArtifactsLastUsedIndexLeavesOutClearedArtifacts checks that the index
// eviction reads holds only artifacts in storage. Eviction and retention
// clear a row's storage_path and keep the row, and those rows sort first,
// so in a full index every batch walks past all of them.
func TestArtifactsLastUsedIndexLeavesOutClearedArtifacts(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		def := lastUsedIndexDefinition(t, db)
		if !isPartialOnStoredArtifacts(def) {
			t.Errorf("fresh schema: idx_artifacts_last_used = %q, want it limited to storage_path IS NOT NULL", def)
		}
	})
}

// TestMigrationMakesArtifactsLastUsedIndexPartial upgrades a database that
// has the index over every artifact, as migration 014 created it.
func TestMigrationMakesArtifactsLastUsedIndexPartial(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		full := `CREATE INDEX idx_artifacts_last_used ON artifacts(COALESCE(last_accessed_at, fetched_at))`
		if db.dialect == DialectPostgres {
			full = `CREATE INDEX idx_artifacts_last_used ON artifacts((COALESCE(last_accessed_at, fetched_at)) NULLS FIRST)`
		}
		if _, err := db.Exec(`DROP INDEX IF EXISTS idx_artifacts_last_used`); err != nil {
			t.Fatalf("dropping the index: %v", err)
		}
		if _, err := db.Exec(full); err != nil {
			t.Fatalf("creating the old index: %v", err)
		}
		if isPartialOnStoredArtifacts(lastUsedIndexDefinition(t, db)) {
			t.Fatal("the old index is already partial")
		}

		// Twice: the migration must be safe to run again.
		for range 2 {
			if err := migratePartialArtifactsLastUsedIndex(db); err != nil {
				t.Fatalf("migration: %v", err)
			}
			if def := lastUsedIndexDefinition(t, db); !isPartialOnStoredArtifacts(def) {
				t.Fatalf("after the migration: idx_artifacts_last_used = %q, want it limited to storage_path IS NOT NULL", def)
			}
		}

		// A database that never had the index gets it too.
		if _, err := db.Exec(`DROP INDEX IF EXISTS idx_artifacts_last_used`); err != nil {
			t.Fatalf("dropping the index: %v", err)
		}
		if err := migratePartialArtifactsLastUsedIndex(db); err != nil {
			t.Fatalf("migration without an index: %v", err)
		}
		if def := lastUsedIndexDefinition(t, db); !isPartialOnStoredArtifacts(def) {
			t.Errorf("created from nothing: idx_artifacts_last_used = %q", def)
		}
	})
}

// TestLeastRecentlyUsedSkipsClearedArtifacts checks eviction still gets the
// same answer: artifacts in storage, least recently used first, with the
// cleared ones, which sort before them, left out.
func TestLeastRecentlyUsedSkipsClearedArtifacts(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		now := time.Now()
		add := func(name string, used time.Duration) {
			t.Helper()
			path := "npm/" + name + "/1.0.0/" + name + ".tgz"
			if err := db.UpsertArtifact(&Artifact{
				VersionPURL: "pkg:npm/" + name + "@1.0.0",
				Filename:    name + ".tgz",
				UpstreamURL: "https://example.test/" + name + ".tgz",
				StoragePath: sql.NullString{String: path, Valid: true},
				Size:        sql.NullInt64{Int64: 1, Valid: true},
				FetchedAt:   sql.NullTime{Time: now.Add(-used), Valid: true},
			}); err != nil {
				t.Fatalf("UpsertArtifact: %v", err)
			}
		}
		add("cleared-a", 5*time.Hour)
		add("cleared-b", 4*time.Hour)
		add("oldest", 3*time.Hour)
		add("newer", 2*time.Hour)
		for _, name := range []string{"cleared-a", "cleared-b"} {
			cleared, err := db.ClearArtifactCache("pkg:npm/"+name+"@1.0.0", name+".tgz", "npm/"+name+"/1.0.0/"+name+".tgz")
			if err != nil || !cleared {
				t.Fatalf("ClearArtifactCache(%s) = %v, %v", name, cleared, err)
			}
		}

		entries, err := db.GetLeastRecentlyUsedCacheEntries(10)
		if err != nil {
			t.Fatalf("GetLeastRecentlyUsedCacheEntries: %v", err)
		}
		var got []string
		for _, e := range entries {
			got = append(got, e.Name)
		}
		if want := []string{"oldest.tgz", "newer.tgz"}; strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("entries = %v, want %v", got, want)
		}
	})
}
