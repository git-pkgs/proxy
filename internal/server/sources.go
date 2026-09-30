package server

import (
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// maxTrackedSources bounds the in-memory source table, because the caller set
// is not under the proxy's control.
//
// When it is full the least recently seen caller is evicted rather than the new
// one refused. Refusing would freeze the table on whoever happened to arrive
// first, which is the wrong answer in exactly the deployment this is for:
// containerised CI gives every job a fresh address, so the table would fill
// with dead entries within minutes and every live caller would land in the
// overflow row. Evicted totals move into that row, so nothing is lost from the
// sums; only the per-caller breakdown ages out.
const maxTrackedSources = 200

// unknownClient labels a request whose User-Agent names no recognised tool.
const unknownClient = "other"

// knownClients maps the leading token of a User-Agent to a stable client name.
//
// The value set is deliberately closed. Client names become Prometheus labels,
// and a User-Agent is attacker-controlled, so anything unrecognised collapses
// to unknownClient rather than minting a new time series per request.
var knownClients = map[string]string{
	"npm":               "npm",
	"pnpm":              "pnpm",
	"yarn":              "yarn",
	"bun":               "bun",
	"node":              "npm",
	"pip":               "pip",
	"poetry":            "poetry",
	"uv":                "uv",
	"twine":             "twine",
	"python-requests":   "pip",
	"gocommand":         "go",
	"go-http-client":    "go",
	"cargo":             "cargo",
	"maven":             "maven",
	"apache-maven":      "maven",
	"aether":            "maven",
	"gradle":            "gradle",
	"nuget":             "nuget",
	"nuget-client":      "nuget",
	"composer":          "composer",
	"bundler":           "bundler",
	"rubygems":          "bundler",
	"gem":               "bundler",
	"docker":            "docker",
	"containerd":        "containerd",
	"skopeo":            "skopeo",
	"buildkit":          "buildkit",
	"helm":              "helm",
	"apt":               "apt",
	"debian":            "apt",
	"libdnf":            "dnf",
	"dnf":               "dnf",
	"urlgrabber":        "dnf",
	"apk":               "apk",
	"conda":             "conda",
	"mamba":             "conda",
	"conan":             "conan",
	"hex":               "hex",
	"mix":               "hex",
	"dart":              "pub",
	"pub":               "pub",
	"swift":             "swift",
	"julia":             "julia",
	"r":                 "cran",
	"curl":              "curl",
	"wget":              "wget",
	"mozilla":           "browser",
	"gitlab-runner":     "gitlab-runner",
	"github-actions":    "github-actions",
	"jenkins":           "jenkins",
	"renovate":          "renovate",
	"dependabot":        "dependabot",
	"prometheus":        "prometheus",
	"kube-probe":        "kube-probe",
	"blackbox_exporter": "prometheus",
}

// clientName reduces a User-Agent to one of the names in knownClients.
//
// Package managers put their own name first ("pip/21.2.4 {...}",
// "GoCommand/1 (+https://go.dev/cmd/go)"), so the leading token identifies the
// tool without needing to parse the rest.
func clientName(userAgent string) string {
	if userAgent == "" {
		return unknownClient
	}

	token := userAgent
	if i := strings.IndexAny(token, "/ ("); i >= 0 {
		token = token[:i]
	}

	if name, ok := knownClients[strings.ToLower(strings.TrimSpace(token))]; ok {
		return name
	}
	return unknownClient
}

// clientAddr returns the address a request should be attributed to.
//
// X-Forwarded-For is honoured only when trustForwarded is set, because any
// client can send the header: behind an ingress it is the only way to see past
// the load balancer, and in front of one it is a way to forge attribution.
//
// The forwarded value must parse as an IP. A load balancer that appends to the
// header leaves the leftmost entry caller-controlled, and that string becomes a
// map key held for the process lifetime and a line in the access log, so an
// arbitrary-length value would be a way to spend the proxy's memory.
func clientAddr(r *http.Request, trustForwarded bool) string {
	if trustForwarded {
		// Leftmost entry is the original client; the rest are hops.
		first, _, _ := strings.Cut(r.Header.Get("X-Forwarded-For"), ",")
		if ip := net.ParseIP(strings.TrimSpace(first)); ip != nil {
			return ip.String()
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// trustsForwardedFor reports whether X-Forwarded-For should be believed.
//
// cfg is nil on a partially-constructed Server, which tests do build, so this
// answers false rather than dereferencing it.
func (s *Server) trustsForwardedFor() bool {
	return s.cfg != nil && s.cfg.AccessLog.TrustForwardedFor
}

// sourceKey identifies a caller by address and by the tool it was running, so
// two tools on one machine are counted apart.
type sourceKey struct {
	Addr   string
	Client string
}

type sourceStat struct {
	Requests int64
	Bytes    int64
	LastSeen time.Time
}

// sourceTracker accumulates per-caller request and byte counts.
//
// This is process-lifetime state held in memory, like the Prometheus registry
// and unlike the cache figures: it starts empty and is lost on restart. Caller
// addresses are never published as metric labels — the set is unbounded and
// outside the proxy's control — so this table, and the access log, are where
// per-address detail lives.
//
// The zero value is usable.
type sourceTracker struct {
	mu       sync.Mutex
	max      int
	stats    map[sourceKey]*sourceStat
	overflow sourceStat
	// evictions counts fold-ins to the overflow row, not distinct callers: a
	// caller that is evicted, returns, and is evicted again counts twice.
	// Counting callers instead would mean keeping every key ever seen, which
	// is the unbounded set the limit exists to avoid.
	evictions int64
}

func (t *sourceTracker) limit() int {
	if t.max <= 0 {
		return maxTrackedSources
	}
	return t.max
}

// Record attributes one completed request to a caller.
func (t *sourceTracker) Record(addr, client string, bytes int64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.stats == nil {
		t.stats = make(map[sourceKey]*sourceStat)
	}

	key := sourceKey{Addr: addr, Client: client}
	stat, ok := t.stats[key]
	if !ok {
		if len(t.stats) >= t.limit() {
			t.evictOldestLocked()
		}
		stat = &sourceStat{}
		t.stats[key] = stat
	}

	stat.Requests++
	stat.Bytes += bytes
	stat.LastSeen = time.Now()
}

// evictOldestLocked folds the least recently seen caller into the overflow row.
func (t *sourceTracker) evictOldestLocked() {
	var oldest sourceKey
	var oldestStat *sourceStat
	for k, s := range t.stats {
		if oldestStat == nil || s.LastSeen.Before(oldestStat.LastSeen) {
			oldest, oldestStat = k, s
		}
	}
	if oldestStat == nil {
		return
	}

	t.overflow.Requests += oldestStat.Requests
	t.overflow.Bytes += oldestStat.Bytes
	if oldestStat.LastSeen.After(t.overflow.LastSeen) {
		t.overflow.LastSeen = oldestStat.LastSeen
	}
	t.evictions++
	delete(t.stats, oldest)
}

// SourceRow is one caller's row on the analytics page.
type SourceRow struct {
	Addr     string
	Client   string
	Requests string
	Bytes    string
	LastSeen string
	// IsOverflow marks the row that stands for every caller past the limit.
	IsOverflow bool
}

// Top returns the busiest callers by bytes served, most first.
func (t *sourceTracker) Top(limit int) []SourceRow {
	t.mu.Lock()
	defer t.mu.Unlock()

	type entry struct {
		key  sourceKey
		stat sourceStat
	}

	entries := make([]entry, 0, len(t.stats))
	for k, s := range t.stats {
		entries = append(entries, entry{key: k, stat: *s})
	}

	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		switch {
		case a.stat.Bytes != b.stat.Bytes:
			return a.stat.Bytes > b.stat.Bytes
		case a.stat.Requests != b.stat.Requests:
			return a.stat.Requests > b.stat.Requests
		default:
			return a.key.Addr < b.key.Addr
		}
	})

	// Callers past the limit are summarised rather than dropped, alongside any
	// that were evicted, so the rows still add up to what the page reports
	// above them.
	rest := sourceStat{Requests: t.overflow.Requests, Bytes: t.overflow.Bytes, LastSeen: t.overflow.LastSeen}
	hidden := 0
	if limit > 0 && len(entries) > limit {
		for _, e := range entries[limit:] {
			rest.Requests += e.stat.Requests
			rest.Bytes += e.stat.Bytes
			if e.stat.LastSeen.After(rest.LastSeen) {
				rest.LastSeen = e.stat.LastSeen
			}
			hidden++
		}
		entries = entries[:limit]
	}

	rows := make([]SourceRow, 0, len(entries)+1)
	for _, e := range entries {
		rows = append(rows, SourceRow{
			Addr:     e.key.Addr,
			Client:   e.key.Client,
			Requests: formatCount(e.stat.Requests),
			Bytes:    formatSize(e.stat.Bytes),
			LastSeen: formatTimeAgo(e.stat.LastSeen),
		})
	}

	if rest.Requests > 0 {
		rows = append(rows, SourceRow{
			Addr:       "other callers",
			Client:     overflowLabel(hidden, t.evictions),
			Requests:   formatCount(rest.Requests),
			Bytes:      formatSize(rest.Bytes),
			LastSeen:   formatTimeAgo(rest.LastSeen),
			IsOverflow: true,
		})
	}
	return rows
}

// overflowLabel describes what the overflow row stands for. The two counts are
// kept apart because only one of them is exact: hidden is a count of callers
// currently tracked below the cut, while evictions counts fold-ins, which can
// exceed the number of distinct callers behind them.
func overflowLabel(hidden int, evictions int64) string {
	switch {
	case hidden > 0 && evictions > 0:
		return formatCount(int64(hidden)) + " not shown, " + formatCount(evictions) + " evicted"
	case evictions > 0:
		return formatCount(evictions) + " evicted"
	default:
		return formatCount(int64(hidden)) + " not shown"
	}
}

// Count reports how many distinct callers are being tracked individually.
func (t *sourceTracker) Count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.stats)
}
