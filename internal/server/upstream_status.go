package server

import (
	"fmt"
	"strings"
	"time"

	"github.com/git-pkgs/proxy/internal/httpclient"
)

// Levels of an upstream's status, shown as a coloured light.
const (
	statusIdle  = "idle"
	statusOK    = "ok"
	statusWarn  = "warn"
	statusError = "error"
)

// slowUpstream is the mean time to an upstream's response headers above
// which it is shown as slow.
const slowUpstream = 5 * time.Second

// latencyPrecision rounds response times of a second or more.
const latencyPrecision = 100 * time.Millisecond

// UpstreamStatus is how an upstream's recent requests went. It carries no
// host or error text: /ui has no auth of its own.
type UpstreamStatus struct {
	Level   string
	Summary string

	Requests, Failures int
	Mean, Slowest      string
	LastSuccess        string
	LastFailure        string
	LastFailureKind    string
}

// upstreamSignals is what the proxy knows about one upstream host.
type upstreamSignals struct {
	activity    httpclient.HostActivity
	breakerOpen bool
	heldUntil   time.Time
}

func upstreamStatus(in upstreamSignals, now time.Time) UpstreamStatus {
	a := in.activity
	s := UpstreamStatus{
		Requests:        a.Requests,
		Failures:        a.Failures,
		LastSuccess:     formatTimeAgoAt(a.LastSuccess, now),
		LastFailure:     formatTimeAgoAt(a.LastFailure, now),
		LastFailureKind: a.LastFailureKind,
	}
	if a.Requests > 0 {
		s.Mean, s.Slowest = formatLatency(a.Mean), formatLatency(a.Slowest)
	}
	window := fmt.Sprintf("in the last %d minutes", int(httpclient.ActivityWindow.Minutes()))
	failed := fmt.Sprintf("%d of %d requests failed %s", a.Failures, a.Requests, window)

	switch {
	case in.breakerOpen:
		s.Level, s.Summary = statusError, "Circuit breaker open: artifact fetches fail without contacting the upstream"
	case in.heldUntil.After(now):
		s.Level, s.Summary = statusError, "Rate limited until "+in.heldUntil.UTC().Format("15:04")+" UTC: requests wait or fail"
	case a.Failures > 0 && (a.Failures == a.Requests || a.Requests >= 3 && 2*a.Failures >= a.Requests):
		s.Level, s.Summary = statusError, failed
	case a.Failures > 0:
		s.Level, s.Summary = statusWarn, failed
	case a.Requests > 0 && a.Mean > slowUpstream:
		s.Level, s.Summary = statusWarn, "Slow to answer: "+s.Mean+" on average "+window
	case a.Requests > 0:
		s.Level, s.Summary = statusOK, requestCount(a.Requests)+" "+window
	case !a.LastFailure.IsZero() && a.LastFailure.After(a.LastSuccess):
		s.Level, s.Summary = statusWarn, "The last request failed ("+a.LastFailureKind+"), "+s.LastFailure
	case !a.LastSuccess.IsZero():
		s.Level, s.Summary = statusOK, "No requests "+window+"; it last answered "+s.LastSuccess
	default:
		s.Level, s.Summary = statusIdle, "No requests since the proxy started"
	}
	return s
}

func requestCount(n int) string {
	if n == 1 {
		return "1 request"
	}
	return fmt.Sprintf("%d requests", n)
}

func formatLatency(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(latencyPrecision).String()
}

// statusSeverity ranks the levels from least to most severe.
var statusSeverity = func() map[string]int {
	m := map[string]int{}
	for i, level := range []string{statusIdle, statusOK, statusWarn, statusError} {
		m[level] = i
	}
	return m
}()

// worstStatus returns the most severe status of rows, so an ecosystem shows
// a problem with any of its upstreams.
func worstStatus(rows []UpstreamRow) UpstreamStatus {
	worst := UpstreamStatus{Level: statusIdle}
	for _, row := range rows {
		if statusSeverity[row.Status.Level] > statusSeverity[worst.Level] {
			worst = row.Status
		}
	}
	return worst
}

// upstreamStatuses fills in the status of every upstream and ecosystem from
// the activity, circuit breakers and rate-limit holds. Breakers and holds are
// keyed by host.
func upstreamStatuses(ecosystems []UpstreamEcosystem, activity *httpclient.UpstreamActivity, breakers map[string]string, holds map[string]time.Time, now time.Time) {
	open := make(map[string]bool, len(breakers))
	for host, state := range breakers {
		if state == breakerStateOpen {
			open[strings.ToLower(host)] = true
		}
	}
	for i := range ecosystems {
		eco := &ecosystems[i]
		for j := range eco.Upstreams {
			row := &eco.Upstreams[j]
			in := upstreamSignals{breakerOpen: open[row.host], heldUntil: holds[row.host]}
			in.activity, _ = activity.Host(row.host)
			row.Status = upstreamStatus(in, now)
		}
		eco.Status = worstStatus(eco.Upstreams)
	}
}

// StatusSummary counts the ecosystems at each level, for the top of the
// upstreams page.
type StatusSummary struct {
	Level, Summary        string
	OK, Warn, Error, Idle int
}

func statusSummary(ecosystems []UpstreamEcosystem) StatusSummary {
	sum := StatusSummary{Level: statusIdle}
	for _, eco := range ecosystems {
		switch eco.Status.Level {
		case statusOK:
			sum.OK++
		case statusWarn:
			sum.Warn++
		case statusError:
			sum.Error++
		default:
			sum.Idle++
		}
		if statusSeverity[eco.Status.Level] > statusSeverity[sum.Level] {
			sum.Level = eco.Status.Level
		}
	}
	plural := func(n int) string {
		if n == 1 {
			return "1 ecosystem"
		}
		return fmt.Sprintf("%d ecosystems", n)
	}
	switch sum.Level {
	case statusError:
		sum.Summary = "Problems with " + plural(sum.Error)
	case statusWarn:
		sum.Summary = "Warnings for " + plural(sum.Warn)
	case statusOK:
		sum.Summary = "All upstreams in use are answering"
	default:
		sum.Summary = "No upstream requests since the proxy started"
	}
	return sum
}
