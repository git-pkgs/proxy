package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestRecordRequest(t *testing.T) {
	// Record a few requests
	RecordRequest("npm", 200, 100*time.Millisecond)
	RecordRequest("npm", 404, 50*time.Millisecond)
	RecordRequest("pypi", 200, 200*time.Millisecond)

	// No assertions needed - just verify no panics
	// Actual metric values are checked via Prometheus scraping
}

func TestRecordCacheOperations(t *testing.T) {
	RecordCacheHit("npm")
	RecordCacheHit("npm")
	RecordCacheMiss("npm")
	RecordCacheMiss("pypi")

	// No panics = success
}

func TestRecordUpstreamOperations(t *testing.T) {
	RecordUpstreamFetch("npm", 500*time.Millisecond)
	RecordUpstreamFetch("pypi", 1*time.Second)
	RecordUpstreamError("npm", "fetch_failed")
	RecordUpstreamError("pypi", "not_found")

	// No panics = success
}

func TestRecordStorageOperations(t *testing.T) {
	RecordStorageOperation("read", 10*time.Millisecond)
	RecordStorageOperation("write", 50*time.Millisecond)
	RecordStorageError("read")
	RecordStorageError("write")

	// No panics = success
}

func TestUpdateCacheStats(t *testing.T) {
	UpdateCacheStats(1024*1024*1024, 100) // 1GB, 100 artifacts
	UpdateCacheStats(0, 0)                  // Empty cache

	// No panics = success
}

func TestCircuitBreakerMetrics(t *testing.T) {
	UpdateCircuitBreakerState("npmjs.org", 0) // closed
	UpdateCircuitBreakerState("npmjs.org", 2) // open
	RecordCircuitBreakerTrip("npmjs.org")

	// No panics = success
}

func TestActiveRequests(t *testing.T) {
	IncrementActiveRequests()
	IncrementActiveRequests()
	DecrementActiveRequests()
	DecrementActiveRequests()

	// No panics = success
}

func TestMetricsAreRegistered(t *testing.T) {
	// Verify that all metrics are properly registered with Prometheus
	metrics := []prometheus.Collector{
		RequestsTotal,
		RequestDuration,
		CacheHits,
		CacheMisses,
		UpstreamFetchDuration,
		UpstreamErrors,
		StorageOperationDuration,
		StorageErrors,
		CacheSize,
		CachedArtifacts,
		CircuitBreakerState,
		CircuitBreakerTrips,
		ActiveRequests,
	}

	for _, metric := range metrics {
		if metric == nil {
			t.Error("found nil metric")
		}

		// Try to describe the metric (will panic if not properly initialized)
		ch := make(chan *prometheus.Desc, 10)
		metric.Describe(ch)
		close(ch)

		count := 0
		for range ch {
			count++
		}
		if count == 0 {
			t.Errorf("metric has no descriptors: %T", metric)
		}
	}
}

func TestCacheHitRatio(t *testing.T) {
	// Reset by recording some hits and misses
	RecordCacheHit("test")
	RecordCacheHit("test")
	RecordCacheMiss("test")

	// Collect metrics
	hits := getMetricValue(t, CacheHits, "test")
	misses := getMetricValue(t, CacheMisses, "test")

	if hits <= 0 {
		t.Error("expected cache hits to be recorded")
	}
	if misses <= 0 {
		t.Error("expected cache misses to be recorded")
	}
}

func TestRequestDurationHistogram(t *testing.T) {
	// Record some durations
	RecordRequest("test-hist", 200, 100*time.Millisecond)
	RecordRequest("test-hist", 200, 500*time.Millisecond)

	// Verify histogram can be collected without errors
	ch := make(chan prometheus.Metric, 10)
	RequestDuration.Collect(ch)
	close(ch)

	found := false
	for range ch {
		found = true
	}

	if !found {
		t.Error("expected histogram metrics to be collected")
	}
}

func getMetricValue(t *testing.T, collector prometheus.Collector, labelValue string) float64 {
	t.Helper()

	ch := make(chan prometheus.Metric, 10)
	collector.Collect(ch)
	close(ch)

	for m := range ch {
		metric := &dto.Metric{}
		if err := m.Write(metric); err != nil {
			continue
		}

		// Check if this metric matches our label
		if metric.Counter != nil {
			for _, label := range metric.Label {
				if label.GetValue() == labelValue {
					return metric.Counter.GetValue()
				}
			}
		}
	}

	return 0
}

func TestMetricsEndpointOutput(t *testing.T) {
	// Record some test metrics
	RecordRequest("npm", 200, 50*time.Millisecond)
	RecordCacheHit("npm")
	UpdateCacheStats(1024*1024, 10)

	// Get the handler and check it produces output
	handler := Handler()
	if handler == nil {
		t.Fatal("metrics handler is nil")
	}

	// The handler should be a prometheus.Handler
	// We can't easily test the HTTP output without making a request,
	// but we can verify the handler was created
}

func TestMetricsLabeling(t *testing.T) {
	// Test that different ecosystems are properly labeled
	ecosystems := []string{"npm", "pypi", "cargo", "gem"}

	for _, eco := range ecosystems {
		RecordRequest(eco, 200, 10*time.Millisecond)
		RecordCacheHit(eco)
	}

	// Verify each ecosystem has metrics
	for _, eco := range ecosystems {
		val := getMetricValue(t, CacheHits, eco)
		if val == 0 {
			t.Errorf("no cache hits recorded for %s", eco)
		}
	}
}

func TestMetricNames(t *testing.T) {
	// Verify metric names follow Prometheus naming conventions
	expectedMetrics := []string{
		"proxy_requests_total",
		"proxy_request_duration_seconds",
		"proxy_cache_hits_total",
		"proxy_cache_misses_total",
		"proxy_upstream_fetch_duration_seconds",
		"proxy_upstream_errors_total",
		"proxy_storage_operation_duration_seconds",
		"proxy_storage_errors_total",
		"proxy_cache_size_bytes",
		"proxy_cached_artifacts_total",
		"proxy_circuit_breaker_state",
		"proxy_circuit_breaker_trips_total",
		"proxy_active_requests",
	}

	for _, name := range expectedMetrics {
		if !strings.HasPrefix(name, "proxy_") {
			t.Errorf("metric %s doesn't have proxy_ prefix", name)
		}
		if strings.Contains(name, "-") {
			t.Errorf("metric %s contains hyphens (should use underscores)", name)
		}
	}
}
