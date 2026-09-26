package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/storage"
)

const (
	evictionInterval = 1 * time.Minute
	evictionBatch    = 50
)

func (s *Server) startEvictionLoop(ctx context.Context) {
	maxSize := s.cfg.ParseMaxSize()
	if maxSize <= 0 {
		return
	}

	s.logger.Info("cache eviction enabled", "max_size", s.cfg.Storage.MaxSize)

	ticker := time.NewTicker(evictionInterval)
	defer ticker.Stop()

	s.runEviction(ctx, maxSize)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runEviction(ctx, maxSize)
		}
	}
}

func (s *Server) runEviction(ctx context.Context, maxSize int64) {
	evictLRU(ctx, s.db, s.storage, s.logger, maxSize)
}

func evictLRU(ctx context.Context, db *database.DB, store storage.Storage, logger *slog.Logger, maxSize int64) {
	totalSize, err := db.GetTotalCacheSize()
	if err != nil {
		logger.Warn("eviction: failed to get cache size", "error", err)
		return
	}

	if totalSize <= maxSize {
		return
	}

	logger.Info("eviction: cache size exceeds limit, evicting",
		"current_size", totalSize, "max_size", maxSize)

	evicted := 0
	freedBytes := int64(0)

	for totalSize-freedBytes > maxSize {
		if ctx.Err() != nil {
			break
		}

		artifacts, err := db.GetLeastRecentlyUsedArtifacts(evictionBatch)
		if err != nil {
			logger.Warn("eviction: failed to get LRU artifacts", "error", err)
			break
		}
		if len(artifacts) == 0 {
			break
		}

		cleared, freed := evictBatch(ctx, db, store, logger, artifacts, maxSize, totalSize-freedBytes)
		evicted += cleared
		freedBytes += freed

		if ctx.Err() != nil {
			break
		}

		// A batch that clears nothing returns the same records next time, so
		// stop rather than retry them without end. The next pass retries after
		// the interval.
		if cleared == 0 {
			logger.Warn("eviction: no artifact in this batch could be evicted, ending the pass",
				"batch", len(artifacts))
			break
		}
	}

	if evicted > 0 {
		logger.Info("eviction: completed",
			"evicted", evicted, "freed_bytes", freedBytes)
	}
}

// evictBatch evicts from artifacts until the size still in use falls to
// maxSize, reporting how many records it cleared and how many bytes that
// freed. Progress is reported in records as well as bytes because a cached
// record can have no size recorded, and clearing one is still progress.
func evictBatch(ctx context.Context, db *database.DB, store storage.Storage, logger *slog.Logger,
	artifacts []database.Artifact, maxSize, inUse int64) (int, int64) {
	cleared := 0
	freed := int64(0)

	for _, art := range artifacts {
		if inUse-freed <= maxSize {
			break
		}
		if ctx.Err() != nil {
			break
		}
		if !art.StoragePath.Valid {
			continue
		}

		if err := store.Delete(ctx, art.StoragePath.String); err != nil {
			logger.Warn("eviction: failed to delete from storage",
				"path", art.StoragePath.String, "error", err)
			continue
		}

		recordCleared, err := db.ClearArtifactCache(art.VersionPURL, art.Filename, art.StoragePath.String)
		if err != nil {
			logger.Warn("eviction: failed to clear artifact record",
				"version_purl", art.VersionPURL, "filename", art.Filename, "error", err)
			continue
		}
		if !recordCleared {
			// The record no longer points here, so this delete freed nothing
			// the recorded size counts.
			continue
		}

		if art.Size.Valid {
			freed += art.Size.Int64
		}
		cleared++
	}

	return cleared, freed
}
