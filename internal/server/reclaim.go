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
			reclaimStorage(ctx, s.db, s.storage, s.logger, time.Now().Add(-grace))
		}
	}
}

// reclaimStorage deletes up to one batch of objects queued before cutoff. A
// delete that fails stays queued for the next pass to retry.
func reclaimStorage(ctx context.Context, db *database.DB, store storage.Storage, logger *slog.Logger, cutoff time.Time) {
	paths, err := db.GetDuePendingDeletes(cutoff, reclaimBatch)
	if err != nil {
		logger.Warn("reclaim: failed to list pending deletes", "error", err)
		return
	}

	for _, path := range paths {
		if ctx.Err() != nil {
			return
		}
		if err := store.Delete(ctx, path); err != nil {
			logger.Warn("reclaim: failed to delete object, will retry", "path", path, "error", err)
			continue
		}
		if err := db.RemovePendingDelete(path); err != nil {
			logger.Warn("reclaim: failed to dequeue deleted object", "path", path, "error", err)
		}
	}
}
