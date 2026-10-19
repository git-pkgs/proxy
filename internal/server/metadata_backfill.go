package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
)

// metadataBackfillBatch bounds each backfill transaction, so other writers
// get the database in between.
const metadataBackfillBatch = 1000

// backfillMetadataAccess gives metadata rows cached before last_accessed_at
// existed their write time, batch by batch. It is resumable: a restart picks
// up whatever rows are still empty.
func backfillMetadataAccess(ctx context.Context, db *database.DB, logger *slog.Logger, batch int) {
	total := 0
	for ctx.Err() == nil {
		n, err := db.BackfillMetadataLastAccessed(batch)
		if err != nil {
			logger.Warn("metadata backfill failed, retrying", "error", err)
			select {
			case <-ctx.Done():
			case <-time.After(time.Minute):
			}
			continue
		}
		total += n
		if n < batch {
			break
		}
	}
	if total > 0 {
		logger.Info("metadata backfill: completed", "rows", total)
	}
}
