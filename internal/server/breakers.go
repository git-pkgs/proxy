package server

import (
	"log/slog"
	"sync"

	"github.com/git-pkgs/proxy/internal/metrics"
)

// Gauge values for proxy_circuit_breaker_state. The fetcher reports only open
// or closed, so half-open (1) is never published.
const (
	breakerGaugeClosed = 0
	breakerGaugeOpen   = 2
)

const (
	breakerStateOpen   = "open"
	breakerStateClosed = "closed"
)

// breakerStateSource reports circuit breaker state per registry host, keyed by
// host, with values breakerStateOpen or breakerStateClosed. Implemented by
// fetch.CircuitBreakerFetcher.
type breakerStateSource interface {
	GetBreakerState() map[string]string
}

// breakerMonitor mirrors the artifact fetcher's per-registry circuit breaker
// state into Prometheus metrics, the health report, and the log.
//
// Breaker state lives only in the fetcher's memory. While a breaker is open
// every artifact fetch for that host that misses the cache fails without
// reaching the upstream, which looks identical to an upstream outage from the
// outside: metadata still serves (it does not go through the fetcher), other
// registries still serve, and /health reports the database and storage as fine.
// Publishing the state makes that distinguishable.
type breakerMonitor struct {
	source breakerStateSource
	logger *slog.Logger

	// mu guards seen, which holds one entry per registry that has tripped at
	// least once in this process — the only registries published as metrics.
	mu   sync.Mutex
	seen map[string]string
}

func newBreakerMonitor(source breakerStateSource, logger *slog.Logger) *breakerMonitor {
	if logger == nil {
		logger = slog.Default()
	}
	return &breakerMonitor{
		source: source,
		logger: logger,
		seen:   map[string]string{},
	}
}

// snapshot returns the current state of every breaker the fetcher has created,
// keyed by registry host, and mirrors it into the breaker metrics as a side
// effect. It returns nil for a nil monitor so callers that build a Server
// without a fetcher (tests) need no special case.
//
// Only registries that have tripped at least once are published as metrics.
// The fetcher creates a breaker per host it fetches from, and for some
// ecosystems that host comes from upstream metadata rather than configuration
// (composer takes it from a package's dist.url, helm from the chart URLs in
// index.yaml), so publishing every host would let upstream content grow the
// series count for the life of the process. A host that has never tripped
// carries no information a series could convey; once it trips it keeps
// reporting, including the 0 that marks its recovery. /health is a per-request
// response rather than a persistent series, so it reports every breaker.
//
// Trips are counted on the closed→open transitions observed between calls,
// because the fetcher exposes current state rather than trip events: a breaker
// that opens and recovers entirely between two calls is not counted.
func (m *breakerMonitor) snapshot() map[string]string {
	if m == nil || m.source == nil {
		return nil
	}

	states := m.source.GetBreakerState()

	m.mu.Lock()
	defer m.mu.Unlock()

	for registry, state := range states {
		previous, published := m.seen[registry]

		switch {
		case state == breakerStateOpen && previous != breakerStateOpen:
			metrics.RecordCircuitBreakerTrip(registry)
			m.logger.Error("circuit breaker open, artifact fetches for this registry "+
				"fail without contacting it", "registry", registry)
		case state == breakerStateClosed && previous == breakerStateOpen:
			m.logger.Info("circuit breaker closed", "registry", registry)
		case state == breakerStateClosed && !published:
			// Never tripped: nothing to publish.
			continue
		}

		gauge := breakerGaugeClosed
		if state == breakerStateOpen {
			gauge = breakerGaugeOpen
		}
		metrics.UpdateCircuitBreakerState(registry, gauge)
		m.seen[registry] = state
	}

	return states
}
