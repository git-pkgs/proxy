package metrics

import (
	"testing"
	"time"

	"github.com/git-pkgs/purl"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestUpdateEcosystemStats(t *testing.T) {
	UpdateEcosystemStats([]EcosystemStats{
		{Ecosystem: "npm", Packages: 10, Versions: 25, Artifacts: 40, CacheSize: 1500, Downloads: 4, DownloadedBytes: 3500},
		{Ecosystem: "cargo", Packages: 2, Versions: 3, Artifacts: 3, CacheSize: 200, Downloads: 10, DownloadedBytes: 2000},
	})

	if got := testutil.ToFloat64(EcosystemDownloadedBytes.WithLabelValues("npm")); got != 3500 {
		t.Errorf("npm downloaded bytes = %v, want 3500", got)
	}
	if got := testutil.ToFloat64(EcosystemDownloads.WithLabelValues("npm")); got != 4 {
		t.Errorf("npm downloads = %v, want 4", got)
	}
	if got := testutil.ToFloat64(EcosystemCacheSize.WithLabelValues("cargo")); got != 200 {
		t.Errorf("cargo cache size = %v, want 200", got)
	}
	if got := testutil.ToFloat64(EcosystemCachedArtifacts.WithLabelValues("npm")); got != 40 {
		t.Errorf("npm cached artifacts = %v, want 40", got)
	}
	if got := testutil.ToFloat64(EcosystemPackages.WithLabelValues("npm")); got != 10 {
		t.Errorf("npm packages = %v, want 10", got)
	}
	if got := testutil.ToFloat64(EcosystemVersions.WithLabelValues("npm")); got != 25 {
		t.Errorf("npm versions = %v, want 25", got)
	}
}

// A refresh that no longer mentions an ecosystem must drop its series rather
// than leave it frozen at the last observed value.
func TestUpdateEcosystemStatsDropsStaleSeries(t *testing.T) {
	UpdateEcosystemStats([]EcosystemStats{
		{Ecosystem: "npm", DownloadedBytes: 3500},
		{Ecosystem: "gem", DownloadedBytes: 900},
	})
	if got := testutil.CollectAndCount(EcosystemDownloadedBytes); got != 2 {
		t.Fatalf("expected 2 series after first refresh, got %d", got)
	}

	UpdateEcosystemStats([]EcosystemStats{{Ecosystem: "npm", DownloadedBytes: 4000}})

	if got := testutil.CollectAndCount(EcosystemDownloadedBytes); got != 1 {
		t.Errorf("expected 1 series after gem disappeared, got %d", got)
	}
	if got := testutil.ToFloat64(EcosystemDownloadedBytes.WithLabelValues("npm")); got != 4000 {
		t.Errorf("npm downloaded bytes = %v, want 4000", got)
	}
}

// Ecosystem labels are normalized so these gauges join against the other
// metrics that take their ecosystem from a package record.
func TestUpdateEcosystemStatsNormalizesLabels(t *testing.T) {
	UpdateEcosystemStats([]EcosystemStats{{Ecosystem: "NPM", DownloadedBytes: 12}})

	if got := testutil.ToFloat64(EcosystemDownloadedBytes.WithLabelValues("npm")); got != 12 {
		t.Errorf("normalized npm gauge = %v, want 12", got)
	}
}

// Every metric that takes its ecosystem from a package record must label it the
// same way, or the families cannot be joined in a query. Handlers pass their own
// name, so the aliased ecosystems are the ones that can drift.
func TestEcosystemLabelsAgreeAcrossFamilies(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(UpstreamFetchDuration, UpstreamErrors, CacheHits, EcosystemDownloadedBytes)

	for _, raw := range []string{"composer", "gem", "go"} {
		RecordUpstreamFetch(raw, time.Millisecond)
		RecordUpstreamError(raw, "fetch_failed")
		RecordCacheHit(raw)
		UpdateEcosystemStats([]EcosystemStats{{Ecosystem: raw, DownloadedBytes: 1}})

		want := purl.NormalizeEcosystem(raw)
		if want == raw {
			t.Fatalf("%q is not an aliased ecosystem; pick one that is", raw)
		}

		snap, err := GatherFrom(reg)
		if err != nil {
			t.Fatalf("GatherFrom: %v", err)
		}

		for _, name := range []string{
			"proxy_upstream_fetch_duration_seconds",
			"proxy_upstream_errors_total",
			"proxy_cache_hits_total",
			"proxy_downloaded_bytes",
		} {
			found := false
			for _, sample := range snap.Samples(name) {
				if sample.Label("ecosystem") == want {
					found = true
				}
				if sample.Label("ecosystem") == raw {
					t.Errorf("%s labelled %q; it must be normalized to %q", name, raw, want)
				}
			}
			if !found {
				t.Errorf("%s has no series labelled %q", name, want)
			}
		}
	}
}

// GetEcosystemStats groups by the raw packages.ecosystem column, so a database
// carrying both spellings of an aliased ecosystem yields two rows that
// normalize to one label. They must sum rather than overwrite each other.
func TestUpdateEcosystemStatsSumsAliasedRows(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(EcosystemDownloadedBytes, EcosystemCacheSize, EcosystemPackages)

	UpdateEcosystemStats([]EcosystemStats{
		{Ecosystem: "gem", DownloadedBytes: 100, CacheSize: 10, Packages: 1},
		{Ecosystem: "rubygems", DownloadedBytes: 200, CacheSize: 20, Packages: 2},
		{Ecosystem: "npm", DownloadedBytes: 50, CacheSize: 5, Packages: 3},
	})

	if got := testutil.ToFloat64(EcosystemDownloadedBytes.WithLabelValues("rubygems")); got != 300 {
		t.Errorf("rubygems downloaded bytes = %v, want 300", got)
	}
	if got := testutil.ToFloat64(EcosystemCacheSize.WithLabelValues("rubygems")); got != 30 {
		t.Errorf("rubygems cache size = %v, want 30", got)
	}
	if got := testutil.ToFloat64(EcosystemPackages.WithLabelValues("rubygems")); got != 3 {
		t.Errorf("rubygems packages = %v, want 3", got)
	}
	// An unaliased ecosystem is unaffected.
	if got := testutil.ToFloat64(EcosystemDownloadedBytes.WithLabelValues("npm")); got != 50 {
		t.Errorf("npm downloaded bytes = %v, want 50", got)
	}

	// Adding must not accumulate across refreshes; Reset has to clear first.
	UpdateEcosystemStats([]EcosystemStats{{Ecosystem: "npm", DownloadedBytes: 50}})
	if got := testutil.ToFloat64(EcosystemDownloadedBytes.WithLabelValues("npm")); got != 50 {
		t.Errorf("npm downloaded bytes after a second refresh = %v, want 50", got)
	}
}
