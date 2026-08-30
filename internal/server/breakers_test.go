package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/metrics"
	"github.com/git-pkgs/registries/fetch"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// cbThreshold in registries/fetch: failures inside the breaker's rolling window
// needed to trip it. The library counts the window, not a consecutive run, so
// these fetches only have to land close together, which they do.
const breakerTripFailures = 5

// fakeBreakerSource reports breaker state without a real fetcher, so tests can
// drive transitions that would otherwise need to wait out a 30s backoff.
type fakeBreakerSource struct {
	states map[string]string
}

func (f *fakeBreakerSource) GetBreakerState() map[string]string {
	states := make(map[string]string, len(f.states))
	for registry, state := range f.states {
		states[registry] = state
	}
	return states
}

// newTrippedMonitor returns a monitor over a real circuit-breaker fetcher whose
// breaker for the test server's host has been tripped by repeated 5xx
// responses, plus that host.
func newTrippedMonitor(t *testing.T) (*breakerMonitor, string) {
	t.Helper()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(upstream.Close)

	monitor, fetcher, host := newMonitorFor(t, upstream)
	for range breakerTripFailures {
		if _, err := fetcher.Fetch(context.Background(), upstream.URL+"/artifact.tgz"); err == nil {
			t.Fatal("fetch against a 502 upstream should fail")
		}
	}
	return monitor, host
}

// newMonitorFor builds a monitor over a circuit-breaker fetcher that talks to
// srv through its own client, bypassing the SSRF dial gate that would otherwise
// refuse the loopback address.
func newMonitorFor(t *testing.T, srv *httptest.Server) (
	monitor *breakerMonitor, fetcher *fetch.CircuitBreakerFetcher, host string,
) {
	t.Helper()

	base := fetch.NewFetcher(
		fetch.WithHTTPClient(srv.Client()),
		fetch.WithMaxRetries(0),
	)
	t.Cleanup(func() { _ = base.Close() })

	fetcher = fetch.NewCircuitBreakerFetcher(base)
	return newBreakerMonitor(fetcher, slog.New(slog.DiscardHandler)),
		fetcher,
		strings.TrimPrefix(srv.URL, "http://")
}

// metricValue returns the value of the series carrying registry=want, and
// whether such a series exists at all. It collects rather than calling
// WithLabelValues, which would create the series it is looking for.
func metricValue(t *testing.T, collector prometheus.Collector, want string) (value float64, found bool) {
	t.Helper()

	ch := make(chan prometheus.Metric, 64)
	go func() {
		collector.Collect(ch)
		close(ch)
	}()

	for metric := range ch {
		var parsed dto.Metric
		if err := metric.Write(&parsed); err != nil {
			t.Fatalf("writing metric: %v", err)
		}
		for _, label := range parsed.GetLabel() {
			if label.GetName() != "registry" || label.GetValue() != want {
				continue
			}
			if gauge := parsed.GetGauge(); gauge != nil {
				return gauge.GetValue(), true
			}
			return parsed.GetCounter().GetValue(), true
		}
	}
	return 0, false
}

// resetSeries drops any series for host left behind by an earlier test, since
// httptest ports can be reused within a process and the metrics registry is
// global. Absolute trip counts stay meaningful after it.
func resetSeries(host string) {
	metrics.CircuitBreakerState.DeleteLabelValues(host)
	metrics.CircuitBreakerTrips.DeleteLabelValues(host)
}

func TestBreakerMonitor_OpenBreakerReportedAndCounted(t *testing.T) {
	monitor, host := newTrippedMonitor(t)
	resetSeries(host)

	if state := monitor.snapshot()[host]; state != breakerStateOpen {
		t.Fatalf("state for %s = %q, want open", host, state)
	}

	gauge, found := metricValue(t, metrics.CircuitBreakerState, host)
	if !found {
		t.Fatalf("no state gauge published for %s", host)
	}
	if gauge != breakerGaugeOpen {
		t.Errorf("state gauge = %v, want %v", gauge, breakerGaugeOpen)
	}
	if trips, _ := metricValue(t, metrics.CircuitBreakerTrips, host); trips != 1 {
		t.Errorf("trips = %v, want 1", trips)
	}

	// A breaker that stays open is one trip, not one per scrape.
	monitor.snapshot()
	monitor.snapshot()
	if trips, _ := metricValue(t, metrics.CircuitBreakerTrips, host); trips != 1 {
		t.Errorf("trips after further snapshots = %v, want 1", trips)
	}
}

// A breaker per fetched host is created even for hosts that come from upstream
// metadata (composer dist.url, helm chart URLs), so publishing a series for
// every one of them would let upstream content grow the series count without
// bound. Only registries that have actually tripped are published.
func TestBreakerMonitor_HealthyRegistryPublishesNoSeries(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("artifact"))
	}))
	defer upstream.Close()

	monitor, fetcher, host := newMonitorFor(t, upstream)
	resetSeries(host)

	artifact, err := fetcher.Fetch(context.Background(), upstream.URL+"/artifact.tgz")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	_, _ = io.Copy(io.Discard, artifact.Body)
	_ = artifact.Body.Close()

	// The breaker exists and is reported to /health...
	if state := monitor.snapshot()[host]; state != breakerStateClosed {
		t.Fatalf("state for %s = %q, want closed", host, state)
	}
	// ...but carries no metric series.
	if _, found := metricValue(t, metrics.CircuitBreakerState, host); found {
		t.Errorf("state gauge published for %s, want none until it trips", host)
	}
	if _, found := metricValue(t, metrics.CircuitBreakerTrips, host); found {
		t.Errorf("trip counter published for %s, want none until it trips", host)
	}
}

// Once a registry has tripped it keeps reporting, so recovery is visible as a
// transition to 0 rather than as a series that disappears.
func TestBreakerMonitor_RecoveryReportedAfterTrip(t *testing.T) {
	const host = "recovering.example.com"

	resetSeries(host)

	source := &fakeBreakerSource{states: map[string]string{host: breakerStateOpen}}
	monitor := newBreakerMonitor(source, slog.New(slog.DiscardHandler))

	monitor.snapshot()
	if gauge, _ := metricValue(t, metrics.CircuitBreakerState, host); gauge != breakerGaugeOpen {
		t.Fatalf("state gauge = %v, want %v", gauge, breakerGaugeOpen)
	}

	source.states[host] = breakerStateClosed
	if state := monitor.snapshot()[host]; state != breakerStateClosed {
		t.Fatalf("state for %s = %q, want closed", host, state)
	}
	gauge, found := metricValue(t, metrics.CircuitBreakerState, host)
	if !found {
		t.Fatal("state gauge disappeared after recovery, want 0")
	}
	if gauge != breakerGaugeClosed {
		t.Errorf("state gauge = %v, want %v", gauge, breakerGaugeClosed)
	}
	if trips, _ := metricValue(t, metrics.CircuitBreakerTrips, host); trips != 1 {
		t.Errorf("trips = %v, want 1", trips)
	}

	// Tripping again after recovery counts a second trip.
	source.states[host] = breakerStateOpen
	monitor.snapshot()
	if trips, _ := metricValue(t, metrics.CircuitBreakerTrips, host); trips != 2 {
		t.Errorf("trips after re-trip = %v, want 2", trips)
	}
}

func TestBreakerMonitor_NilSafe(t *testing.T) {
	var nilMonitor *breakerMonitor
	if states := nilMonitor.snapshot(); states != nil {
		t.Errorf("nil monitor snapshot = %v, want nil", states)
	}
	if states := newBreakerMonitor(nil, nil).snapshot(); states != nil {
		t.Errorf("snapshot without a state source = %v, want nil", states)
	}
}

func TestHealthEndpoint_ReportsOpenBreaker(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	monitor, host := newTrippedMonitor(t)
	ts.server.breakers = monitor

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	// An unreachable upstream is not this proxy being unhealthy: the breaker
	// state is reported, but the probe still passes.
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	var resp HealthResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("status = %q, want ok", resp.Status)
	}
	if got := resp.CircuitBreakers[host]; got != breakerStateOpen {
		t.Errorf("circuit_breakers[%s] = %q, want open", host, got)
	}
}

func TestHealthEndpoint_OmitsBreakersWhenNoneExist(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if strings.Contains(w.Body.String(), "circuit_breakers") {
		t.Errorf("body should omit circuit_breakers when no breaker exists: %s", w.Body.String())
	}
}

// blockingBreakerSource holds each state read open until the test releases it,
// and reports every entry, so a read that escaped the monitor lock announces
// itself. inFlight is a second net: it catches an overlap anywhere in the test,
// not just while the release channel is held.
type blockingBreakerSource struct {
	states   map[string]string
	entered  chan struct{}
	release  chan struct{}
	inFlight atomic.Int32
	overlap  atomic.Bool
}

func (b *blockingBreakerSource) GetBreakerState() map[string]string {
	if b.inFlight.Add(1) > 1 {
		b.overlap.Store(true)
	}
	defer b.inFlight.Add(-1)

	b.entered <- struct{}{}
	<-b.release

	states := make(map[string]string, len(b.states))
	for registry, state := range b.states {
		states[registry] = state
	}
	return states
}

// /health requests and /metrics scrapes call snapshot concurrently. The state
// read has to happen under the same lock as the seen updates it feeds: read
// outside it, two snapshots straddling a transition can apply their reads to
// seen in the opposite order and count one trip twice.
func TestBreakerMonitor_SnapshotSerializesStateReads(t *testing.T) {
	const host = "serialized.example.com"

	resetSeries(host)

	source := &blockingBreakerSource{
		states:  map[string]string{host: breakerStateOpen},
		entered: make(chan struct{}, 2),
		release: make(chan struct{}),
	}
	monitor := newBreakerMonitor(source, slog.New(slog.DiscardHandler))

	first := make(chan map[string]string, 1)
	go func() { first <- monitor.snapshot() }()

	// The first snapshot is inside GetBreakerState now, holding the lock.
	<-source.entered

	second := make(chan map[string]string, 1)
	go func() { second <- monitor.snapshot() }()

	// Nothing can release the first read, so the second cannot get past the
	// lock: an unlocked read would have queued an entry on the channel.
	select {
	case <-source.entered:
		t.Fatal("second snapshot read breaker state while the first still held the lock")
	case <-time.After(100 * time.Millisecond):
	}

	close(source.release)

	if state := (<-first)[host]; state != breakerStateOpen {
		t.Errorf("first snapshot state for %s = %q, want open", host, state)
	}
	// Only reached once the first snapshot has returned the lock.
	<-source.entered
	if state := (<-second)[host]; state != breakerStateOpen {
		t.Errorf("second snapshot state for %s = %q, want open", host, state)
	}

	if source.overlap.Load() {
		t.Error("two snapshots read breaker state concurrently")
	}
	// The same open breaker seen twice is one trip, which is what the paired
	// read and update buys.
	if trips, _ := metricValue(t, metrics.CircuitBreakerTrips, host); trips != 1 {
		t.Errorf("trips = %v, want 1", trips)
	}
}
