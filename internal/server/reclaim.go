package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/storage"
)

// An object no record points at any more waits in the pending_deletes queue
// for a grace period before it is deleted, so a request that read the record
// before it changed can still open the object, and a signed URL to it stays
// valid. Reclaim runs whether or not a cache size limit is set.
const (
	reclaimInterval = 1 * time.Minute
	reclaimBatch    = 100
	reclaimMinGrace = 1 * time.Hour
	// reclaimTickBudget bounds how long one tick keeps deleting batches, so
	// a long queue, such as one left by a retention sweep, drains faster than
	// one batch per interval without one tick running on indefinitely.
	reclaimTickBudget = 30 * time.Second
)

func (s *Server) startReclaimLoop(ctx context.Context) {
	grace := max(reclaimMinGrace, s.cfg.ParseDirectServeTTL())

	ticker := time.NewTicker(reclaimInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reclaimDue(ctx, s.db, s.storage, s.logger, time.Now().Add(-grace), reclaimTickBudget)
		}
	}
}

// reclaimDue deletes batches of objects queued before cutoff until a batch
// comes back short or budget has passed.
func reclaimDue(ctx context.Context, db *database.DB, store storage.Storage, logger *slog.Logger,
	cutoff time.Time, budget time.Duration) {
	start := time.Now()
	for ctx.Err() == nil && time.Since(start) < budget {
		if reclaimStorage(ctx, db, store, logger, cutoff) < reclaimBatch {
			return
		}
	}
}

// reclaimStorage deletes up to one batch of objects queued before cutoff and
// reports how many it listed. A delete that fails is queued again, behind the
// rest, so objects the backend keeps refusing cannot fill every batch.
func reclaimStorage(ctx context.Context, db *database.DB, store storage.Storage, logger *slog.Logger, cutoff time.Time) int {
	paths, err := db.GetDuePendingDeletes(cutoff, reclaimBatch)
	if err != nil {
		logger.Warn("reclaim: failed to list pending deletes", "error", err)
		return 0
	}

	for _, path := range paths {
		if ctx.Err() != nil {
			return len(paths)
		}
		if err := store.Delete(ctx, path); err != nil {
			if ctx.Err() != nil {
				return len(paths)
			}
			logger.Warn("reclaim: failed to delete object, will retry", "path", path, "error", err)
			if err := db.QueuePendingDelete(path); err != nil {
				logger.Warn("reclaim: failed to requeue object", "path", path, "error", err)
			}
			continue
		}
		if err := db.RemovePendingDelete(path); err != nil {
			logger.Warn("reclaim: failed to dequeue deleted object", "path", path, "error", err)
		}
	}
	return len(paths)
}
