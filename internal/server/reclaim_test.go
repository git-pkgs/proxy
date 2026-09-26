package server

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/storage"
)

func storeQueued(t *testing.T, db *database.DB, store storage.Storage, path string) {
	t.Helper()
	if _, _, err := store.Store(context.Background(), path, strings.NewReader("superseded")); err != nil {
		t.Fatalf("storing %s: %v", path, err)
	}
	if err := db.QueuePendingDelete(path); err != nil {
		t.Fatalf("queueing %s: %v", path, err)
	}
}

func queuedPaths(t *testing.T, db *database.DB) []string {
	t.Helper()
	paths, err := db.GetDuePendingDeletes(time.Now().Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("listing queue: %v", err)
	}
	return paths
}

func objectExists(t *testing.T, store storage.Storage, path string) bool {
	t.Helper()
	ok, err := store.Exists(context.Background(), path)
	if err != nil {
		t.Fatalf("checking %s: %v", path, err)
	}
	return ok
}

func TestReclaimStorageWaitsForGracePeriod(t *testing.T) {
	db, store := setupEvictionTest(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	const path = "npm/old/1.0.0/a/old-1.0.0.tgz"
	storeQueued(t, db, store, path)

	reclaimStorage(context.Background(), db, store, logger, time.Now().Add(-time.Hour))
	if !objectExists(t, store, path) {
		t.Fatal("object deleted before its grace period ended")
	}

	reclaimStorage(context.Background(), db, store, logger, time.Now().Add(time.Hour))
	if objectExists(t, store, path) {
		t.Error("object survived after its grace period ended")
	}
	if got := queuedPaths(t, db); len(got) != 0 {
		t.Errorf("queue = %v after reclaim, want empty", got)
	}
}

func TestReclaimStorageKeepsFailedDeletesQueued(t *testing.T) {
	db, store := setupEvictionTest(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	const path = "npm/old/1.0.0/a/old-1.0.0.tgz"
	storeQueued(t, db, store, path)

	undeletable := &undeletableStorage{Storage: store}
	reclaimStorage(context.Background(), db, undeletable, logger, time.Now().Add(time.Hour))

	if got := undeletable.deletes.Load(); got != 1 {
		t.Errorf("delete attempts = %d, want 1", got)
	}
	if got := queuedPaths(t, db); !slices.Equal(got, []string{path}) {
		t.Errorf("queue = %v, want the failed path kept for retry", got)
	}
}

func TestReclaimStorageStopsWhenContextCanceled(t *testing.T) {
	db, store := setupEvictionTest(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	const path = "npm/old/1.0.0/a/old-1.0.0.tgz"
	storeQueued(t, db, store, path)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reclaimStorage(ctx, db, store, logger, time.Now().Add(time.Hour))

	if !objectExists(t, store, path) {
		t.Error("object deleted after the context was canceled")
	}
	if got := queuedPaths(t, db); !slices.Equal(got, []string{path}) {
		t.Errorf("queue = %v, want the path kept", got)
	}
}
