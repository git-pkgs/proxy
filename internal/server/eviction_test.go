package server

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/storage"
)

func setupEvictionTest(t *testing.T) (*database.DB, *storage.Blob) {
	t.Helper()

	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test.db")
	storagePath := filepath.Join(tempDir, "artifacts")

	db, err := database.Create(dbPath)
	if err != nil {
		t.Fatalf("failed to create database: %v", err)
	}

	store, err := storage.OpenBucket(context.Background(), "file://"+storagePath)
	if err != nil {
		_ = db.Close()
		t.Fatalf("failed to create storage: %v", err)
	}
	blob, ok := store.(*storage.Blob)
	if !ok {
		_ = db.Close()
		t.Fatalf("OpenBucket returned %T, want *storage.Blob", store)
	}

	t.Cleanup(func() {
		_ = db.Close()
	})

	return db, blob
}

func seedArtifact(t *testing.T, ctx context.Context, db *database.DB, store storage.Storage, name string, dataSize int, accessedAt time.Time) {
	t.Helper()

	pkgPURL := "pkg:npm/" + name
	versionPURL := pkgPURL + "@1.0.0"
	filename := name + "-1.0.0.tgz"

	if err := db.UpsertPackage(&database.Package{
		PURL:      pkgPURL,
		Ecosystem: "npm",
		Name:      name,
	}); err != nil {
		t.Fatalf("failed to upsert package: %v", err)
	}

	if err := db.UpsertVersion(&database.Version{
		PURL:        versionPURL,
		PackagePURL: pkgPURL,
	}); err != nil {
		t.Fatalf("failed to upsert version: %v", err)
	}

	storagePath := storage.ArtifactPath("npm", "", name, "1.0.0", filename)
	data := strings.NewReader(strings.Repeat("x", dataSize))
	size, hash, err := store.Store(ctx, storagePath, data)
	if err != nil {
		t.Fatalf("failed to store artifact: %v", err)
	}

	if err := db.UpsertArtifact(&database.Artifact{
		VersionPURL:    versionPURL,
		Filename:       filename,
		UpstreamURL:    "https://example.com/" + filename,
		StoragePath:    sql.NullString{String: storagePath, Valid: true},
		ContentHash:    sql.NullString{String: hash, Valid: true},
		Size:           sql.NullInt64{Int64: size, Valid: true},
		ContentType:    sql.NullString{String: "application/gzip", Valid: true},
		FetchedAt:      sql.NullTime{Time: time.Now(), Valid: true},
		LastAccessedAt: sql.NullTime{Time: accessedAt, Valid: true},
	}); err != nil {
		t.Fatalf("failed to upsert artifact: %v", err)
	}
}

func TestEvictLRU_NoEvictionWhenUnderLimit(t *testing.T) {
	db, store := setupEvictionTest(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	seedArtifact(t, ctx, db, store, "pkg-a", 100, time.Now())

	evictLRU(ctx, db, store, logger, 1024)

	count, err := db.GetCachedArtifactCount()
	if err != nil {
		t.Fatalf("failed to get count: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 cached artifact, got %d", count)
	}
}

func TestEvictLRU_EvictsOldestFirst(t *testing.T) {
	db, store := setupEvictionTest(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	now := time.Now()
	seedArtifact(t, ctx, db, store, "old-pkg", 500, now.Add(-3*time.Hour))
	seedArtifact(t, ctx, db, store, "mid-pkg", 500, now.Add(-1*time.Hour))
	seedArtifact(t, ctx, db, store, "new-pkg", 500, now)

	// Total is 1500 bytes, limit to 1100 so only the oldest gets evicted
	evictLRU(ctx, db, store, logger, 1100)

	// old-pkg should be evicted
	art, err := db.GetArtifact("pkg:npm/old-pkg@1.0.0", "old-pkg-1.0.0.tgz")
	if err != nil {
		t.Fatalf("failed to get artifact: %v", err)
	}
	if art.StoragePath.Valid {
		t.Error("expected old-pkg to be evicted (storage_path should be NULL)")
	}

	// mid-pkg and new-pkg should remain
	art, err = db.GetArtifact("pkg:npm/mid-pkg@1.0.0", "mid-pkg-1.0.0.tgz")
	if err != nil {
		t.Fatalf("failed to get artifact: %v", err)
	}
	if !art.StoragePath.Valid {
		t.Error("expected mid-pkg to remain cached")
	}

	art, err = db.GetArtifact("pkg:npm/new-pkg@1.0.0", "new-pkg-1.0.0.tgz")
	if err != nil {
		t.Fatalf("failed to get artifact: %v", err)
	}
	if !art.StoragePath.Valid {
		t.Error("expected new-pkg to remain cached")
	}

	// Storage file should be removed for old-pkg
	storagePath := storage.ArtifactPath("npm", "", "old-pkg", "1.0.0", "old-pkg-1.0.0.tgz")
	exists, _ := store.Exists(ctx, storagePath)
	if exists {
		t.Error("expected old-pkg file to be deleted from storage")
	}
}

func TestEvictLRU_EvictsMultipleToGetUnderLimit(t *testing.T) {
	db, store := setupEvictionTest(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	now := time.Now()
	seedArtifact(t, ctx, db, store, "pkg-1", 400, now.Add(-4*time.Hour))
	seedArtifact(t, ctx, db, store, "pkg-2", 400, now.Add(-3*time.Hour))
	seedArtifact(t, ctx, db, store, "pkg-3", 400, now.Add(-2*time.Hour))
	seedArtifact(t, ctx, db, store, "pkg-4", 400, now)

	// Total is 1600 bytes, limit to 900 so pkg-1 and pkg-2 get evicted
	evictLRU(ctx, db, store, logger, 900)

	count, err := db.GetCachedArtifactCount()
	if err != nil {
		t.Fatalf("failed to get count: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 cached artifacts remaining, got %d", count)
	}

	// Verify the right ones remain
	for _, name := range []string{"pkg-3", "pkg-4"} {
		art, err := db.GetArtifact("pkg:npm/"+name+"@1.0.0", name+"-1.0.0.tgz")
		if err != nil {
			t.Fatalf("failed to get artifact %s: %v", name, err)
		}
		if !art.StoragePath.Valid {
			t.Errorf("expected %s to remain cached", name)
		}
	}
}

func TestEvictLRU_NothingToEvictWhenEmpty(t *testing.T) {
	db, store := setupEvictionTest(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Should not panic or error with no artifacts
	evictLRU(ctx, db, store, logger, 1024)

	count, err := db.GetCachedArtifactCount()
	if err != nil {
		t.Fatalf("failed to get count: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 cached artifacts, got %d", count)
	}
}

func TestEvictLRU_StorageFileDeleted(t *testing.T) {
	db, store := setupEvictionTest(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	seedArtifact(t, ctx, db, store, "delete-me", 1000, time.Now().Add(-1*time.Hour))

	storagePath := storage.ArtifactPath("npm", "", "delete-me", "1.0.0", "delete-me-1.0.0.tgz")
	exists, _ := store.Exists(ctx, storagePath)
	if !exists {
		t.Fatal("expected artifact file to exist before eviction")
	}

	evictLRU(ctx, db, store, logger, 500)

	exists, _ = store.Exists(ctx, storagePath)
	if exists {
		t.Error("expected artifact file to be deleted after eviction")
	}

	art, err := db.GetArtifact("pkg:npm/delete-me@1.0.0", "delete-me-1.0.0.tgz")
	if err != nil {
		t.Fatalf("failed to get artifact: %v", err)
	}
	if art.StoragePath.Valid {
		t.Error("expected storage_path to be NULL after eviction")
	}
	if art.Size.Valid {
		t.Error("expected size to be NULL after eviction")
	}
}

func TestStartEvictionLoop_UnlimitedSkips(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test.db")
	storagePath := filepath.Join(tempDir, "artifacts")

	db, err := database.Create(dbPath)
	if err != nil {
		t.Fatalf("failed to create database: %v", err)
	}
	defer func() { _ = db.Close() }()

	store, err := storage.OpenBucket(context.Background(), "file://"+storagePath)
	if err != nil {
		t.Fatalf("failed to create storage: %v", err)
	}

	cfg := defaultTestConfig(storagePath, dbPath)

	s := &Server{
		cfg:     cfg,
		db:      db,
		storage: store,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// Should return immediately since max_size is empty (unlimited)
	done := make(chan struct{})
	go func() {
		s.startEvictionLoop(ctx)
		close(done)
	}()

	select {
	case <-done:
		// Good, returned immediately
	case <-time.After(1 * time.Second):
		t.Error("startEvictionLoop should return immediately when max_size is unlimited")
		cancel()
	}
}

func defaultTestConfig(storagePath, dbPath string) *config.Config {
	return &config.Config{
		Listen:  ":8080",
		BaseURL: "http://localhost:8080",
		Storage: config.StorageConfig{URL: "file://" + storagePath, MaxSize: ""},
		Database: config.DatabaseConfig{
			Driver: "sqlite",
			Path:   dbPath,
		},
		Log: config.LogConfig{Level: "info", Format: "text"},
	}
}

// undeletableStorage is a backend whose Delete always fails, standing in for a
// backend that refuses deletion: a permissions problem, or Windows while a
// reader holds the file open.
type undeletableStorage struct {
	storage.Storage
	deletes atomic.Int64
}

func (u *undeletableStorage) Delete(_ context.Context, _ string) error {
	u.deletes.Add(1)
	return errors.New("storage refused the delete")
}

// runEvictionWithDeadline fails the test if a pass does not return, rather than
// hanging until the package timeout.
func runEvictionWithDeadline(t *testing.T, ctx context.Context, db *database.DB, store storage.Storage, maxSize int64) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	done := make(chan struct{})
	go func() {
		defer close(done)
		evictLRU(ctx, db, store, logger, maxSize)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("evictLRU never returned: it is retrying records it cannot evict")
	}
}

// TestEvictLRU_EndsPassWhenNothingCanBeEvicted is the loop that would otherwise
// never end. Records that fail to delete stay eligible, so the same batch comes
// back forever while the recorded size never drops.
func TestEvictLRU_EndsPassWhenNothingCanBeEvicted(t *testing.T) {
	db, store := setupEvictionTest(t)
	ctx := context.Background()

	now := time.Now()
	seedArtifact(t, ctx, db, store, "old-pkg", 500, now.Add(-2*time.Hour))
	seedArtifact(t, ctx, db, store, "new-pkg", 500, now)

	undeletable := &undeletableStorage{Storage: store}
	runEvictionWithDeadline(t, ctx, db, undeletable, 100)

	// One attempt per record, then the pass ends. Both records survive for the
	// next pass to retry.
	if got := undeletable.deletes.Load(); got != 2 {
		t.Errorf("delete attempts = %d, want 2: one per record in the single batch", got)
	}
	count, err := db.GetCachedArtifactCount()
	if err != nil {
		t.Fatalf("failed to get count: %v", err)
	}
	if count != 2 {
		t.Errorf("cached artifacts = %d, want 2: a failed delete must not clear the record", count)
	}
}

// TestEvictLRU_StopsWhenContextCanceled covers shutdown: a canceled context can
// make every delete fail instantly, which is the fastest way to spin.
func TestEvictLRU_StopsWhenContextCanceled(t *testing.T) {
	db, store := setupEvictionTest(t)
	seedArtifact(t, context.Background(), db, store, "old-pkg", 500, time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	undeletable := &undeletableStorage{Storage: store}
	runEvictionWithDeadline(t, ctx, db, undeletable, 100)

	if got := undeletable.deletes.Load(); got != 0 {
		t.Errorf("delete attempts = %d, want 0 once the context is canceled", got)
	}
}

// TestEvictLRU_EvictsArtifactWithoutRecordedSize covers progress counted in
// records rather than bytes: a record with no size frees nothing, but evicting
// it is still progress and the pass must go on to the next record.
func TestEvictLRU_EvictsArtifactWithoutRecordedSize(t *testing.T) {
	db, store := setupEvictionTest(t)
	ctx := context.Background()

	now := time.Now()
	seedArtifact(t, ctx, db, store, "sizeless-pkg", 500, now.Add(-2*time.Hour))
	seedArtifact(t, ctx, db, store, "sized-pkg", 500, now)
	clearRecordedSize(t, db, "pkg:npm/sizeless-pkg@1.0.0", "sizeless-pkg-1.0.0.tgz")

	// The sizeless record is the oldest, so it is evicted first and frees
	// nothing. Only evicting the second record gets under the limit.
	runEvictionWithDeadline(t, ctx, db, store, 100)

	count, err := db.GetCachedArtifactCount()
	if err != nil {
		t.Fatalf("failed to get count: %v", err)
	}
	if count != 0 {
		t.Errorf("cached artifacts = %d, want 0: the pass must continue past a record that frees nothing", count)
	}
}

func clearRecordedSize(t *testing.T, db *database.DB, versionPURL, filename string) {
	t.Helper()
	query := db.Rebind(`UPDATE artifacts SET size = NULL WHERE version_purl = ? AND filename = ?`)
	if _, err := db.Exec(query, versionPURL, filename); err != nil {
		t.Fatalf("clearing recorded size: %v", err)
	}
}

// TestEvictBatch_SkipsRecordThatMoved evicts from a row read before a newer
// fetch moved the record. Only the old object goes, and nothing is counted as
// freed, since the size in use is unchanged and counting it would end the pass
// while the cache is still over its limit.
func TestEvictBatch_SkipsRecordThatMoved(t *testing.T) {
	db, store := setupEvictionTest(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	seedArtifact(t, ctx, db, store, "moved", 1000, time.Now().Add(-time.Hour))

	stale, err := db.GetLeastRecentlyUsedArtifacts(evictionBatch)
	if err != nil || len(stale) != 1 {
		t.Fatalf("reading LRU rows: %v, %v", stale, err)
	}
	newer := storage.FetchPath("npm", "moved", "1.0.0", storage.NewFetchID(), "moved-1.0.0.tgz")
	if _, _, err := store.Store(ctx, newer, strings.NewReader("refetched")); err != nil {
		t.Fatalf("storing refetch: %v", err)
	}
	moved := stale[0]
	moved.StoragePath = sql.NullString{String: newer, Valid: true}
	if err := db.UpsertArtifact(&moved); err != nil {
		t.Fatalf("moving record: %v", err)
	}

	cleared, freed := evictBatch(ctx, db, store, logger, stale, 0, 1000)
	if cleared != 0 || freed != 0 {
		t.Errorf("cleared %d records freeing %d bytes, want nothing counted", cleared, freed)
	}
	record, err := db.GetArtifact(moved.VersionPURL, moved.Filename)
	if err != nil || record == nil || record.StoragePath.String != newer {
		t.Fatalf("record = %+v (err %v), want it kept at %q", record, err, newer)
	}
	if ok, _ := store.Exists(ctx, newer); !ok {
		t.Error("the newer fetch's object was deleted")
	}
}
