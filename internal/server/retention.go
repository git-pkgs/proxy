package server

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/metrics"
	"github.com/git-pkgs/proxy/internal/retention"
)

// retentionWindow is how many artifact ids one query scans.
const retentionWindow = 5000

// retentionMaxClears caps how many artifacts one sweep clears, so a first
// sweep over a large, old cache spreads its deletes and the refetches that
// follow over several intervals. A variable so tests can lower it.
var retentionMaxClears = 10000

func (s *Server) startRetentionLoop(ctx context.Context) {
	reg := retention.Default
	rules, err := s.cfg.RetentionRules(reg)
	if err != nil {
		// Validate already rejected an invalid config at startup.
		s.logger.Error("retention: invalid configuration, sweep disabled", "error", err)
		return
	}
	if rules.MinPositive() == 0 {
		return
	}

	covered := reg.Keys()
	if len(covered) == 0 {
		// Only a default can be set without any registered ecosystem;
		// validation rejects every other rule.
		s.logger.Info("retention: no ecosystem supports retention in this version, sweep disabled",
			"default_not_applied", notCovered(covered))
		return
	}
	s.logger.Info("retention enabled",
		"covered", covered,
		"default_not_applied", notCovered(covered),
		"sweep_interval", s.cfg.ParseRetentionSweepInterval())

	ticker := time.NewTicker(s.cfg.ParseRetentionSweepInterval())
	defer ticker.Stop()

	sweepRetention(ctx, s.db, s.logger, reg, rules, time.Now())

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweepRetention(ctx, s.db, s.logger, reg, rules, time.Now())
		}
	}
}

// notCovered returns the known ecosystem keys that are not in covered.
func notCovered(covered []string) []string {
	var missing []string
	for _, key := range retention.KnownKeys {
		if !slices.Contains(covered, key) {
			missing = append(missing, key)
		}
	}
	return missing
}

// sweepRetention clears cached artifacts that have outlived their retention
// as of now and queues their objects for deletion. Objects are not deleted
// directly: a hit still buffered when the scan ran can make an artifact that
// is being served look expired, and the queue's grace period covers that.
func sweepRetention(ctx context.Context, db *database.DB, logger *slog.Logger,
	reg *retention.Registry, rules retention.Rules, now time.Time) {
	minimum := rules.MinPositive()
	if minimum == 0 {
		return
	}

	if err := db.FlushHits(); err != nil {
		logger.Warn("retention: failed to write buffered hits before the sweep", "error", err)
	}

	maxID, err := db.MaxArtifactID()
	if err != nil {
		logger.Warn("retention: failed to read artifact ids", "error", err)
		return
	}

	// Rows that are not expired under the shortest rule cannot be expired
	// under any, so the query uses that cutoff and each row is then checked
	// against its own rule.
	scanCutoff := now.Add(-minimum)
	cleared := 0
	freed := int64(0)
	capped := false

	for from := int64(0); from < maxID && !capped; from += retentionWindow {
		if ctx.Err() != nil {
			return
		}
		candidates, err := db.GetRetentionCandidates(from, from+retentionWindow, scanCutoff)
		if err != nil {
			logger.Warn("retention: failed to scan artifacts", "from_id", from, "error", err)
			return
		}
		for _, c := range candidates {
			if ctx.Err() != nil {
				return
			}
			if cleared >= retentionMaxClears {
				capped = true
				break
			}
			if !clearIfExpired(db, logger, reg, rules, now, c) {
				continue
			}
			metrics.RecordArtifactEvicted("retention", c.Ecosystem)
			cleared++
			if c.Size.Valid {
				freed += c.Size.Int64
			}
		}
	}

	if cleared > 0 {
		logger.Info("retention: sweep completed",
			"cleared", cleared, "freed_bytes", freed, "capped", capped)
	}
}

// clearIfExpired clears one candidate when its own rule says it has expired
// and queues its object for deletion, reporting whether it did.
func clearIfExpired(db *database.DB, logger *slog.Logger, reg *retention.Registry,
	rules retention.Rules, now time.Time, c database.RetentionCandidate) bool {
	key, ok := reg.KeyForDBEcosystem(c.Ecosystem)
	if !ok {
		return false
	}
	d := rules.For(key, c.PackagePURL)
	if d <= 0 {
		return false
	}
	cutoff := now.Add(-d)
	if !c.ExpiredBefore(cutoff) {
		return false
	}

	recordCleared, err := db.ClearExpiredArtifact(c.VersionPURL, c.Filename, c.StoragePath, cutoff)
	if err != nil {
		logger.Warn("retention: failed to clear artifact record",
			"version_purl", c.VersionPURL, "filename", c.Filename, "error", err)
		return false
	}
	if !recordCleared {
		return false
	}
	if err := db.QueuePendingDelete(c.StoragePath); err != nil {
		logger.Warn("retention: failed to queue object for deletion", "path", c.StoragePath, "error", err)
	}
	return true
}

// recordLRUEviction counts an artifact the size limit evicted under the
// ecosystem of its package record.
func recordLRUEviction(db *database.DB, logger *slog.Logger, versionPURL string) {
	ecosystem, err := db.GetArtifactEcosystem(versionPURL)
	if err != nil {
		logger.Debug("eviction: failed to look up ecosystem for metrics", "version_purl", versionPURL, "error", err)
	}
	metrics.RecordArtifactEvicted("lru", ecosystem)
}
