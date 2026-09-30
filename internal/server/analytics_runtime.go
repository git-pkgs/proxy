package server

import (
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/git-pkgs/proxy/internal/metrics"
)

// RuntimeView holds the process-lifetime figures read out of the Prometheus
// registry. Unlike the cache figures, which are computed from the database and
// survive a restart, everything here starts at zero when the proxy starts.
type RuntimeView struct {
	Available bool

	Requests        string
	ActiveRequests  string
	RequestMean     string
	StatusClasses   []LabelledCount
	CacheHits       string
	CacheMisses     string
	CacheHitRatio   string
	HasCacheTraffic bool

	UpstreamFetches   string
	UpstreamFetchMean string
	UpstreamErrors    []LabelledCount

	StorageOps        []LabelledStat
	StorageErrors     []LabelledCount
	IntegrityFailures []LabelledCount
	ProbeFailures     []LabelledCount

	Breakers     []BreakerRow
	BreakerTrips []LabelledCount

	Scans        []LabelledStat
	ScansBlocked []LabelledCount
	ScanErrors   []LabelledCount
	ScanningOn   bool

	ResponseBytes string
	Clients       []ClientRow
	Sources       []SourceRow
	SourceCount   int
	TrustsForward bool
}

// ClientRow is one client tool's share of requests and bytes.
type ClientRow struct {
	Client   string
	Requests string
	Bytes    string
}

// LabelledCount is a single counter series rendered as a row.
type LabelledCount struct {
	Label string
	Count string
	// Bad marks a failure row, which the template tints.
	Bad bool
}

// LabelledStat is a histogram series rendered as a row: how many observations,
// and their mean.
type LabelledStat struct {
	Label string
	Count string
	Mean  string
}

// BreakerRow is one upstream's circuit breaker state.
type BreakerRow struct {
	Registry string
	State    string
	Open     bool
}

// runtimeView shapes a registry snapshot for the analytics page.
func runtimeView(snap *metrics.Snapshot) RuntimeView {
	if snap == nil {
		return RuntimeView{}
	}

	v := RuntimeView{Available: true}

	requests := snap.Sum("proxy_requests_total")
	v.Requests = formatCount(int64(requests))
	v.ActiveRequests = formatCount(int64(snap.Sum("proxy_active_requests")))
	v.RequestMean = formatDuration(snap.Mean("proxy_request_duration_seconds"))
	v.StatusClasses = statusClasses(snap)

	hits := snap.Sum("proxy_cache_hits_total")
	misses := snap.Sum("proxy_cache_misses_total")
	v.CacheHits = formatCount(int64(hits))
	v.CacheMisses = formatCount(int64(misses))
	v.HasCacheTraffic = hits+misses > 0
	if v.HasCacheTraffic {
		v.CacheHitRatio = strconv.FormatFloat(hits/(hits+misses)*100, 'f', 1, 64) //nolint:mnd // percent
	}

	v.UpstreamFetches = formatCount(int64(snap.Count("proxy_upstream_fetch_duration_seconds")))
	v.UpstreamFetchMean = formatDuration(snap.Mean("proxy_upstream_fetch_duration_seconds"))
	v.UpstreamErrors = failureRows(snap, "proxy_upstream_errors_total", "ecosystem", "error_type")

	v.StorageOps = histogramRows(snap, "proxy_storage_operation_duration_seconds", "operation")
	v.StorageErrors = failureRows(snap, "proxy_storage_errors_total", "operation")
	v.IntegrityFailures = failureRows(snap, "proxy_integrity_failures_total", "ecosystem")
	v.ProbeFailures = failureRows(snap, "proxy_health_probe_failures_total", "step")

	v.Breakers = breakerRows(snap)
	v.BreakerTrips = failureRows(snap, "proxy_circuit_breaker_trips_total", "registry")

	v.Scans = histogramRows(snap, "proxy_scan_duration_seconds", "ecosystem", "scanner")
	v.ScansBlocked = failureRows(snap, "proxy_scan_blocked_total", "ecosystem", "scanner")
	v.ScanErrors = failureRows(snap, "proxy_scan_errors_total", "ecosystem", "scanner", "error_type")
	v.ScanningOn = len(v.Scans) > 0 || len(v.ScansBlocked) > 0 || len(v.ScanErrors) > 0

	v.ResponseBytes = formatSize(int64(snap.Sum("proxy_response_bytes_total")))
	v.Clients = clientRows(snap)

	return v
}

// clientRows pairs each client tool's request count with the bytes it pulled,
// busiest first. Both come from the same closed label set, so the two counters
// line up row for row.
func clientRows(snap *metrics.Snapshot) []ClientRow {
	bytesByClient := snap.SumBy("proxy_client_response_bytes_total", "client")

	type entry struct {
		client   string
		requests float64
		bytes    float64
	}

	entries := make([]entry, 0)
	for _, s := range snap.Samples("proxy_client_requests_total") {
		client := s.Label("client")
		entries = append(entries, entry{
			client:   client,
			requests: s.Value,
			bytes:    bytesByClient[client],
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.bytes != b.bytes {
			return a.bytes > b.bytes
		}
		return a.requests > b.requests
	})

	rows := make([]ClientRow, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, ClientRow{
			Client:   e.client,
			Requests: formatCount(int64(e.requests)),
			Bytes:    formatSize(int64(e.bytes)),
		})
	}
	return rows
}

// statusClasses groups proxy_requests_total into 2xx/3xx/4xx/5xx buckets, which
// is the breakdown worth showing; the per-code detail stays in Prometheus.
func statusClasses(snap *metrics.Snapshot) []LabelledCount {
	byClass := make(map[string]float64)
	for _, s := range snap.Samples("proxy_requests_total") {
		status := s.Label("status")
		if len(status) == 0 {
			continue
		}
		byClass[string(status[0])+"xx"] += s.Value
	}

	classes := make([]string, 0, len(byClass))
	for c := range byClass {
		classes = append(classes, c)
	}
	sort.Strings(classes)

	rows := make([]LabelledCount, 0, len(classes))
	for _, c := range classes {
		rows = append(rows, LabelledCount{
			Label: c,
			Count: formatCount(int64(byClass[c])),
			Bad:   c == "5xx",
		})
	}
	return rows
}

// failureRows renders every non-zero series of a failure counter, keyed by the
// joined values of the given labels. Zero-valued series are dropped: a counter
// that has never fired carries no information, and a wall of zeroes buries the
// rows that matter.
func failureRows(snap *metrics.Snapshot, name string, labels ...string) []LabelledCount {
	var rows []LabelledCount
	for _, s := range snap.Samples(name) {
		if s.Value == 0 {
			continue
		}
		rows = append(rows, LabelledCount{
			Label: joinLabels(s, labels...),
			Count: formatCount(int64(s.Value)),
			Bad:   true,
		})
	}
	return rows
}

// histogramRows renders every observed series of a histogram with its count and mean.
func histogramRows(snap *metrics.Snapshot, name string, labels ...string) []LabelledStat {
	var rows []LabelledStat
	for _, s := range snap.Samples(name) {
		if s.Count == 0 {
			continue
		}
		rows = append(rows, LabelledStat{
			Label: joinLabels(s, labels...),
			Count: formatCount(int64(s.Count)),
			Mean:  formatDuration(s.Mean()),
		})
	}
	return rows
}

func breakerRows(snap *metrics.Snapshot) []BreakerRow {
	var rows []BreakerRow
	for _, s := range snap.Samples("proxy_circuit_breaker_state") {
		state := "closed"
		switch s.Value {
		case 1:
			state = "half-open"
		case 2: //nolint:mnd // 2 = open, per the gauge's documented encoding
			state = "open"
		}
		rows = append(rows, BreakerRow{
			Registry: s.Label("registry"),
			State:    state,
			Open:     s.Value > 0,
		})
	}
	return rows
}

func joinLabels(s metrics.Sample, labels ...string) string {
	out := ""
	for _, l := range labels {
		v := s.Label(l)
		if v == "" {
			continue
		}
		if out != "" {
			out += " · "
		}
		out += v
	}
	if out == "" {
		return "—"
	}
	return out
}

// formatDuration renders a mean latency at a sensible unit. Storage operations
// land in microseconds and upstream fetches in seconds, so a fixed unit would
// read as either 0.000 or an unreadable pile of digits.
func formatDuration(seconds float64) string {
	if seconds <= 0 {
		return "—"
	}

	d := time.Duration(seconds * float64(time.Second))
	switch {
	case d < time.Microsecond:
		return fmt.Sprintf("%.0f ns", float64(d))
	case d < time.Millisecond:
		return fmt.Sprintf("%.0f µs", float64(d)/float64(time.Microsecond))
	case d < time.Second:
		return fmt.Sprintf("%.1f ms", float64(d)/float64(time.Millisecond))
	default:
		return fmt.Sprintf("%.2f s", d.Seconds())
	}
}
