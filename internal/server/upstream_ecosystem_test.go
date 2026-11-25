package server

import (
	"testing"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/metrics"
	"github.com/prometheus/client_golang/prometheus"
)

func cacheSnapshot(t *testing.T, hits, misses map[[2]string]float64) *metrics.Snapshot {
	t.Helper()
	reg := prometheus.NewRegistry()
	h := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "proxy_cache_hits_total"}, []string{"ecosystem", "kind"})
	m := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "proxy_cache_misses_total"}, []string{"ecosystem", "kind"})
	reg.MustRegister(h, m)
	for k, v := range hits {
		h.WithLabelValues(k[0], k[1]).Add(v)
	}
	for k, v := range misses {
		m.WithLabelValues(k[0], k[1]).Add(v)
	}
	snap, err := metrics.GatherFrom(reg)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestEcosystemCaches(t *testing.T) {
	stats := []database.EcosystemStats{
		{Ecosystem: "npm", Packages: 1200, Artifacts: 3400, CacheSize: 5 << 30, Downloads: 9000},
		{Ecosystem: "golang", Packages: 3},
	}
	snap := cacheSnapshot(t,
		map[[2]string]float64{{"npm", metrics.CacheKindArtifact}: 90, {"npm", metrics.CacheKindMetadata}: 1000},
		map[[2]string]float64{{"npm", metrics.CacheKindArtifact}: 10, {"pypi", metrics.CacheKindArtifact}: 4})

	ecosystems := []UpstreamEcosystem{{Key: "npm"}, {Key: "golang"}, {Key: "pypi"}, {Key: "git"}}
	ecosystemCaches(ecosystems, stats, snap)

	npm := ecosystems[0].Cache
	if npm == nil || npm.Packages != "1,200" || npm.Artifacts != "3,400" || npm.Size != "5.0 GB" || npm.Downloads != "9,000" {
		t.Fatalf("npm cache = %+v", npm)
	}
	// Artifact hits only: a metadata hit costs and saves something else.
	if npm.HitRate != "90.0" {
		t.Errorf("npm hit rate = %q, want 90.0", npm.HitRate)
	}
	// The go key in the database is golang; nothing served since start.
	if g := ecosystems[1].Cache; g == nil || g.Packages != "3" || g.HitRate != "" {
		t.Errorf("golang cache = %+v", g)
	}
	// Misses since start but nothing in the database yet.
	if p := ecosystems[2].Cache; p == nil || p.HitRate != "0.0" || p.Packages != "0" {
		t.Errorf("pypi cache = %+v", p)
	}
	if ecosystems[3].Cache != nil {
		t.Errorf("git cache = %+v, want none", ecosystems[3].Cache)
	}
}

func TestEcosystemPolicies(t *testing.T) {
	cfg := config.CooldownConfig{Default: "3d", Ecosystems: map[string]string{"npm": "7d", "oci": "1d"}}
	policy, err := cfg.Policy()
	if err != nil {
		t.Fatal(err)
	}
	view, err := newCooldownView(cfg, policy)
	if err != nil {
		t.Fatal(err)
	}
	mirrorCfg := config.MirrorConfig{
		Refresh:   &config.MirrorRefresh{Interval: "6h"},
		Schedules: []config.MirrorSchedule{{Interval: "1h", Images: []string{"pkg:docker/library/alpine@3"}}, {Interval: "1h", Images: []string{"pkg:docker/library/debian@12"}}},
	}
	if err := mirrorCfg.Validate(); err != nil {
		t.Fatal(err)
	}

	ecosystems := []UpstreamEcosystem{{Key: "npm"}, {Key: "pypi"}, {Key: "oci"}, {Key: "deb"}, {Key: "git"}}
	ecosystemPolicies(ecosystems, view, &mirrorCfg)

	for i, want := range []EcosystemPolicy{
		{Cooldown: "7d", CooldownRule: "ecosystem override", Refresh: "every 6h"},
		{Cooldown: "3d", CooldownRule: "default", Refresh: "every 6h"},
		// No publish times, so the override does nothing.
		{CooldownUnsupported: true, Schedules: 2},
		{CooldownUnsupported: true, Refresh: "every 6h"},
		{CooldownUnsupported: true},
	} {
		if got := ecosystems[i].Policy; got != want {
			t.Errorf("%s policy = %+v, want %+v", ecosystems[i].Key, got, want)
		}
	}

	ecosystems = []UpstreamEcosystem{{Key: "npm"}}
	ecosystemPolicies(ecosystems, nil, &config.MirrorConfig{})
	if got := ecosystems[0].Policy; got != (EcosystemPolicy{}) {
		t.Errorf("with cooldown and mirroring off: %+v", got)
	}
}
