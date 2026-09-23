package database

import (
	"database/sql"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

const (
	pendingVersionPURL = "pkg:npm/pending@1.0.0"
	pendingFilename    = "pending-1.0.0.tgz"
)

func upsertPendingArtifact(t *testing.T, db *DB, storagePath string) {
	t.Helper()
	a := &Artifact{
		VersionPURL: pendingVersionPURL,
		Filename:    pendingFilename,
		UpstreamURL: "https://example.com/" + pendingFilename,
		StoragePath: sql.NullString{String: storagePath, Valid: true},
		Size:        sql.NullInt64{Int64: 1, Valid: true},
		FetchedAt:   sql.NullTime{Time: time.Now(), Valid: true},
	}
	if err := db.UpsertArtifact(a); err != nil {
		t.Fatalf("UpsertArtifact(%q): %v", storagePath, err)
	}
}

func recordedPath(t *testing.T, db *DB) sql.NullString {
	t.Helper()
	a, err := db.GetArtifact(pendingVersionPURL, pendingFilename)
	if err != nil || a == nil {
		t.Fatalf("GetArtifact: %v, %v", a, err)
	}
	return a.StoragePath
}

// allPendingDeletes returns every queued path, due or not.
func allPendingDeletes(t *testing.T, db *DB) []string {
	t.Helper()
	paths, err := db.GetDuePendingDeletes(time.Now().Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("GetDuePendingDeletes: %v", err)
	}
	return paths
}

func TestUpsertArtifactQueuesThePathItReplaces(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		upsertPendingArtifact(t, db, "npm/pending/1.0.0/a/pending-1.0.0.tgz")
		upsertPendingArtifact(t, db, "npm/pending/1.0.0/a/pending-1.0.0.tgz")
		if got := allPendingDeletes(t, db); len(got) != 0 {
			t.Fatalf("queued %v, want nothing while the record keeps its path", got)
		}

		upsertPendingArtifact(t, db, "npm/pending/1.0.0/b/pending-1.0.0.tgz")
		want := []string{"npm/pending/1.0.0/a/pending-1.0.0.tgz"}
		if got := allPendingDeletes(t, db); !slices.Equal(got, want) {
			t.Errorf("queued %v, want %v", got, want)
		}
	})
}

// TestUpsertArtifactSkipsRecordThatMoved is the race UpsertArtifact retries:
// another commit moved the record after this one read it, so the write must
// not apply, or the path that commit stored would never be queued.
func TestUpsertArtifactSkipsRecordThatMoved(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		upsertPendingArtifact(t, db, "npm/pending/1.0.0/a/pending-1.0.0.tgz")
		read := recordedPath(t, db)
		upsertPendingArtifact(t, db, "npm/pending/1.0.0/b/pending-1.0.0.tgz")

		late := &Artifact{
			VersionPURL: pendingVersionPURL,
			Filename:    pendingFilename,
			UpstreamURL: "https://example.com/" + pendingFilename,
			StoragePath: sql.NullString{String: "npm/pending/1.0.0/c/pending-1.0.0.tgz", Valid: true},
		}
		applied, err := db.upsertArtifactFrom(late, read)
		if err != nil {
			t.Fatalf("upsertArtifactFrom: %v", err)
		}
		if applied {
			t.Error("write applied over a record that moved since it was read")
		}
		if got := recordedPath(t, db).String; got != "npm/pending/1.0.0/b/pending-1.0.0.tgz" {
			t.Errorf("record points at %q, want the newer commit kept", got)
		}

		missing := &Artifact{VersionPURL: "pkg:npm/pending@2.0.0", Filename: pendingFilename, UpstreamURL: "u"}
		applied, err = db.upsertArtifactFrom(missing, sql.NullString{})
		if err != nil || !applied {
			t.Errorf("insert of a new record: applied=%v err=%v", applied, err)
		}
	})
}

// TestConcurrentUpsertsQueueEveryReplacedPath commits one artifact from several
// fetches at once: whichever commit ends up recorded, every other path must be
// queued, or its object is never deleted.
func TestConcurrentUpsertsQueueEveryReplacedPath(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		// A commit only retries after another commit succeeded, so this many
		// always fit within upsertArtifactAttempts.
		const commits = upsertArtifactAttempts - 1
		paths := make([]string, commits)
		errs := make(chan error, commits)
		var wg sync.WaitGroup
		for i := range paths {
			paths[i] = fmt.Sprintf("npm/pending/1.0.0/%d/pending-1.0.0.tgz", i)
			wg.Go(func() {
				errs <- db.UpsertArtifact(&Artifact{
					VersionPURL: pendingVersionPURL,
					Filename:    pendingFilename,
					UpstreamURL: "https://example.com/" + pendingFilename,
					StoragePath: sql.NullString{String: paths[i], Valid: true},
				})
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("UpsertArtifact: %v", err)
			}
		}

		recorded := recordedPath(t, db).String
		want := slices.DeleteFunc(slices.Clone(paths), func(p string) bool { return p == recorded })
		got := allPendingDeletes(t, db)
		slices.Sort(want)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("recorded %q, queued %v, want %v", recorded, got, want)
		}
	})
}

func TestClearArtifactCacheLeavesRecordThatMoved(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		upsertPendingArtifact(t, db, "npm/pending/1.0.0/a/pending-1.0.0.tgz")
		upsertPendingArtifact(t, db, "npm/pending/1.0.0/b/pending-1.0.0.tgz")

		if cleared, err := db.ClearArtifactCache(pendingVersionPURL, pendingFilename, "npm/pending/1.0.0/a/pending-1.0.0.tgz"); err != nil || cleared {
			t.Fatalf("ClearArtifactCache: cleared=%v err=%v, want nothing cleared", cleared, err)
		}
		if err := db.DiscardArtifact(pendingVersionPURL, pendingFilename, "npm/pending/1.0.0/a/pending-1.0.0.tgz"); err != nil {
			t.Fatalf("DiscardArtifact: %v", err)
		}
		if got := recordedPath(t, db).String; got != "npm/pending/1.0.0/b/pending-1.0.0.tgz" {
			t.Errorf("record points at %q, want the newer commit kept", got)
		}
	})
}

func TestDiscardArtifactClearsAndQueues(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		upsertPendingArtifact(t, db, "npm/pending/1.0.0/a/pending-1.0.0.tgz")

		if err := db.DiscardArtifact(pendingVersionPURL, pendingFilename, "npm/pending/1.0.0/a/pending-1.0.0.tgz"); err != nil {
			t.Fatalf("DiscardArtifact: %v", err)
		}
		if got := recordedPath(t, db); got.Valid {
			t.Errorf("record still points at %q", got.String)
		}
		want := []string{"npm/pending/1.0.0/a/pending-1.0.0.tgz"}
		if got := allPendingDeletes(t, db); !slices.Equal(got, want) {
			t.Errorf("queued %v, want %v", got, want)
		}
	})
}

func TestGetDuePendingDeletes(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		for _, path := range []string{"old", "referenced"} {
			if err := db.QueuePendingDelete(path); err != nil {
				t.Fatalf("QueuePendingDelete: %v", err)
			}
		}
		upsertPendingArtifact(t, db, "referenced")

		if got, err := db.GetDuePendingDeletes(time.Now().Add(-time.Hour), 100); err != nil || len(got) != 0 {
			t.Errorf("before the grace period: got %v (err %v), want nothing", got, err)
		}
		if got := allPendingDeletes(t, db); !slices.Equal(got, []string{"old"}) {
			t.Errorf("after the grace period: got %v, want [old] and not the path a record points at", got)
		}

		if err := db.RemovePendingDelete("old"); err != nil {
			t.Fatalf("RemovePendingDelete: %v", err)
		}
		if got := allPendingDeletes(t, db); len(got) != 0 {
			t.Errorf("after removal: got %v", got)
		}
	})
}
