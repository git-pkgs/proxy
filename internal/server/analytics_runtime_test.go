package server

import (
	"testing"

	"github.com/git-pkgs/proxy/internal/metrics"
	"github.com/prometheus/client_golang/prometheus"
)

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		seconds float64
		want    string
	}{
		{0, "—"},
		{-1, "—"},
		{0.0000004, "400 ns"},
		{0.00041, "410 µs"},
		{0.0081, "8.1 ms"},
		{1.25, "1.25 s"},
	}
	for _, tc := range tests {
		if got := formatDuration(tc.seconds); got != tc.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tc.seconds, got, tc.want)
		}
	}
}

func TestRuntimeViewNilSnapshot(t *testing.T) {
	if v := runtimeView(nil); v.Available {
		t.Error("a nil snapshot must not report itself as available")
	}
}

func TestRuntimeViewShapesRegistry(t *testing.T) {
	reg := prometheus.NewRegistry()

	requests := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "proxy_requests_total", Help: "t"}, []string{"ecosystem", "status"})
	scanErrors := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "proxy_scan_errors_total", Help: "t"}, []string{"ecosystem", "scanner", "error_type"})
	hits := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "proxy_cache_hits_total", Help: "t"}, []string{"ecosystem"})
	misses := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "proxy_cache_misses_total", Help: "t"}, []string{"ecosystem"})
	breaker := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "proxy_circuit_breaker_state", Help: "t"}, []string{"registry"})
	storage := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "proxy_storage_operation_duration_seconds", Help: "t"}, []string{"operation"})
	reg.MustRegister(requests, scanErrors, hits, misses, breaker, storage)

	requests.WithLabelValues("npm", "200").Add(90)
	requests.WithLabelValues("npm", "304").Add(5)
	requests.WithLabelValues("npm", "500").Add(5)
	scanErrors.WithLabelValues("npm", "clamav", "timeout").Add(3)
	hits.WithLabelValues("npm").Add(80)
	misses.WithLabelValues("npm").Add(20)
	breaker.WithLabelValues("registry.npmjs.org").Set(0)
	breaker.WithLabelValues("static.crates.io").Set(2)
	storage.WithLabelValues("get").Observe(0.002)

	snap, err := metrics.GatherFrom(reg)
	if err != nil {
		t.Fatalf("GatherFrom: %v", err)
	}
	v := runtimeView(snap)

	if !v.Available {
		t.Fatal("expected the view to be available")
	}
	if v.Requests != "100" {
		t.Errorf("Requests = %q, want %q", v.Requests, "100")
	}
	if v.CacheHitRatio != "80.0" || !v.HasCacheTraffic {
		t.Errorf("CacheHitRatio = %q (traffic=%v), want 80.0", v.CacheHitRatio, v.HasCacheTraffic)
	}

	// Status codes collapse to classes; 5xx is flagged as a failure row.
	wantClasses := map[string]struct {
		count string
		bad   bool
	}{"2xx": {"90", false}, "3xx": {"5", false}, "5xx": {"5", true}}
	if len(v.StatusClasses) != len(wantClasses) {
		t.Fatalf("got %d status classes, want %d: %+v", len(v.StatusClasses), len(wantClasses), v.StatusClasses)
	}
	for _, row := range v.StatusClasses {
		want, ok := wantClasses[row.Label]
		if !ok {
			t.Errorf("unexpected status class %q", row.Label)
			continue
		}
		if row.Count != want.count || row.Bad != want.bad {
			t.Errorf("%s = %q (bad=%v), want %q (bad=%v)", row.Label, row.Count, row.Bad, want.count, want.bad)
		}
	}

	// The metric that prompted this: it must reach the page with its labels joined.
	if len(v.ScanErrors) != 1 {
		t.Fatalf("expected 1 scan error row, got %+v", v.ScanErrors)
	}
	if v.ScanErrors[0].Label != "npm · clamav · timeout" || v.ScanErrors[0].Count != "3" {
		t.Errorf("scan error row = %+v, want npm · clamav · timeout = 3", v.ScanErrors[0])
	}
	if !v.ScanningOn {
		t.Error("scanning should read as on once a scan metric carries a value")
	}

	if len(v.Breakers) != 2 {
		t.Fatalf("expected 2 breakers, got %+v", v.Breakers)
	}
	var open, closed int
	for _, b := range v.Breakers {
		if b.Open {
			open++
			if b.State != "open" {
				t.Errorf("open breaker state = %q, want %q", b.State, "open")
			}
		} else {
			closed++
		}
	}
	if open != 1 || closed != 1 {
		t.Errorf("breakers: %d open / %d closed, want 1 / 1", open, closed)
	}

	if len(v.StorageOps) != 1 || v.StorageOps[0].Label != "get" || v.StorageOps[0].Mean != "2.0 ms" {
		t.Errorf("StorageOps = %+v, want one 'get' row with a 2.0 ms mean", v.StorageOps)
	}
}

// A counter sitting at zero carries no information and would bury the rows
// that do, so it is left out.
func TestRuntimeViewDropsZeroCounters(t *testing.T) {
	reg := prometheus.NewRegistry()
	errs := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "proxy_storage_errors_total", Help: "t"}, []string{"operation"})
	reg.MustRegister(errs)

	// Touching a label creates the series at zero.
	errs.WithLabelValues("get")
	errs.WithLabelValues("put").Add(2)

	snap, err := metrics.GatherFrom(reg)
	if err != nil {
		t.Fatalf("GatherFrom: %v", err)
	}
	v := runtimeView(snap)

	if len(v.StorageErrors) != 1 {
		t.Fatalf("expected only the non-zero row, got %+v", v.StorageErrors)
	}
	if v.StorageErrors[0].Label != "put" {
		t.Errorf("kept row = %q, want %q", v.StorageErrors[0].Label, "put")
	}
}

// An empty registry must still render, reporting nothing rather than dividing
// by zero or showing a hit rate of NaN.
func TestRuntimeViewEmptyRegistry(t *testing.T) {
	snap, err := metrics.GatherFrom(prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("GatherFrom: %v", err)
	}
	v := runtimeView(snap)

	if !v.Available {
		t.Error("an empty registry is still a valid snapshot")
	}
	if v.HasCacheTraffic {
		t.Error("no traffic recorded, so HasCacheTraffic must be false")
	}
	if v.CacheHitRatio != "" {
		t.Errorf("CacheHitRatio = %q, want empty when nothing was recorded", v.CacheHitRatio)
	}
	if v.Requests != "0" {
		t.Errorf("Requests = %q, want %q", v.Requests, "0")
	}
	if v.RequestMean != "—" {
		t.Errorf("RequestMean = %q, want an em dash", v.RequestMean)
	}
	if v.ScanningOn {
		t.Error("scanning must read as off with no scan metrics")
	}
}
