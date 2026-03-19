package mirror

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/storage"
	"github.com/git-pkgs/registries/fetch"
)

// setupTestMirror creates a Mirror with real DB and filesystem storage for integration tests.
func setupTestMirror(t *testing.T, workers int) *Mirror {
	t.Helper()

	dbPath := t.TempDir() + "/test.db"
	db, err := database.Create(dbPath)
	if err != nil {
		t.Fatalf("creating database: %v", err)
	}
	if err := db.MigrateSchema(); err != nil {
		t.Fatalf("migrating schema: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	storeDir := t.TempDir()
	store, err := storage.OpenBucket(context.Background(), "file://"+storeDir)
	if err != nil {
		t.Fatalf("opening storage: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	fetcher := fetch.NewFetcher()
	resolver := fetch.NewResolver()
	proxy := handler.NewProxy(db, store, fetcher, resolver, logger)

	return New(proxy, db, store, logger, workers)
}

const testPackageLodash = "lodash"

func TestMirrorRunEmptySource(t *testing.T) {
	m := setupTestMirror(t, 2)

	source := &PURLSource{PURLs: []string{}}
	progress, err := m.Run(context.Background(), source)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if progress.Total != 0 {
		t.Errorf("total = %d, want 0", progress.Total)
	}
	if progress.Phase != "complete" {
		t.Errorf("phase = %q, want %q", progress.Phase, "complete")
	}
}

func TestMirrorRunDryRun(t *testing.T) {
	m := setupTestMirror(t, 1)

	source := &PURLSource{
		PURLs: []string{
			"pkg:npm/lodash@4.17.21",
			"pkg:cargo/serde@1.0.0",
		},
	}

	items, err := m.RunDryRun(context.Background(), source)
	if err != nil {
		t.Fatalf("RunDryRun() error = %v", err)
	}

	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}

	// Dry run should not modify the database
	stats, err := m.db.GetCacheStats()
	if err != nil {
		t.Fatalf("GetCacheStats() error = %v", err)
	}
	if stats.TotalArtifacts != 0 {
		t.Errorf("artifacts = %d, want 0 (dry run should not cache)", stats.TotalArtifacts)
	}
}

func TestMirrorRunCanceled(t *testing.T) {
	m := setupTestMirror(t, 1)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	// Use a source that produces items but they'll all fail due to canceled context
	source := &PURLSource{
		PURLs: []string{"pkg:npm/lodash@4.17.21"},
	}

	progress, err := m.Run(ctx, source)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// With a canceled context, the fetch should fail
	if progress.Failed != 1 {
		t.Errorf("failed = %d, want 1", progress.Failed)
	}
}

func TestProgressTrackerSnapshot(t *testing.T) {
	pt := newProgressTracker()
	pt.total.Store(10)
	pt.completed.Store(5)
	pt.skipped.Store(3)
	pt.failed.Store(2)
	pt.bytes.Store(1024)
	pt.phase.Store("downloading")
	pt.addError("npm", testPackageLodash, "4.17.21", "fetch failed")

	snap := pt.snapshot()
	if snap.Total != 10 {
		t.Errorf("total = %d, want 10", snap.Total)
	}
	if snap.Completed != 5 {
		t.Errorf("completed = %d, want 5", snap.Completed)
	}
	if snap.Skipped != 3 {
		t.Errorf("skipped = %d, want 3", snap.Skipped)
	}
	if snap.Failed != 2 {
		t.Errorf("failed = %d, want 2", snap.Failed)
	}
	if snap.Bytes != 1024 {
		t.Errorf("bytes = %d, want 1024", snap.Bytes)
	}
	if snap.Phase != "downloading" {
		t.Errorf("phase = %q, want %q", snap.Phase, "downloading")
	}
	if len(snap.Errors) != 1 {
		t.Fatalf("errors = %d, want 1", len(snap.Errors))
	}
	if snap.Errors[0].Name != testPackageLodash {
		t.Errorf("error name = %q, want %q", snap.Errors[0].Name, testPackageLodash)
	}
	if snap.StartedAt.IsZero() {
		t.Error("started_at should not be zero")
	}
}

func TestProgressTrackerConcurrentAccess(t *testing.T) {
	pt := newProgressTracker()
	done := make(chan struct{})

	for range 10 {
		go func() {
			pt.completed.Add(1)
			pt.addError("npm", "test", "1.0.0", "error")
			_ = pt.snapshot()
			done <- struct{}{}
		}()
	}

	timeout := time.After(5 * time.Second)
	for range 10 {
		select {
		case <-done:
		case <-timeout:
			t.Fatal("timed out waiting for goroutines")
		}
	}

	snap := pt.snapshot()
	if snap.Completed != 10 {
		t.Errorf("completed = %d, want 10", snap.Completed)
	}
	if len(snap.Errors) != 10 {
		t.Errorf("errors = %d, want 10", len(snap.Errors))
	}
}

func TestNewMirrorDefaultWorkers(t *testing.T) {
	m := New(nil, nil, nil, slog.Default(), 0)
	if m.workers != 1 {
		t.Errorf("workers = %d, want 1 (minimum)", m.workers)
	}

	m = New(nil, nil, nil, slog.Default(), -5)
	if m.workers != 1 {
		t.Errorf("workers = %d, want 1 (minimum)", m.workers)
	}
}
