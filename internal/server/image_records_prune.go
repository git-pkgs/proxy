package server

import (
	"context"
	"time"

	"github.com/git-pkgs/proxy/internal/retention"
)

const (
	// imageRecordPruneInterval is how often container tag and manifest
	// records are pruned.
	imageRecordPruneInterval = time.Hour
	// imageTagMaxAge is how long a tag the upstream has not confirmed is
	// kept, and with it what its image references.
	imageTagMaxAge = 90 * 24 * time.Hour
)

// startImageRecordPruning prunes the container tag and manifest records once
// at startup and then every imageRecordPruneInterval. They are written on
// every install, so this runs whether or not a retention rule is set.
func (s *Server) startImageRecordPruning(ctx context.Context) {
	keep := 0
	if rules, err := s.cfg.RetentionRules(retention.Default); err == nil {
		keep = rules.KeepLatest
	}
	prune := func() {
		s.runExclusive(ctx, "image-record-pruning", func() {
			pruneImageRecords(s.db, s.logger, keep, time.Now())
		})
	}
	s.background.Go(func() {
		prune()
		ticker := time.NewTicker(imageRecordPruneInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				prune()
			}
		}
	})
}
