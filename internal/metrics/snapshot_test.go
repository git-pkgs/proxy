package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// newTestRegistry builds an isolated registry so these tests are not affected
// by counters other tests in this package have already incremented.
func newTestRegistry(t *testing.T) (*prometheus.Registry, *prometheus.CounterVec, *prometheus.HistogramVec, *prometheus.GaugeVec) {
	t.Helper()

	reg := prometheus.NewRegistry()
	counter := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_errors_total", Help: "t"},
		[]string{"ecosystem", "kind"},
	)
	hist := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "test_duration_seconds", Help: "t"},
		[]string{"op"},
	)
	gauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "test_active", Help: "t"}, []string{})
	reg.MustRegister(counter, hist, gauge)
	return reg, counter, hist, gauge
}

func TestSnapshotSumAndSumBy(t *testing.T) {
	reg, counter, _, gauge := newTestRegistry(t)
	counter.WithLabelValues("npm", "timeout").Add(3)
	counter.WithLabelValues("npm", "refused").Add(2)
	counter.WithLabelValues("pypi", "timeout").Add(5)
	gauge.WithLabelValues().Set(7)

	snap, err := GatherFrom(reg)
	if err != nil {
		t.Fatalf("GatherFrom: %v", err)
	}

	if got := snap.Sum("test_errors_total"); got != 10 {
		t.Errorf("Sum = %v, want 10", got)
	}
	if got := snap.Sum("test_active"); got != 7 {
		t.Errorf("gauge Sum = %v, want 7", got)
	}

	byEco := snap.SumBy("test_errors_total", "ecosystem")
	if byEco["npm"] != 5 || byEco["pypi"] != 5 {
		t.Errorf("SumBy(ecosystem) = %v, want npm=5 pypi=5", byEco)
	}
}

func TestSnapshotHistogram(t *testing.T) {
	reg, _, hist, _ := newTestRegistry(t)
	hist.WithLabelValues("get").Observe(0.010)
	hist.WithLabelValues("get").Observe(0.030)
	hist.WithLabelValues("put").Observe(0.100)

	snap, err := GatherFrom(reg)
	if err != nil {
		t.Fatalf("GatherFrom: %v", err)
	}

	if got := snap.Count("test_duration_seconds"); got != 3 {
		t.Errorf("Count = %d, want 3", got)
	}
	// (0.010 + 0.030 + 0.100) / 3
	if got := snap.Mean("test_duration_seconds"); got < 0.0466 || got > 0.0467 {
		t.Errorf("Mean = %v, want ~0.04667", got)
	}

	// A histogram carries no single value, so Value stays zero.
	for _, s := range snap.Samples("test_duration_seconds") {
		if s.Value != 0 {
			t.Errorf("histogram sample carries Value %v, want 0", s.Value)
		}
		if s.Label("op") == "get" && s.Count != 2 {
			t.Errorf("get count = %d, want 2", s.Count)
		}
	}
}

func TestSnapshotSamplesAreOrdered(t *testing.T) {
	reg, counter, _, _ := newTestRegistry(t)
	for _, eco := range []string{"pypi", "npm", "cargo"} {
		counter.WithLabelValues(eco, "timeout").Inc()
	}

	snap, err := GatherFrom(reg)
	if err != nil {
		t.Fatalf("GatherFrom: %v", err)
	}

	var got []string
	for _, s := range snap.Samples("test_errors_total") {
		got = append(got, s.Label("ecosystem"))
	}
	want := []string{"cargo", "npm", "pypi"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample order = %v, want %v (stable rendering depends on it)", got, want)
		}
	}
}

func TestSnapshotMissingMetric(t *testing.T) {
	reg, _, _, _ := newTestRegistry(t)
	snap, err := GatherFrom(reg)
	if err != nil {
		t.Fatalf("GatherFrom: %v", err)
	}

	if got := snap.Samples("nope_total"); got != nil {
		t.Errorf("Samples of an unknown metric = %v, want nil", got)
	}
	if got := snap.Sum("nope_total"); got != 0 {
		t.Errorf("Sum of an unknown metric = %v, want 0", got)
	}
	if got := snap.Mean("nope_total"); got != 0 {
		t.Errorf("Mean of an unknown metric = %v, want 0", got)
	}
}

// Gather reads the real registry, so every metric this package registers must
// come back. This is the check that a newly added metric is reachable by the UI.
func TestGatherSeesRegisteredMetrics(t *testing.T) {
	RecordCacheHit("npm")
	RecordScanError("npm", "clamav", "timeout")

	snap, err := Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	for _, name := range []string{"proxy_cache_hits_total", "proxy_scan_errors_total"} {
		if len(snap.Samples(name)) == 0 {
			t.Errorf("%s is registered but absent from the snapshot", name)
		}
	}
}
