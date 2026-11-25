package server

import (
	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/metrics"
	"github.com/git-pkgs/proxy/internal/mirror"
	"github.com/git-pkgs/purl"
)

// EcosystemCache is what the cache holds for an ecosystem and how often it
// served its artifacts.
type EcosystemCache struct {
	Packages, Artifacts, Size, Downloads string
	// HitRate is the percentage of artifact requests served from the cache
	// since the proxy started, "" with none yet.
	HitRate string
}

// EcosystemPolicy is the configuration that changes what an ecosystem's
// clients get.
type EcosystemPolicy struct {
	// Cooldown is the ecosystem's cooldown and CooldownRule where it comes
	// from, both "" with cooldown off. CooldownUnsupported marks an
	// ecosystem cooldown can't apply to, for lack of publish times.
	Cooldown, CooldownRule string
	CooldownUnsupported    bool
	// Refresh is how often mirror.refresh mirrors new releases, "" when it
	// doesn't cover the ecosystem.
	Refresh string
	// Schedules counts the mirror.schedules entries, for images.
	Schedules int
}

// ecosystemCaches fills in each ecosystem's cache figures from the database
// stats and the counters since start. Ecosystems with neither get none.
func ecosystemCaches(ecosystems []UpstreamEcosystem, stats []database.EcosystemStats, snap *metrics.Snapshot) {
	byKey := make(map[string]database.EcosystemStats, len(stats))
	for _, s := range stats {
		byKey[purl.NormalizeEcosystem(s.Ecosystem)] = s
	}
	hits, misses := artifactCacheCounts(snap, "proxy_cache_hits_total"), artifactCacheCounts(snap, "proxy_cache_misses_total")
	for i := range ecosystems {
		key := purl.NormalizeEcosystem(ecosystems[i].Key)
		s, stored := byKey[key]
		served := hits[key] + misses[key]
		if !stored && served == 0 {
			continue
		}
		c := &EcosystemCache{
			Packages:  formatCount(s.Packages),
			Artifacts: formatCount(s.Artifacts),
			Size:      formatSize(s.CacheSize),
			Downloads: formatCount(s.Downloads),
		}
		if served > 0 {
			c.HitRate = percentOf(int64(hits[key]), int64(served))
		}
		ecosystems[i].Cache = c
	}
}

// artifactCacheCounts totals a cache counter's artifact series per
// ecosystem.
func artifactCacheCounts(snap *metrics.Snapshot, name string) map[string]float64 {
	out := map[string]float64{}
	for _, s := range snap.Samples(name) {
		if s.Label("kind") == metrics.CacheKindArtifact {
			out[purl.NormalizeEcosystem(s.Label("ecosystem"))] += s.Value
		}
	}
	return out
}

// ecosystemPolicies fills in each ecosystem's cooldown and mirroring.
func ecosystemPolicies(ecosystems []UpstreamEcosystem, cooldown *cooldownView, mirrorCfg *config.MirrorConfig) {
	for i := range ecosystems {
		key := ecosystems[i].Key
		var p EcosystemPolicy
		if cooldown.enabled() {
			if handler.CooldownSupported(key) {
				p.Cooldown = formatCooldown(cooldown.policy.For(key, ""))
				p.CooldownRule = "default"
				if _, ok := cooldown.cfg.Ecosystems[key]; ok {
					p.CooldownRule = "ecosystem override"
				}
			} else {
				p.CooldownUnsupported = true
			}
		}
		if mirrorCfg != nil {
			if mirrorCfg.Refresh != nil && mirror.Refreshes(key) {
				p.Refresh = "every " + formatCooldown(mirrorCfg.Refresh.IntervalDuration())
			}
			if key == "oci" {
				p.Schedules = len(mirrorCfg.Schedules)
			}
		}
		ecosystems[i].Policy = p
	}
}
