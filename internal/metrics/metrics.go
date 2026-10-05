// Package metrics provides Prometheus metrics collection for the proxy.
package metrics

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/git-pkgs/purl"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// Request metrics
	RequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "proxy_requests_total",
			Help: "Total number of requests by ecosystem and status",
		},
		[]string{"ecosystem", "status"},
	)

	RequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "proxy_request_duration_seconds",
			Help:    "Request duration in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"ecosystem", "status"},
	)

	// Cache metrics
	CacheHits = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "proxy_cache_hits_total",
			Help: "Total number of cache hits by ecosystem",
		},
		[]string{"ecosystem"},
	)

	CacheMisses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "proxy_cache_misses_total",
			Help: "Total number of cache misses by ecosystem",
		},
		[]string{"ecosystem"},
	)

	CacheSize = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "proxy_cache_size_bytes",
			Help: "Total size of cached artifacts in bytes",
		},
	)

	CachedArtifacts = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "proxy_cached_artifacts_total",
			Help: "Total number of cached artifacts",
		},
	)

	// Upstream metrics
	UpstreamFetchDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "proxy_upstream_fetch_duration_seconds",
			Help:    "Upstream fetch duration in seconds",
			Buckets: []float64{.1, .25, .5, 1, 2.5, 5, 10, 30},
		},
		[]string{"ecosystem"},
	)

	UpstreamErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "proxy_upstream_errors_total",
			Help: "Total number of upstream fetch errors by type",
		},
		[]string{"ecosystem", "error_type"},
	)

	// Circuit breaker metrics
	CircuitBreakerState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "proxy_circuit_breaker_state",
			Help: "Circuit breaker state (0=closed, 1=half-open, 2=open)",
		},
		[]string{"registry"},
	)

	CircuitBreakerTrips = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "proxy_circuit_breaker_trips_total",
			Help: "Total number of circuit breaker trips",
		},
		[]string{"registry"},
	)

	// Storage metrics
	StorageOperationDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "proxy_storage_operation_duration_seconds",
			Help:    "Storage operation duration in seconds",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1},
		},
		[]string{"operation"},
	)

	StorageErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "proxy_storage_errors_total",
			Help: "Total number of storage errors by operation",
		},
		[]string{"operation"},
	)

	// Active requests
	ActiveRequests = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "proxy_active_requests",
			Help: "Number of currently active requests",
		},
	)

	IntegrityFailures = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "proxy_integrity_failures_total",
			Help: "Cached artifacts that failed hash verification on read",
		},
		[]string{"ecosystem"},
	)

	MissingObjects = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "proxy_cache_missing_objects_total",
			Help: "Cache records whose stored object was gone, so the artifact was refetched",
		},
		[]string{"ecosystem"},
	)

	HealthProbeFailures = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "proxy_health_probe_failures_total",
			Help: "Total number of storage health probe failures, by step (write|size|read|verify|delete).",
		},
		[]string{"step"},
	)

	// Per-ecosystem gauges, derived from the database rather than incremented
	// in the request path, and refreshed on the same tick as the cache gauges
	// above.
	EcosystemDownloadedBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "proxy_ecosystem_downloaded_bytes",
			Help: "Accumulated bytes served from cache per ecosystem (cache hits x artifact size)",
		},
		[]string{"ecosystem"},
	)

	EcosystemDownloads = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "proxy_ecosystem_artifact_downloads",
			Help: "Accumulated artifact downloads served from cache per ecosystem",
		},
		[]string{"ecosystem"},
	)

	EcosystemCacheSize = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "proxy_ecosystem_cache_size_bytes",
			Help: "Size of cached artifacts per ecosystem in bytes",
		},
		[]string{"ecosystem"},
	)

	EcosystemCachedArtifacts = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "proxy_ecosystem_cached_artifacts",
			Help: "Number of cached artifacts per ecosystem",
		},
		[]string{"ecosystem"},
	)

	EcosystemPackages = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "proxy_ecosystem_packages",
			Help: "Number of known packages per ecosystem",
		},
		[]string{"ecosystem"},
	)

	EcosystemVersions = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "proxy_ecosystem_versions",
			Help: "Number of known package versions per ecosystem",
		},
		[]string{"ecosystem"},
	)

	ResponseBytes = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "proxy_response_bytes_total",
			Help: "Total response body bytes written to clients, by ecosystem",
		},
		[]string{"ecosystem"},
	)

	ClientRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "proxy_client_requests_total",
			Help: "Total requests by client tool, as identified from the User-Agent",
		},
		[]string{"client"},
	)

	ClientResponseBytes = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "proxy_client_response_bytes_total",
			Help: "Total response body bytes written to clients, by client tool",
		},
		[]string{"client"},
	)

	// Scanning metrics
	ScanDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "proxy_scan_duration_seconds",
			Help:    "Pre-cache artifact scan duration in seconds, by ecosystem and scanner",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"ecosystem", "scanner"},
	)

	ScanBlocked = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "proxy_scan_blocked_total",
			Help: "Total number of artifacts blocked by a pre-cache scan, by ecosystem and scanner",
		},
		[]string{"ecosystem", "scanner"},
	)

	ScanErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "proxy_scan_errors_total",
			Help: "Total number of pre-cache scan errors, by ecosystem, scanner, and error type",
		},
		[]string{"ecosystem", "scanner", "error_type"},
	)
)

func init() {
	// Register all metrics with Prometheus
	prometheus.MustRegister(
		RequestsTotal,
		RequestDuration,
		CacheHits,
		CacheMisses,
		CacheSize,
		CachedArtifacts,
		UpstreamFetchDuration,
		UpstreamErrors,
		CircuitBreakerState,
		CircuitBreakerTrips,
		StorageOperationDuration,
		StorageErrors,
		ActiveRequests,
		IntegrityFailures,
		MissingObjects,
		HealthProbeFailures,
		EcosystemDownloadedBytes,
		EcosystemDownloads,
		EcosystemCacheSize,
		EcosystemCachedArtifacts,
		EcosystemPackages,
		EcosystemVersions,
		ResponseBytes,
		ClientRequests,
		ClientResponseBytes,
		ScanDuration,
		ScanBlocked,
		ScanErrors,
	)
}

// Handler returns an HTTP handler for the Prometheus /metrics endpoint.
func Handler() http.Handler {
	return promhttp.Handler()
}

// RecordRequest tracks request metrics with timing.
func RecordRequest(ecosystem string, status int, duration time.Duration) {
	statusStr := strconv.Itoa(status)
	RequestsTotal.WithLabelValues(ecosystem, statusStr).Inc()
	RequestDuration.WithLabelValues(ecosystem, statusStr).Observe(duration.Seconds())
}

// RecordResponse tracks what a client actually downloaded.
//
// Distinct from proxy_ecosystem_downloaded_bytes: that gauge is derived from
// the database and counts cache hits multiplied by artifact size, while this
// counts body bytes as they are written, including metadata responses and
// cache misses.
//
// client must come from a closed set -- a User-Agent is attacker-controlled, so
// passing it through raw would mint a time series per request.
func RecordResponse(ecosystem, client string, bytes int64) {
	ClientRequests.WithLabelValues(client).Inc()
	if bytes <= 0 {
		return
	}
	ResponseBytes.WithLabelValues(ecosystem).Add(float64(bytes))
	ClientResponseBytes.WithLabelValues(client).Add(float64(bytes))
}

// RecordCacheHit increments cache hit counter.
func RecordCacheHit(ecosystem string) {
	CacheHits.WithLabelValues(purl.NormalizeEcosystem(ecosystem)).Inc()
}

// RecordCacheMiss increments cache miss counter.
func RecordCacheMiss(ecosystem string) {
	CacheMisses.WithLabelValues(purl.NormalizeEcosystem(ecosystem)).Inc()
}

// RecordUpstreamFetch tracks upstream fetch duration.
func RecordUpstreamFetch(ecosystem string, duration time.Duration) {
	UpstreamFetchDuration.WithLabelValues(ecosystem).Observe(duration.Seconds())
}

// RecordUpstreamError increments upstream error counter.
func RecordUpstreamError(ecosystem, errorType string) {
	UpstreamErrors.WithLabelValues(ecosystem, errorType).Inc()
}

// RecordStorageOperation tracks storage operation duration.
func RecordStorageOperation(operation string, duration time.Duration) {
	StorageOperationDuration.WithLabelValues(operation).Observe(duration.Seconds())
}

// RecordIntegrityFailure increments the integrity failure counter.
func RecordIntegrityFailure(ecosystem string) {
	IntegrityFailures.WithLabelValues(ecosystem).Inc()
}

// RecordMissingObject counts a cache record found without its stored object.
func RecordMissingObject(ecosystem string) {
	MissingObjects.WithLabelValues(ecosystem).Inc()
}

// RecordHealthProbeFailure increments the health probe failure counter.
// step is one of: "write", "size", "read", "verify", "delete".
func RecordHealthProbeFailure(step string) {
	HealthProbeFailures.WithLabelValues(step).Inc()
}

// RecordStorageError increments storage error counter.
func RecordStorageError(operation string) {
	StorageErrors.WithLabelValues(operation).Inc()
}

// RecordScanResult tracks a completed pre-cache scan call.
func RecordScanResult(ecosystem, scannerName string, allowed bool, duration time.Duration) {
	ecosystem = purl.NormalizeEcosystem(ecosystem)
	ScanDuration.WithLabelValues(ecosystem, scannerName).Observe(duration.Seconds())
	if !allowed {
		ScanBlocked.WithLabelValues(ecosystem, scannerName).Inc()
	}
}

// RecordScanError increments the scan error counter.
// errorType is one of: "error" (scanner call failed), "timeout", "cancelled".
func RecordScanError(ecosystem, scannerName, errorType string) {
	ScanErrors.WithLabelValues(purl.NormalizeEcosystem(ecosystem), scannerName, errorType).Inc()
}

// UpdateCacheStats updates cache size and artifact count gauges.
func UpdateCacheStats(sizeBytes, artifactCount int64) {
	CacheSize.Set(float64(sizeBytes))
	CachedArtifacts.Set(float64(artifactCount))
}

// EcosystemStats is one ecosystem's row of the snapshot published as gauges.
type EcosystemStats struct {
	Ecosystem       string
	Packages        int64
	Versions        int64
	Artifacts       int64
	CacheSize       int64
	Downloads       int64
	DownloadedBytes int64
}

// publishedEcosystems tracks which labels the per-ecosystem gauges currently
// carry, so a label that disappears can be deleted individually.
var (
	publishedMu         sync.Mutex
	publishedEcosystems = map[string]bool{}
)

// UpdateEcosystemStats republishes the per-ecosystem gauges from a fresh snapshot.
//
// Rows are summed by label before anything is published, because normalizing
// collapses aliases: a database holding both "gem" and "rubygems" rows -- the
// proxy writes the former, git-pkgs the latter -- arrives as two rows belonging
// to one label, and publishing them one at a time would leave only the last.
//
// The vectors are not Reset() first. Reset followed by a repopulating loop
// leaves a window in which a scrape sees the families empty or half filled,
// which renders as a spurious gap on any panel built from them. Each series is
// Set instead, and only labels that have actually disappeared are deleted.
func UpdateEcosystemStats(stats []EcosystemStats) {
	totals := make(map[string]EcosystemStats, len(stats))
	for _, s := range stats {
		ecosystem := purl.NormalizeEcosystem(s.Ecosystem)
		t := totals[ecosystem]
		t.Packages += s.Packages
		t.Versions += s.Versions
		t.Artifacts += s.Artifacts
		t.CacheSize += s.CacheSize
		t.Downloads += s.Downloads
		t.DownloadedBytes += s.DownloadedBytes
		totals[ecosystem] = t
	}

	for ecosystem, t := range totals {
		EcosystemDownloadedBytes.WithLabelValues(ecosystem).Set(float64(t.DownloadedBytes))
		EcosystemDownloads.WithLabelValues(ecosystem).Set(float64(t.Downloads))
		EcosystemCacheSize.WithLabelValues(ecosystem).Set(float64(t.CacheSize))
		EcosystemCachedArtifacts.WithLabelValues(ecosystem).Set(float64(t.Artifacts))
		EcosystemPackages.WithLabelValues(ecosystem).Set(float64(t.Packages))
		EcosystemVersions.WithLabelValues(ecosystem).Set(float64(t.Versions))
	}

	publishedMu.Lock()
	defer publishedMu.Unlock()

	for ecosystem := range publishedEcosystems {
		if _, still := totals[ecosystem]; still {
			continue
		}
		EcosystemDownloadedBytes.DeleteLabelValues(ecosystem)
		EcosystemDownloads.DeleteLabelValues(ecosystem)
		EcosystemCacheSize.DeleteLabelValues(ecosystem)
		EcosystemCachedArtifacts.DeleteLabelValues(ecosystem)
		EcosystemPackages.DeleteLabelValues(ecosystem)
		EcosystemVersions.DeleteLabelValues(ecosystem)
	}

	publishedEcosystems = make(map[string]bool, len(totals))
	for ecosystem := range totals {
		publishedEcosystems[ecosystem] = true
	}
}

// UpdateCircuitBreakerState updates circuit breaker state gauge.
// state: 0=closed, 1=half-open, 2=open
func UpdateCircuitBreakerState(registry string, state int) {
	CircuitBreakerState.WithLabelValues(registry).Set(float64(state))
}

// RecordCircuitBreakerTrip increments circuit breaker trip counter.
func RecordCircuitBreakerTrip(registry string) {
	CircuitBreakerTrips.WithLabelValues(registry).Inc()
}

// IncrementActiveRequests increments the active request counter.
func IncrementActiveRequests() {
	ActiveRequests.Inc()
}

// DecrementActiveRequests decrements the active request counter.
func DecrementActiveRequests() {
	ActiveRequests.Dec()
}
