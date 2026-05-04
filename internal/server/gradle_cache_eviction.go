package server

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/git-pkgs/proxy/internal/storage"
)

const gradleBuildCacheStoragePrefix = "_gradle/http-build-cache/"

type gradleBuildCacheLister interface {
	ListPrefix(ctx context.Context, prefix string) ([]storage.ObjectInfo, error)
}

func (s *Server) startGradleBuildCacheEviction(ctx context.Context) {
	maxAge := s.cfg.ParseGradleBuildCacheMaxAge()
	maxSize := s.cfg.ParseGradleBuildCacheMaxSize()
	if maxAge <= 0 && maxSize <= 0 {
		return
	}

	lister, ok := s.storage.(gradleBuildCacheLister)
	if !ok {
		s.logger.Warn("gradle cache eviction is enabled, but storage backend cannot list objects")
		return
	}

	interval := s.cfg.ParseGradleBuildCacheSweepInterval()
	s.logger.Info("gradle cache eviction enabled",
		"max_age", maxAge,
		"max_size_bytes", maxSize,
		"interval", interval)

	sweep := func() {
		deletedCount, freedBytes, err := sweepGradleBuildCache(ctx, s.storage, lister, maxAge, maxSize, time.Now())
		if err != nil {
			s.logger.Warn("gradle cache eviction sweep failed", "error", err)
			return
		}
		if deletedCount > 0 {
			s.logger.Info("gradle cache eviction sweep completed",
				"deleted_entries", deletedCount,
				"freed_bytes", freedBytes)
		}
	}

	sweep()

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweep()
			}
		}
	}()
}

func sweepGradleBuildCache(
	ctx context.Context,
	store storage.Storage,
	lister gradleBuildCacheLister,
	maxAge time.Duration,
	maxSize int64,
	now time.Time,
) (int, int64, error) {
	entries, err := lister.ListPrefix(ctx, gradleBuildCacheStoragePrefix)
	if err != nil {
		return 0, 0, fmt.Errorf("listing gradle cache entries: %w", err)
	}

	if len(entries) == 0 {
		return 0, 0, nil
	}

	sortOldestFirst(entries)

	deletedCount := 0
	freedBytes := int64(0)
	var firstDeleteErr error

	deleteEntry := func(entry storage.ObjectInfo) bool {
		if err := store.Delete(ctx, entry.Path); err != nil {
			if firstDeleteErr == nil {
				firstDeleteErr = err
			}
			return false
		}
		deletedCount++
		freedBytes += entry.Size
		return true
	}

	remaining := entries
	if maxAge > 0 {
		cutoff := now.Add(-maxAge)
		kept := make([]storage.ObjectInfo, 0, len(entries))

		for _, entry := range entries {
			if !entry.ModTime.IsZero() && entry.ModTime.Before(cutoff) {
				if deleteEntry(entry) {
					continue
				}
			}
			kept = append(kept, entry)
		}

		remaining = kept
	}

	if maxSize > 0 {
		totalSize := int64(0)
		for _, entry := range remaining {
			totalSize += entry.Size
		}

		for _, entry := range remaining {
			if totalSize <= maxSize {
				break
			}
			if deleteEntry(entry) {
				totalSize -= entry.Size
			}
		}
	}

	if firstDeleteErr != nil {
		return deletedCount, freedBytes, fmt.Errorf("deleting gradle cache entries: %w", firstDeleteErr)
	}

	return deletedCount, freedBytes, nil
}

func sortOldestFirst(entries []storage.ObjectInfo) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].ModTime.Equal(entries[j].ModTime) {
			return entries[i].Path < entries[j].Path
		}
		return entries[i].ModTime.Before(entries[j].ModTime)
	})
}
