package server

import (
	"context"
	"time"
)

// projectPruneInterval is how often projects older than projects.max_age
// are removed.
const projectPruneInterval = time.Hour

// startProjectPruning removes projects that have not uploaded an SBOM for
// projects.max_age, once at startup and then every projectPruneInterval.
func (s *Server) startProjectPruning(ctx context.Context) {
	maxAge := s.cfg.Projects.MaxAgeDuration()
	if !s.cfg.Projects.Enabled || maxAge <= 0 {
		return
	}
	prune := func() {
		s.runExclusive(ctx, "project-pruning", func() {
			pruned, err := s.db.PruneProjects(time.Now().Add(-maxAge))
			if err != nil {
				s.logger.Warn("pruning projects failed", "error", err)
			}
			if pruned > 0 {
				s.logger.Info("pruned projects", "count", pruned, "max_age", maxAge)
			}
		})
	}
	s.background.Go(func() {
		prune()
		ticker := time.NewTicker(projectPruneInterval)
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
