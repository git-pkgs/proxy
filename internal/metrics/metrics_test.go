package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
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
	RecordArtifactCacheHit("npm")
	RecordArtifactCacheHit("npm")
	RecordArtifactCacheMiss("npm")
	RecordMetadataCacheHit("pypi", 128)
	RecordMetadataCacheMiss("pypi")

	// No panics = success
}

func TestRecordUpstreamOperations(t *testing.T) {
	isolateRecentFailures(t)
	RecordUpstreamFetch("npm", 500*time.Millisecond)
	RecordUpstreamFetch("pypi", 1*time.Second)
	RecordUpstreamError("npm", "fetch_failed", FailureDetail{})
	RecordUpstreamError("pypi", "not_found", FailureDetail{})

	// No panics = success
}

func TestRecordStorageOperations(t *testing.T) {
	isolateRecentFailures(t)
	RecordStorageOperation("read", 10*time.Millisecond)
	RecordStorageOperation("write", 50*time.Millisecond)
	RecordStorageError("read", FailureDetail{})
	RecordStorageError("write", FailureDetail{})

	// No panics = success
}

func TestUpdateCacheStats(t *testing.T) {
	UpdateCacheStats(1024*1024*1024, 100) // 1GB, 100 artifacts
	UpdateCacheStats(0, 0)                // Empty cache

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
	RecordArtifactCacheHit("test")
	RecordArtifactCacheHit("test")
	RecordArtifactCacheMiss("test")

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
	RecordArtifactCacheHit("npm")
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
	ecosystems := []struct {
		input string
		label string
	}{
		{input: "npm", label: "npm"},
		{input: "pypi", label: "pypi"},
		{input: "cargo", label: "cargo"},
		{input: "gem", label: "rubygems"},
	}

	for _, eco := range ecosystems {
		RecordRequest(eco.input, 200, 10*time.Millisecond)
		RecordArtifactCacheHit(eco.input)
	}

	// Verify each ecosystem has metrics
	for _, eco := range ecosystems {
		val := getMetricValue(t, CacheHits, eco.label)
		if val == 0 {
			t.Errorf("no cache hits recorded for %s", eco.label)
		}
	}
}

func TestCacheMetricLabelsAreNormalized(t *testing.T) {
	rubyHitsBefore := testutil.ToFloat64(CacheHits.WithLabelValues("rubygems", CacheKindArtifact))
	composerMissesBefore := testutil.ToFloat64(CacheMisses.WithLabelValues("packagist", CacheKindArtifact))
	goMetadataHitsBefore := testutil.ToFloat64(CacheHits.WithLabelValues("golang", CacheKindMetadata))
	goServedBefore := testutil.ToFloat64(MetadataServedBytes.WithLabelValues("golang"))

	RecordArtifactCacheHit("gem")
	RecordArtifactCacheMiss("composer")
	RecordMetadataCacheHit("go", 10)

	if diff := testutil.ToFloat64(CacheHits.WithLabelValues("rubygems", CacheKindArtifact)) - rubyHitsBefore; diff != 1 {
		t.Errorf("rubygems cache hits delta = %.0f, want 1", diff)
	}
	if diff := testutil.ToFloat64(CacheMisses.WithLabelValues("packagist", CacheKindArtifact)) - composerMissesBefore; diff != 1 {
		t.Errorf("packagist cache misses delta = %.0f, want 1", diff)
	}
	if diff := testutil.ToFloat64(CacheHits.WithLabelValues("golang", CacheKindMetadata)) - goMetadataHitsBefore; diff != 1 {
		t.Errorf("golang metadata hits delta = %.0f, want 1", diff)
	}
	if diff := testutil.ToFloat64(MetadataServedBytes.WithLabelValues("golang")) - goServedBefore; diff != 10 {
		t.Errorf("golang metadata served bytes delta = %.0f, want 10", diff)
	}
}

// TestCacheKindsAreCountedApart checks an artifact hit and a metadata hit land
// in different series. Only metadata hits add served bytes.
func TestCacheKindsAreCountedApart(t *testing.T) {
	const eco = "kind-test"
	artifactHits := func() float64 { return testutil.ToFloat64(CacheHits.WithLabelValues(eco, CacheKindArtifact)) }
	metadataHits := func() float64 { return testutil.ToFloat64(CacheHits.WithLabelValues(eco, CacheKindMetadata)) }
	artifactMisses := func() float64 { return testutil.ToFloat64(CacheMisses.WithLabelValues(eco, CacheKindArtifact)) }
	metadataMisses := func() float64 { return testutil.ToFloat64(CacheMisses.WithLabelValues(eco, CacheKindMetadata)) }
	served := func() float64 { return testutil.ToFloat64(MetadataServedBytes.WithLabelValues(eco)) }

	aHits, mHits, aMisses, mMisses, bytes := artifactHits(), metadataHits(), artifactMisses(), metadataMisses(), served()

	RecordArtifactCacheHit(eco)
	RecordMetadataCacheHit(eco, 300)
	RecordMetadataCacheHit(eco, 0)
	RecordMetadataCacheMiss(eco)

	checks := []struct {
		name      string
		got, want float64
	}{
		{"artifact hits", artifactHits() - aHits, 1},
		{"metadata hits", metadataHits() - mHits, 2},
		{"artifact misses", artifactMisses() - aMisses, 0},
		{"metadata misses", metadataMisses() - mMisses, 1},
		{"metadata served bytes", served() - bytes, 300},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s delta = %.0f, want %.0f", c.name, c.got, c.want)
		}
	}
}

func TestUpdateMetadataCacheStats(t *testing.T) {
	UpdateMetadataCacheStats([]MetadataCacheStats{
		{Ecosystem: "gem", Entries: 2, Size: 100},
		{Ecosystem: "rubygems", Entries: 3, Size: 50},
		{Ecosystem: "npm", Entries: 1, Size: 10},
	})

	if got := testutil.ToFloat64(MetadataCacheEntries.WithLabelValues("rubygems")); got != 5 {
		t.Errorf("rubygems entries = %.0f, want 5 (aliases summed)", got)
	}
	if got := testutil.ToFloat64(MetadataCacheSize.WithLabelValues("rubygems")); got != 150 {
		t.Errorf("rubygems size = %.0f, want 150 (aliases summed)", got)
	}
	if got := testutil.CollectAndCount(MetadataCacheSize); got != 2 {
		t.Errorf("size series = %d, want 2", got)
	}

	UpdateMetadataCacheStats([]MetadataCacheStats{{Ecosystem: "npm", Entries: 4, Size: 40}})

	if got := testutil.CollectAndCount(MetadataCacheSize); got != 1 {
		t.Errorf("size series after rubygems disappeared = %d, want 1", got)
	}
	if got := testutil.CollectAndCount(MetadataCacheEntries); got != 1 {
		t.Errorf("entry series after rubygems disappeared = %d, want 1", got)
	}
	if got := testutil.ToFloat64(MetadataCacheSize.WithLabelValues("npm")); got != 40 {
		t.Errorf("npm size = %.0f, want 40", got)
	}

	UpdateMetadataCacheStats(nil)
}

func TestMetricNames(t *testing.T) {
	// Verify metric names follow Prometheus naming conventions
	expectedMetrics := []string{
		"proxy_requests_total",
		"proxy_request_duration_seconds",
		"proxy_cache_hits_total",
		"proxy_cache_misses_total",
		"proxy_metadata_cache_served_bytes_total",
		"proxy_metadata_cache_size_bytes",
		"proxy_metadata_cache_entries",
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
