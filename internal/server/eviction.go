package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/metrics"
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

// evictLRU removes the least recently used cached artifacts and metadata
// documents until what max_size counts falls to maxSize.
func evictLRU(ctx context.Context, db *database.DB, store storage.Storage, logger *slog.Logger, maxSize int64) {
	totalSize, err := db.GetEvictableCacheSize()
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

		entries, err := db.GetLeastRecentlyUsedCacheEntries(evictionBatch)
		if err != nil {
			logger.Warn("eviction: failed to get LRU entries", "error", err)
			break
		}
		if len(entries) == 0 {
			break
		}

		cleared, freed := evictBatch(ctx, db, store, logger, entries, maxSize, totalSize-freedBytes)
		evicted += cleared
		freedBytes += freed

		if ctx.Err() != nil {
			break
		}

		// A batch that clears nothing returns the same records next time, so
		// stop rather than retry them without end. The next pass retries after
		// the interval.
		if cleared == 0 {
			logger.Warn("eviction: no entry in this batch could be evicted, ending the pass",
				"batch", len(entries))
			break
		}
	}

	if evicted > 0 {
		logger.Info("eviction: completed",
			"evicted", evicted, "freed_bytes", freedBytes)
	}
}

// evictBatch evicts from entries until the size still in use falls to
// maxSize, reporting how many records it cleared and how many bytes that
// freed. Progress is reported in records as well as bytes because a cached
// record can have no size recorded, and clearing one is still progress.
func evictBatch(ctx context.Context, db *database.DB, store storage.Storage, logger *slog.Logger,
	entries []database.CacheEntry, maxSize, inUse int64) (int, int64) {
	cleared := 0
	freed := int64(0)

	for _, entry := range entries {
		if inUse-freed <= maxSize {
			break
		}
		if ctx.Err() != nil {
			break
		}
		if !entry.StoragePath.Valid {
			continue
		}

		path := entry.StoragePath.String

		// Clear before deleting: a record a newer fetch moved has left this
		// path queued, and a request may still open it within the grace period.
		recordCleared, err := clearEntry(db, logger, entry)
		if err != nil {
			logger.Warn("eviction: failed to clear record",
				"kind", entry.Kind, "key", entry.Key, "name", entry.Name, "error", err)
			continue
		}
		if !recordCleared {
			continue
		}

		// Metadata goes now too: readers hold the whole document in memory,
		// and a row refetched meanwhile refetches again on a missing blob.
		if err := store.Delete(ctx, path); err != nil {
			logger.Warn("eviction: failed to delete from storage, queueing it", "path", path, "error", err)
			if err := db.QueuePendingDelete(path); err != nil {
				logger.Warn("eviction: failed to queue object for deletion", "path", path, "error", err)
			}
		} else if entry.Kind == database.CacheEntryMetadata {
			// Queued with the row's delete in case the process stopped here.
			if err := db.RemovePendingDelete(path); err != nil {
				logger.Warn("eviction: failed to dequeue deleted object", "path", path, "error", err)
			}
		}

		if entry.Size.Valid {
			freed += entry.Size.Int64
		}
		cleared++
	}

	return cleared, freed
}

// clearEntry drops the record of entry if it still points at the path read
// for it, and counts the eviction.
func clearEntry(db *database.DB, logger *slog.Logger, entry database.CacheEntry) (bool, error) {
	if entry.Kind == database.CacheEntryMetadata {
		cleared, err := db.EvictMetadataCache(entry.Key, entry.Name, entry.StoragePath.String, entry.LastAccessedAt)
		if cleared {
			metrics.RecordMetadataEvicted(entry.Key)
		}
		return cleared, err
	}
	cleared, err := db.ClearArtifactCache(entry.Key, entry.Name, entry.StoragePath.String)
	if cleared {
		recordLRUEviction(db, logger, entry.Key)
	}
	return cleared, err
}
