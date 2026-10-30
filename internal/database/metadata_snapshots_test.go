package database

import (
	"testing"
	"time"
)

func addSnapshot(t *testing.T, db *DB, name, digest string, validFrom time.Time) bool {
	t.Helper()
	added, err := db.AddMetadataSnapshot(&MetadataSnapshot{
		Ecosystem:     "alpine",
		Name:          name,
		StoragePath:   "_snapshots/alpine/" + name + "/" + digest,
		ContentDigest: digest,
		Size:          1,
		ValidFromMS:   validFrom.UnixMilli(),
	})
	if err != nil {
		t.Fatalf("AddMetadataSnapshot: %v", err)
	}
	return added
}

func snapshotDigestAt(t *testing.T, db *DB, name string, at time.Time) string {
	t.Helper()
	snap, err := db.MetadataSnapshotAt("alpine", name, at)
	if err != nil {
		t.Fatalf("MetadataSnapshotAt: %v", err)
	}
	if snap == nil {
		return ""
	}
	return snap.ContentDigest
}

func TestMetadataSnapshots_AddAndLookup(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		day := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
		if !addSnapshot(t, db, "idx", "sha256:a", day) {
			t.Fatal("first snapshot not added")
		}
		if addSnapshot(t, db, "idx", "sha256:a", day.Add(time.Hour)) {
			t.Error("unchanged digest added again")
		}
		if !addSnapshot(t, db, "idx", "sha256:b", day.Add(48*time.Hour)) {
			t.Fatal("changed digest not added")
		}
		// A rollback to an earlier version is a new snapshot.
		if !addSnapshot(t, db, "idx", "sha256:a", day.Add(96*time.Hour)) {
			t.Fatal("rolled back digest not added")
		}

		for _, tc := range []struct {
			at   time.Time
			want string
		}{
			{day, ""},
			{day.Add(time.Minute), "sha256:a"},
			{day.Add(72 * time.Hour), "sha256:b"},
			{day.Add(100 * time.Hour), "sha256:a"},
		} {
			if got := snapshotDigestAt(t, db, "idx", tc.at); got != tc.want {
				t.Errorf("snapshot at %s = %q, want %q", tc.at, got, tc.want)
			}
		}
		latest, err := db.LatestMetadataSnapshot("alpine", "idx")
		if err != nil || latest == nil || !latest.ValidFrom().Equal(day.Add(96*time.Hour)) {
			t.Errorf("LatestMetadataSnapshot = %+v, %v", latest, err)
		}
	})
}

func TestMetadataSnapshots_Prune(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
		addSnapshot(t, db, "idx", "sha256:a", now.AddDate(0, 0, -100))
		addSnapshot(t, db, "idx", "sha256:b", now.AddDate(0, 0, -60))
		addSnapshot(t, db, "idx", "sha256:c", now.AddDate(0, 0, -10))
		// The only version of another index stays however old it is.
		addSnapshot(t, db, "other", "sha256:x", now.AddDate(0, 0, -200))

		pruned, err := db.PruneMetadataSnapshots("alpine", now.AddDate(0, 0, -30))
		if err != nil {
			t.Fatalf("PruneMetadataSnapshots: %v", err)
		}
		// a was replaced 60 days ago, b only 10 days ago.
		if pruned != 1 {
			t.Errorf("pruned = %d, want 1", pruned)
		}
		if got := snapshotDigestAt(t, db, "idx", now.AddDate(0, 0, -90)); got != "" {
			t.Errorf("snapshot of a pruned day = %q, want none", got)
		}
		if got := snapshotDigestAt(t, db, "idx", now.AddDate(0, 0, -50)); got != "sha256:b" {
			t.Errorf("snapshot 50 days ago = %q, want sha256:b", got)
		}
		if got := snapshotDigestAt(t, db, "other", now); got != "sha256:x" {
			t.Errorf("only snapshot of other = %q, want sha256:x", got)
		}

		due, err := db.GetDuePendingDeletes(time.Now().Add(time.Hour), 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(due) != 1 || due[0] != "_snapshots/alpine/idx/sha256:a" {
			t.Errorf("queued deletes = %v, want the pruned blob", due)
		}
	})
}

// TestMetadataSnapshots_ConcurrentPostgres records a version while another
// replica's transaction has recorded the same one and not committed yet, as
// two replicas can. The second must wait and then find it, so one row
// remains. Without the lock both inserts succeed.
func TestMetadataSnapshots_ConcurrentPostgres(t *testing.T) {
	db := createTestPostgresDB(t)
	if db == nil {
		t.Skip("PROXY_DATABASE_URL not set, skipping postgres test")
	}
	defer func() { _ = db.Close() }()
	const name = "v3.22/main/x86_64/APKINDEX.tar.gz"

	other, err := db.Beginx()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Rollback() }()
	if _, err := other.Exec("SELECT pg_advisory_xact_lock($1)", metadataSnapshotLockKey("alpine", name)); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Exec(`INSERT INTO metadata_snapshots (ecosystem, name, storage_path, content_digest, size, valid_from_ms)
		VALUES ('alpine', $1, 'other', 'sha256:a', 1, $2)`, name, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}

	done := make(chan bool, 1)
	go func() { done <- addSnapshot(t, db, name, "sha256:a", time.Now()) }()
	select {
	case added := <-done:
		t.Fatalf("recorded without waiting for the other replica (added=%v)", added)
	case <-time.After(200 * time.Millisecond):
	}
	if err := other.Commit(); err != nil {
		t.Fatal(err)
	}
	if added := <-done; added {
		t.Error("recorded a version the other replica had just recorded")
	}
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM metadata_snapshots WHERE name = $1`, name); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d snapshot rows, want 1", n)
	}

	// Going back to an earlier digest is still recorded.
	if !addSnapshot(t, db, name, "sha256:b", time.Now()) || !addSnapshot(t, db, name, "sha256:a", time.Now().Add(time.Millisecond)) {
		t.Error("a change back to an earlier digest was not recorded")
	}
}
