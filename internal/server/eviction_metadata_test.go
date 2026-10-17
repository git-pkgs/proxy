package server

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/storage"
)

// seedMetadata caches a 500-byte metadata document last used at
// accessedAt and returns its storage path.
func seedMetadata(t *testing.T, ctx context.Context, db *database.DB, store storage.Storage, key string, accessedAt time.Time) string {
	t.Helper()
	path := "_metadata/golang/" + key + "/metadata"
	size, _, err := store.Store(ctx, path, strings.NewReader(strings.Repeat("m", 500)))
	if err != nil {
		t.Fatalf("storing metadata: %v", err)
	}
	if err := db.UpsertMetadataCache(&database.MetadataCacheEntry{
		Ecosystem:   "golang",
		Name:        key,
		StoragePath: path,
		Size:        sql.NullInt64{Int64: size, Valid: true},
		FetchedAt:   sql.NullTime{Time: time.Now(), Valid: true},
	}); err != nil {
		t.Fatalf("upserting metadata: %v", err)
	}
	query := db.Rebind(`UPDATE metadata_cache SET last_accessed_at = ? WHERE ecosystem = ? AND name = ?`)
	if _, err := db.Exec(query, accessedAt.UTC(), "golang", key); err != nil {
		t.Fatalf("setting last access: %v", err)
	}
	return path
}

func TestEvictLRU_CountsAndEvictsMetadata(t *testing.T) {
	db, store := setupEvictionTest(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := time.Now()

	oldTile := seedMetadata(t, ctx, db, store, "tile-old", now.Add(-3*time.Hour))
	seedArtifact(t, ctx, db, store, "mid-pkg", 500, now.Add(-2*time.Hour))
	newTile := seedMetadata(t, ctx, db, store, "tile-new", now)

	// Artifacts alone are within the limit; with metadata the cache is
	// 1500 bytes, so the least recently used entry has to go, and that is
	// the old metadata document.
	evictLRU(ctx, db, store, logger, 1100)

	if entry, _ := db.GetMetadataCache("golang", "tile-old"); entry != nil {
		t.Error("least recently used metadata row was kept")
	}
	if ok, _ := store.Exists(ctx, oldTile); ok {
		t.Error("least recently used metadata blob was kept")
	}
	if entry, _ := db.GetMetadataCache("golang", "tile-new"); entry == nil {
		t.Error("recently used metadata row was evicted")
	}
	if ok, _ := store.Exists(ctx, newTile); !ok {
		t.Error("recently used metadata blob was deleted")
	}
	art, err := db.GetArtifact("pkg:npm/mid-pkg@1.0.0", "mid-pkg-1.0.0.tgz")
	if err != nil || art == nil || !art.StoragePath.Valid {
		t.Errorf("artifact used after the old metadata was evicted: %+v, %v", art, err)
	}
}

func TestEvictLRU_EvictsArtifactBeforeNewerMetadata(t *testing.T) {
	db, store := setupEvictionTest(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := time.Now()

	seedArtifact(t, ctx, db, store, "old-pkg", 500, now.Add(-3*time.Hour))
	seedMetadata(t, ctx, db, store, "tile", now)

	evictLRU(ctx, db, store, logger, 600)

	art, err := db.GetArtifact("pkg:npm/old-pkg@1.0.0", "old-pkg-1.0.0.tgz")
	if err != nil || art == nil || art.StoragePath.Valid {
		t.Errorf("older artifact kept: %+v, %v", art, err)
	}
	if entry, _ := db.GetMetadataCache("golang", "tile"); entry == nil {
		t.Error("newer metadata evicted although evicting the artifact was enough")
	}
}

// TestEvictBatch_KeepsMetadataUsedSinceRead evicts from a row read before a
// request served the document again. The row and its blob stay, and nothing
// is counted as freed.
func TestEvictBatch_KeepsMetadataUsedSinceRead(t *testing.T) {
	db, store := setupEvictionTest(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := seedMetadata(t, ctx, db, store, "tile", time.Now().Add(-time.Hour))

	stale, err := db.GetLeastRecentlyUsedCacheEntries(evictionBatch)
	if err != nil || len(stale) != 1 {
		t.Fatalf("reading LRU rows: %v, %v", stale, err)
	}
	if err := db.RecordMetadataHit("golang", "tile"); err != nil {
		t.Fatalf("recording hit: %v", err)
	}

	cleared, freed := evictBatch(ctx, db, store, logger, stale, 0, 500)
	if cleared != 0 || freed != 0 {
		t.Errorf("cleared %d records freeing %d bytes, want nothing", cleared, freed)
	}
	if entry, _ := db.GetMetadataCache("golang", "tile"); entry == nil {
		t.Error("metadata row served since it was read was evicted")
	}
	if ok, _ := store.Exists(ctx, path); !ok {
		t.Error("metadata blob served since it was read was deleted")
	}
}
