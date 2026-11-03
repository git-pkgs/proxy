package server

import (
	"context"
	"time"
)

// apkSnapshotPruneInterval is how often snapshots kept past
// upstream.apk_snapshots are deleted.
const apkSnapshotPruneInterval = time.Hour

// startAPKSnapshotPruning deletes APK index snapshots that were replaced more
// than upstream.apk_snapshots ago, once at startup and then every hour. Their
// blobs go through the pending-delete queue like evicted cache entries.
func (s *Server) startAPKSnapshotPruning(ctx context.Context) {
	keep := s.cfg.ParseAPKSnapshots()
	if keep <= 0 {
		return
	}
	if !s.cfg.CacheMetadata {
		s.logger.Warn("upstream.apk_snapshots needs cache_metadata, snapshots disabled")
		return
	}

	ticker := time.NewTicker(apkSnapshotPruneInterval)
	defer ticker.Stop()
	for {
		s.pruneAPKSnapshots(ctx, keep, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) pruneAPKSnapshots(ctx context.Context, keep time.Duration, now time.Time) {
	s.runExclusive(ctx, "apk-snapshot-prune", func() {
		n, err := s.db.PruneMetadataSnapshots("alpine", now.Add(-keep))
		if err != nil {
			s.logger.Warn("apk snapshots: prune failed", "error", err)
			return
		}
		if n > 0 {
			s.logger.Info("apk snapshots: pruned", "deleted", n)
		}
	})
}
