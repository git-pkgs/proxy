package server

import (
	"strings"
	"testing"

	"github.com/git-pkgs/proxy/internal/metrics"
)

// metricSurface records where each registered metric is shown on /ui/analytics.
// Adding a metric to the metrics package without adding it here — and without
// actually surfacing it — fails TestEveryMetricIsSurfaced.
var metricSurface = map[string]string{
	// Database-derived, shown in the donut, the KPI row and the breakdown table.
	"proxy_downloaded_bytes":           "donut + breakdown table",
	"proxy_artifact_downloads":         "Downloads tile + breakdown table",
	"proxy_ecosystem_cache_size_bytes": "breakdown table",
	"proxy_ecosystem_cached_artifacts": "breakdown table",
	"proxy_ecosystem_packages":         "breakdown table",
	"proxy_ecosystem_versions":         "breakdown table",
	"proxy_cache_size_bytes":           "Cache size tile",
	"proxy_cached_artifacts_total":     "Cached artifacts tile",

	// Request-path counters, shown in the Runtime card's source section.
	"proxy_response_bytes_total":        "Runtime: Served",
	"proxy_client_requests_total":       "Runtime: Request sources — By client",
	"proxy_client_response_bytes_total": "Runtime: Request sources — By client",

	// Registry-derived, shown in the Runtime card.
	"proxy_requests_total":                     "Runtime: Requests + Responses by status",
	"proxy_request_duration_seconds":           "Runtime: Mean latency",
	"proxy_active_requests":                    "Runtime: In flight",
	"proxy_cache_hits_total":                   "Runtime: Cache lookups + hit rate",
	"proxy_cache_misses_total":                 "Runtime: Cache lookups + hit rate",
	"proxy_upstream_fetch_duration_seconds":    "Runtime: Upstream fetches + mean fetch",
	"proxy_upstream_errors_total":              "Runtime: Upstream errors",
	"proxy_storage_operation_duration_seconds": "Runtime: Storage operations",
	"proxy_storage_errors_total":               "Runtime: Storage errors",
	"proxy_integrity_failures_total":           "Runtime: Integrity failures",
	"proxy_health_probe_failures_total":        "Runtime: Health probe failures",
	"proxy_circuit_breaker_state":              "Runtime: Circuit breakers",
	"proxy_circuit_breaker_trips_total":        "Runtime: Circuit breaker trips",
	"proxy_scan_duration_seconds":              "Runtime: Pre-cache scanning — Scans",
	"proxy_scan_blocked_total":                 "Runtime: Pre-cache scanning — Artifacts blocked",
	"proxy_scan_errors_total":                  "Runtime: Pre-cache scanning — Scan errors",
}

// TestEveryMetricIsSurfaced fails when a metric is registered but has no home
// on the analytics page. The page is meant to be a complete view of what
// /metrics exposes, so a silently unsurfaced metric is a gap, not a detail.
func TestEveryMetricIsSurfaced(t *testing.T) {
	// Touch every metric family so it is present in the registry output, since
	// a vector with no observed label values gathers as nothing at all.
	metrics.RecordRequest("npm", 200, 0)
	metrics.RecordResponse("npm", "npm", 1)
	metrics.RecordCacheHit("npm")
	metrics.RecordCacheMiss("npm")
	metrics.RecordUpstreamFetch("npm", 0)
	metrics.RecordUpstreamError("npm", "fetch_failed")
	metrics.RecordStorageOperation("get", 0)
	metrics.RecordStorageError("get")
	metrics.RecordIntegrityFailure("npm")
	metrics.RecordHealthProbeFailure("write")
	metrics.UpdateCircuitBreakerState("example.test", 0)
	metrics.RecordCircuitBreakerTrip("example.test")
	metrics.RecordScanResult("npm", "clamav", false, 0)
	metrics.RecordScanError("npm", "clamav", "timeout")
	metrics.UpdateCacheStats(1, 1)
	metrics.UpdateEcosystemStats([]metrics.EcosystemStats{{Ecosystem: "npm", DownloadedBytes: 1}})

	snap, err := metrics.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	var registered []string
	for _, name := range snap.Names() {
		if strings.HasPrefix(name, "proxy_") {
			registered = append(registered, name)
		}
	}
	if len(registered) == 0 {
		t.Fatal("no proxy_ metrics in the registry; the probe above is not working")
	}

	for _, name := range registered {
		if _, ok := metricSurface[name]; !ok {
			t.Errorf("%s is registered but has no home on /ui/analytics; "+
				"surface it and add it to metricSurface", name)
		}
	}

	for name := range metricSurface {
		found := false
		for _, r := range registered {
			if r == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("metricSurface lists %s, but it is not registered any more; drop the entry", name)
		}
	}
}
