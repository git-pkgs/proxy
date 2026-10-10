package database

import (
	"database/sql"
	"testing"
	"time"
)

// seedRetentionArtifact stores a cached artifact for pkg:{ecosystem}/{name}
// with the given fetch and last access times; a zero accessed leaves the
// artifact never served.
func seedRetentionArtifact(t *testing.T, db *DB, ecosystem, name string, fetched, accessed time.Time) {
	t.Helper()
	pkgPURL := "pkg:" + ecosystem + "/" + name
	versionPURL := pkgPURL + "@1.0.0"
	if err := db.UpsertPackage(&Package{PURL: pkgPURL, Ecosystem: ecosystem, Name: name}); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}
	if err := db.UpsertVersion(&Version{PURL: versionPURL, PackagePURL: pkgPURL}); err != nil {
		t.Fatalf("UpsertVersion: %v", err)
	}
	art := &Artifact{
		VersionPURL: versionPURL,
		Filename:    name + ".tgz",
		UpstreamURL: "https://example.com/" + name + ".tgz",
		StoragePath: sql.NullString{String: ecosystem + "/" + name + "/1.0.0/" + name + ".tgz", Valid: true},
		Size:        sql.NullInt64{Int64: 100, Valid: true},
		FetchedAt:   sql.NullTime{Time: fetched, Valid: true},
	}
	if !accessed.IsZero() {
		art.LastAccessedAt = sql.NullTime{Time: accessed, Valid: true}
	}
	if err := db.UpsertArtifact(art); err != nil {
		t.Fatalf("UpsertArtifact: %v", err)
	}
}

func candidateNames(candidates []RetentionCandidate) map[string]RetentionCandidate {
	byName := make(map[string]RetentionCandidate, len(candidates))
	for _, c := range candidates {
		byName[c.Filename] = c
	}
	return byName
}

func TestGetRetentionCandidates(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		now := time.Now()
		cutoff := now.Add(-24 * time.Hour)
		old := now.Add(-48 * time.Hour)

		seedRetentionArtifact(t, db, "npm", "stale", old, old)
		seedRetentionArtifact(t, db, "npm", "never-served", old, time.Time{})
		seedRetentionArtifact(t, db, "npm", "recently-served", old, now)
		// Fetched again after an eviction: the clear kept the old access time.
		seedRetentionArtifact(t, db, "npm", "refetched", now, old)
		seedRetentionArtifact(t, db, "gem", "other-ecosystem", old, old)

		maxID, err := db.MaxArtifactID()
		if err != nil {
			t.Fatalf("MaxArtifactID: %v", err)
		}
		candidates, err := db.GetRetentionCandidates(0, maxID, cutoff)
		if err != nil {
			t.Fatalf("GetRetentionCandidates: %v", err)
		}
		got := candidateNames(candidates)

		for _, want := range []string{"stale.tgz", "never-served.tgz", "other-ecosystem.tgz"} {
			if _, ok := got[want]; !ok {
				t.Errorf("%s missing from candidates", want)
			}
		}
		for _, unwanted := range []string{"recently-served.tgz", "refetched.tgz"} {
			if _, ok := got[unwanted]; ok {
				t.Errorf("%s returned as a candidate", unwanted)
			}
		}

		stale := got["stale.tgz"]
		if stale.Ecosystem != "npm" || stale.PackagePURL != "pkg:npm/stale" {
			t.Errorf("stale candidate ecosystem/package = %q/%q, want npm/pkg:npm/stale", stale.Ecosystem, stale.PackagePURL)
		}
		if !stale.ExpiredBefore(cutoff) {
			t.Error("stale candidate not expired before the cutoff")
		}
		if got["other-ecosystem.tgz"].Ecosystem != "gem" {
			t.Errorf("other-ecosystem candidate ecosystem = %q, want gem", got["other-ecosystem.tgz"].Ecosystem)
		}
	})
}

func TestGetRetentionCandidatesWindow(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		old := time.Now().Add(-48 * time.Hour)
		for _, name := range []string{"a", "b", "c"} {
			seedRetentionArtifact(t, db, "npm", name, old, old)
		}
		cutoff := time.Now().Add(-time.Hour)

		first, err := db.GetRetentionCandidates(0, 1, cutoff)
		if err != nil {
			t.Fatalf("GetRetentionCandidates: %v", err)
		}
		rest, err := db.GetRetentionCandidates(1, 3, cutoff)
		if err != nil {
			t.Fatalf("GetRetentionCandidates: %v", err)
		}
		if len(first) != 1 || len(rest) != 2 {
			t.Fatalf("window sizes = %d and %d, want 1 and 2", len(first), len(rest))
		}
		if first[0].ID != 1 || rest[0].ID != 2 || rest[1].ID != 3 {
			t.Errorf("window ids = %d, %d, %d, want 1, 2, 3", first[0].ID, rest[0].ID, rest[1].ID)
		}
	})
}

func TestClearExpiredArtifact(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		now := time.Now()
		old := now.Add(-48 * time.Hour)
		cutoff := now.Add(-24 * time.Hour)
		seedRetentionArtifact(t, db, "npm", "stale", old, old)
		seedRetentionArtifact(t, db, "npm", "served", old, old)
		const stalePath = "npm/stale/1.0.0/stale.tgz"

		// A hit lands between the scan and the clear.
		if err := db.RecordArtifactHit("pkg:npm/served@1.0.0", "served.tgz"); err != nil {
			t.Fatalf("RecordArtifactHit: %v", err)
		}
		cleared, err := db.ClearExpiredArtifact("pkg:npm/served@1.0.0", "served.tgz", "npm/served/1.0.0/served.tgz", cutoff)
		if err != nil || cleared {
			t.Errorf("ClearExpiredArtifact(served) = %v, %v, want false, nil", cleared, err)
		}

		cleared, err = db.ClearExpiredArtifact("pkg:npm/stale@1.0.0", "stale.tgz", "elsewhere", cutoff)
		if err != nil || cleared {
			t.Errorf("ClearExpiredArtifact(moved record) = %v, %v, want false, nil", cleared, err)
		}

		cleared, err = db.ClearExpiredArtifact("pkg:npm/stale@1.0.0", "stale.tgz", stalePath, cutoff)
		if err != nil || !cleared {
			t.Fatalf("ClearExpiredArtifact(stale) = %v, %v, want true, nil", cleared, err)
		}
		art, err := db.GetArtifact("pkg:npm/stale@1.0.0", "stale.tgz")
		if err != nil {
			t.Fatalf("GetArtifact: %v", err)
		}
		if art.StoragePath.Valid || art.FetchedAt.Valid {
			t.Error("stale artifact still cached after the clear")
		}
		if !art.LastAccessedAt.Valid {
			t.Error("clear dropped last_accessed_at")
		}
	})
}

func TestFlushHitsWritesBufferedHits(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		old := time.Now().Add(-48 * time.Hour)
		seedRetentionArtifact(t, db, "npm", "buffered", old, old)
		db.BatchHits(time.Hour, nil)

		if err := db.RecordArtifactHit("pkg:npm/buffered@1.0.0", "buffered.tgz"); err != nil {
			t.Fatalf("RecordArtifactHit: %v", err)
		}
		if err := db.FlushHits(); err != nil {
			t.Fatalf("FlushHits: %v", err)
		}
		art, err := db.GetArtifact("pkg:npm/buffered@1.0.0", "buffered.tgz")
		if err != nil {
			t.Fatalf("GetArtifact: %v", err)
		}
		if !art.LastAccessedAt.Time.After(old) {
			t.Errorf("last_accessed_at = %v after FlushHits, want the buffered hit", art.LastAccessedAt.Time)
		}
	})
}

func TestFlushHitsWithoutBatching(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		if err := db.FlushHits(); err != nil {
			t.Errorf("FlushHits without batching = %v, want nil", err)
		}
	})
}

func TestMaxArtifactIDEmpty(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		maxID, err := db.MaxArtifactID()
		if err != nil || maxID != 0 {
			t.Errorf("MaxArtifactID() = %d, %v, want 0, nil", maxID, err)
		}
	})
}

func TestGetArtifactEcosystem(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		seedRetentionArtifact(t, db, "rubygems", "rails", time.Now(), time.Time{})

		got, err := db.GetArtifactEcosystem("pkg:rubygems/rails@1.0.0")
		if err != nil || got != "rubygems" {
			t.Errorf("GetArtifactEcosystem() = %q, %v, want rubygems, nil", got, err)
		}
		got, err = db.GetArtifactEcosystem("pkg:npm/missing@1.0.0")
		if err != nil || got != "" {
			t.Errorf("GetArtifactEcosystem(missing) = %q, %v, want empty, nil", got, err)
		}
	})
}

func TestPendingDeletesQueuedAtIndex(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		indexQuery := `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_pending_deletes_queued_at'`
		if db.Dialect() == DialectPostgres {
			indexQuery = `SELECT COUNT(*) FROM pg_indexes WHERE indexname = 'idx_pending_deletes_queued_at'`
		}
		hasIndex := func() bool {
			t.Helper()
			var count int
			if err := db.Get(&count, indexQuery); err != nil {
				t.Fatalf("querying indexes: %v", err)
			}
			return count == 1
		}

		if !hasIndex() {
			t.Error("fresh schema has no idx_pending_deletes_queued_at")
		}
		if _, err := db.Exec(`DROP INDEX idx_pending_deletes_queued_at`); err != nil {
			t.Fatalf("dropping index: %v", err)
		}
		if err := migrateAddPendingDeletesQueuedAtIndex(db); err != nil {
			t.Fatalf("migration: %v", err)
		}
		if err := migrateAddPendingDeletesQueuedAtIndex(db); err != nil {
			t.Fatalf("migration is not idempotent: %v", err)
		}
		if !hasIndex() {
			t.Error("migration did not create idx_pending_deletes_queued_at")
		}
	})
}

func TestRetentionCandidateExpiredBefore(t *testing.T) {
	now := time.Now()
	cutoff := now.Add(-24 * time.Hour)
	old := sql.NullTime{Time: now.Add(-48 * time.Hour), Valid: true}
	recent := sql.NullTime{Time: now, Valid: true}

	tests := []struct {
		name              string
		fetched, accessed sql.NullTime
		want              bool
	}{
		{"fetched and accessed before", old, old, true},
		{"never served", old, sql.NullTime{}, true},
		{"accessed since", old, recent, false},
		{"fetched again since", recent, old, false},
		{"not cached", sql.NullTime{}, old, false},
	}
	for _, tt := range tests {
		c := RetentionCandidate{FetchedAt: tt.fetched, LastAccessedAt: tt.accessed}
		if got := c.ExpiredBefore(cutoff); got != tt.want {
			t.Errorf("%s: ExpiredBefore() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// A TIMESTAMP column on Postgres holds the local wall clock time without a
// zone. Read back, the candidate's times must still compare correctly
// against a cutoff east of UTC, where a time labelled UTC would look hours
// newer than it is.
func TestGetRetentionCandidatesTimezone(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skipf("time zone data unavailable: %v", err)
	}
	prev := time.Local
	time.Local = tokyo
	t.Cleanup(func() { time.Local = prev })

	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		now := time.Now()
		fetched := now.Add(-90 * time.Minute)
		seedRetentionArtifact(t, db, "npm", "tz", fetched, time.Time{})
		cutoff := now.Add(-60 * time.Minute)

		candidates, err := db.GetRetentionCandidates(0, 10, cutoff)
		if err != nil {
			t.Fatalf("GetRetentionCandidates: %v", err)
		}
		if len(candidates) != 1 {
			t.Fatalf("got %d candidates, want 1", len(candidates))
		}
		c := candidates[0]
		if !c.ExpiredBefore(cutoff) {
			t.Errorf("candidate fetched at %v not expired before %v", c.FetchedAt.Time, cutoff)
		}
		if diff := c.FetchedAt.Time.Sub(fetched).Abs(); diff > time.Second {
			t.Errorf("fetched_at read back as %v, want %v", c.FetchedAt.Time, fetched)
		}
	})
}
