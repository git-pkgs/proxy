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
		artifacts, err := db.GetLeastRecentlyUsedArtifacts(evictionBatch)
		if err != nil {
			logger.Warn("eviction: failed to get LRU artifacts", "error", err)
			return
		}
		if len(artifacts) == 0 {
			break
		}

		for _, art := range artifacts {
			if totalSize-freedBytes <= maxSize {
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

			if err := db.ClearArtifactCache(art.VersionPURL, art.Filename); err != nil {
				logger.Warn("eviction: failed to clear artifact record",
					"version_purl", art.VersionPURL, "filename", art.Filename, "error", err)
				continue
			}

			size := int64(0)
			if art.Size.Valid {
				size = art.Size.Int64
			}
			freedBytes += size
			evicted++
		}
	}

	if evicted > 0 {
		logger.Info("eviction: completed",
			"evicted", evicted, "freed_bytes", freedBytes)
	}
}
