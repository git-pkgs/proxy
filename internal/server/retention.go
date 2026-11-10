package server

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/metrics"
	"github.com/git-pkgs/proxy/internal/retention"
	"github.com/git-pkgs/purl"
)

// retentionWindow is how many artifact ids one query scans.
const retentionWindow = 5000

// retentionMaxClears caps how many artifacts one sweep clears, so a first
// sweep over a large, old cache spreads its deletes and the refetches that
// follow over several intervals. A variable so tests can lower it.
var retentionMaxClears = 10000

func (s *Server) startRetentionLoop(ctx context.Context, resolvers map[string]sourceResolver) {
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
	attrs := []any{
		"covered", covered,
		"default_not_applied", notCovered(covered),
		"sweep_interval", s.cfg.ParseRetentionSweepInterval(),
	}
	if rules.KeepLatest > 0 {
		attrs = append(attrs, "keep_latest", rules.KeepLatest, "keep_latest_covered", reg.KeepLatestKeys())
	}
	s.logger.Info("retention enabled", attrs...)

	ticker := time.NewTicker(s.cfg.ParseRetentionSweepInterval())
	defer ticker.Stop()

	sweep := func() {
		resolveRetentionSources(ctx, s.logger, resolvers, rules)
		s.sweepRetention(ctx, reg, rules)
	}
	sweep()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

func (s *Server) sweepRetention(ctx context.Context, reg *retention.Registry, rules retention.Rules) {
	s.runExclusive(ctx, "retention", func() {
		sweepRetention(ctx, s.db, s.logger, reg, rules, time.Now())
	})
}

// notCovered returns the known ecosystem keys the global default cannot reach.
func notCovered(covered []string) []string {
	var missing []string
	for _, key := range retention.KnownKeys {
		if key == "generic" || !slices.Contains(covered, key) {
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

	// Without the buffered hits written, an artifact being served right now
	// can look expired, so skip this sweep rather than clear it.
	if err := db.FlushHits(); err != nil {
		logger.Warn("retention: failed to write buffered hits, skipping this sweep", "error", err)
		return
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
	keep := newLatestKeeper(db, logger, reg, rules.KeepLatest)
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
			if !clearIfExpired(db, logger, reg, rules, keep, now, c) {
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
// and keep does not hold it back, and queues its object for deletion,
// reporting whether it did.
func clearIfExpired(db *database.DB, logger *slog.Logger, reg *retention.Registry,
	rules retention.Rules, keep *latestKeeper, now time.Time, c database.RetentionCandidate) bool {
	key, ok := reg.KeyForDBEcosystem(c.Ecosystem)
	if !ok || rules.Pending(key) {
		return false
	}
	d := rules.For(key, c.PackagePURL)
	if d <= 0 {
		return false
	}
	cutoff := now.Add(-d)
	if !c.ExpiredBefore(cutoff) || keep.keeps(key, c) {
		return false
	}

	cleared, err := db.ClearExpiredArtifact(c.VersionPURL, c.Filename, c.StoragePath, cutoff)
	if err != nil {
		logger.Warn("retention: failed to clear artifact record",
			"version_purl", c.VersionPURL, "filename", c.Filename, "error", err)
		return false
	}
	return cleared
}

// latestKeeper answers, for one sweep, whether an artifact belongs to one of
// the highest cached versions of its package, which keep_latest holds back
// from eviction by age.
type latestKeeper struct {
	db     *database.DB
	logger *slog.Logger
	reg    *retention.Registry
	n      int
	// kept holds the version PURLs to keep by package PURL, for the
	// packages looked at so far.
	kept map[string]map[string]struct{}
}

func newLatestKeeper(db *database.DB, logger *slog.Logger, reg *retention.Registry, n int) *latestKeeper {
	return &latestKeeper{db: db, logger: logger, reg: reg, n: n, kept: map[string]map[string]struct{}{}}
}

// keeps reports whether c, an artifact of the ecosystem key, is a file of
// one of the highest cached versions of its package.
func (k *latestKeeper) keeps(key string, c database.RetentionCandidate) bool {
	if k.n <= 0 {
		return false
	}
	spec, _ := k.reg.Lookup(key)
	if spec.CompareVersions == nil {
		return false
	}
	kept, ok := k.kept[c.PackagePURL]
	if !ok {
		var err error
		if kept, err = k.newest(spec, c.PackagePURL); err != nil {
			// Not knowing which versions are the highest, keep the artifact
			// for this sweep rather than evict one of them.
			k.logger.Warn("retention: failed to list cached versions, keeping artifact",
				"package_purl", c.PackagePURL, "error", err)
			return true
		}
		k.kept[c.PackagePURL] = kept
	}
	_, keep := kept[c.VersionPURL]
	return keep
}

// newest returns the version PURLs of the highest cached versions of a
// package.
func (k *latestKeeper) newest(spec retention.Spec, packagePURL string) (map[string]struct{}, error) {
	purls, err := k.db.CachedVersionPURLs(packagePURL)
	if err != nil {
		return nil, err
	}
	byVersion := make(map[string][]string, len(purls))
	versions := make([]string, 0, len(purls))
	for _, versionPURL := range purls {
		p, err := purl.Parse(versionPURL)
		if err != nil || p.Version == "" {
			continue
		}
		if _, seen := byVersion[p.Version]; !seen {
			versions = append(versions, p.Version)
		}
		byVersion[p.Version] = append(byVersion[p.Version], versionPURL)
	}
	kept := map[string]struct{}{}
	for _, v := range retention.NewestVersions(spec.CompareVersions, versions, k.n) {
		for _, versionPURL := range byVersion[v] {
			kept[versionPURL] = struct{}{}
		}
	}
	return kept, nil
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
