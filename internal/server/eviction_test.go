package server

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/storage"
)

func setupEvictionTest(t *testing.T) (*database.DB, *storage.Filesystem) {
	t.Helper()

	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test.db")
	storagePath := filepath.Join(tempDir, "artifacts")

	db, err := database.Create(dbPath)
	if err != nil {
		t.Fatalf("failed to create database: %v", err)
	}

	store, err := storage.NewFilesystem(storagePath)
	if err != nil {
		_ = db.Close()
		t.Fatalf("failed to create storage: %v", err)
	}

	t.Cleanup(func() {
		_ = db.Close()
	})

	return db, store
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

	store, err := storage.NewFilesystem(storagePath)
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
		Storage: config.StorageConfig{Path: storagePath, MaxSize: ""},
		Database: config.DatabaseConfig{
			Driver: "sqlite",
			Path:   dbPath,
		},
		Log: config.LogConfig{Level: "info", Format: "text"},
	}
}
