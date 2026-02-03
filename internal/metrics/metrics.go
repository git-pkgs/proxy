// Package metrics provides Prometheus metrics collection for the proxy.
package metrics

import (
	"net/http"
	"strconv"
	"time"

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

// RecordCacheHit increments cache hit counter.
func RecordCacheHit(ecosystem string) {
	CacheHits.WithLabelValues(ecosystem).Inc()
}

// RecordCacheMiss increments cache miss counter.
func RecordCacheMiss(ecosystem string) {
	CacheMisses.WithLabelValues(ecosystem).Inc()
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

// RecordStorageError increments storage error counter.
func RecordStorageError(operation string) {
	StorageErrors.WithLabelValues(operation).Inc()
}

// UpdateCacheStats updates cache size and artifact count gauges.
func UpdateCacheStats(sizeBytes, artifactCount int64) {
	CacheSize.Set(float64(sizeBytes))
	CachedArtifacts.Set(float64(artifactCount))
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
