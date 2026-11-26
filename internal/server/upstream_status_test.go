package server

import (
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/httpclient"
)

func TestUpstreamStatusLevels(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	for _, tc := range []struct {
		name    string
		in      upstreamSignals
		level   string
		summary string
	}{
		{"never contacted", upstreamSignals{}, statusIdle, "No requests since the proxy started"},
		{"answering", upstreamSignals{activity: httpclient.HostActivity{Requests: 5, LastSuccess: ago(time.Minute)}}, statusOK, "5 requests in the last 15 minutes"},
		{"one request", upstreamSignals{activity: httpclient.HostActivity{Requests: 1}}, statusOK, "1 request in the last 15 minutes"},
		{"quiet since", upstreamSignals{activity: httpclient.HostActivity{LastSuccess: ago(time.Hour)}}, statusOK, "No requests in the last 15 minutes"},
		{"some failures", upstreamSignals{activity: httpclient.HostActivity{Requests: 10, Failures: 2, LastSuccess: ago(time.Minute), LastFailure: ago(2 * time.Minute), LastFailureKind: "HTTP 502"}}, statusWarn, "2 of 10 requests failed in the last 15 minutes"},
		{"half failing", upstreamSignals{activity: httpclient.HostActivity{Requests: 4, Failures: 2, LastFailureKind: "timeout"}}, statusError, "2 of 4 requests failed in the last 15 minutes"},
		{"only request failed", upstreamSignals{activity: httpclient.HostActivity{Requests: 1, Failures: 1, LastFailureKind: "timeout"}}, statusError, "1 of 1 requests failed in the last 15 minutes"},
		{"failed last, a while ago", upstreamSignals{activity: httpclient.HostActivity{LastSuccess: ago(3 * time.Hour), LastFailure: ago(2 * time.Hour), LastFailureKind: "HTTP 503"}}, statusWarn, "The last request failed"},
		{"slow", upstreamSignals{activity: httpclient.HostActivity{Requests: 3, Mean: 6 * time.Second}}, statusWarn, "Slow to answer"},
		{"breaker open", upstreamSignals{breakerOpen: true, activity: httpclient.HostActivity{Requests: 3}}, statusError, "Circuit breaker open"},
		{"rate limited", upstreamSignals{heldUntil: now.Add(time.Minute), activity: httpclient.HostActivity{Requests: 3}}, statusError, "Rate limited until 12:01 UTC"},
		{"hold over", upstreamSignals{heldUntil: ago(time.Minute), activity: httpclient.HostActivity{Requests: 3}}, statusOK, "3 requests"},
	} {
		got := upstreamStatus(tc.in, now)
		if got.Level != tc.level || !strings.HasPrefix(got.Summary, tc.summary) {
			t.Errorf("%s: level %q, summary %q; want %q, %q", tc.name, got.Level, got.Summary, tc.level, tc.summary)
		}
	}
}

func TestUpstreamStatusDetails(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	got := upstreamStatus(upstreamSignals{activity: httpclient.HostActivity{
		Requests: 4, Failures: 1, Mean: 250 * time.Millisecond, Slowest: 2 * time.Second,
		LastSuccess: now.Add(-time.Minute), LastFailure: now.Add(-5 * time.Minute), LastFailureKind: "HTTP 503",
	}}, now)
	if got.Mean != "250ms" || got.Slowest != "2s" || got.LastFailureKind != "HTTP 503" {
		t.Errorf("details = %+v", got)
	}
	if got.LastSuccess != "1 min ago" || got.LastFailure != "5 mins ago" {
		t.Errorf("times = %q, %q", got.LastSuccess, got.LastFailure)
	}
}

func TestWorstUpstreamStatus(t *testing.T) {
	for _, tc := range []struct {
		levels []string
		want   string
	}{
		{[]string{statusIdle, statusOK}, statusOK},
		{[]string{statusOK, statusWarn, statusIdle}, statusWarn},
		{[]string{statusWarn, statusError, statusOK}, statusError},
		{[]string{statusIdle}, statusIdle},
	} {
		var rows []UpstreamRow
		for _, l := range tc.levels {
			rows = append(rows, UpstreamRow{Status: UpstreamStatus{Level: l}})
		}
		if got := worstStatus(rows); got.Level != tc.want {
			t.Errorf("%v: worst = %q, want %q", tc.levels, got.Level, tc.want)
		}
	}
}

func TestWorstUpstreamStatusKeepsTheFirst(t *testing.T) {
	rows := []UpstreamRow{
		{Status: UpstreamStatus{Level: statusWarn, Summary: "first"}},
		{Status: UpstreamStatus{Level: statusWarn, Summary: "second"}},
		{Status: UpstreamStatus{Level: statusIdle, Summary: "idle"}},
	}
	if got := worstStatus(rows); got.Summary != "first" {
		t.Errorf("worst = %q, want the first of equals", got.Summary)
	}
}

// Breakers and holds are keyed by host, the breakers as the fetcher took it
// from the URL.
func TestUpstreamStatusesUseBreakersAndHolds(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	activity := httpclient.NewUpstreamActivity([]string{"registry.npmjs.org", "pypi.org", "crates.io"})
	ecosystems := []UpstreamEcosystem{{Upstreams: []UpstreamRow{{host: "registry.npmjs.org"}, {host: "pypi.org"}, {host: "crates.io"}}}}
	upstreamStatuses(ecosystems, activity,
		map[string]string{"Registry.NPMjs.org": breakerStateOpen, "crates.io": breakerStateClosed},
		map[string]time.Time{"pypi.org": now.Add(time.Minute)}, now)

	rows := ecosystems[0].Upstreams
	if !strings.HasPrefix(rows[0].Status.Summary, "Circuit breaker open") {
		t.Errorf("npm = %+v, want its open breaker", rows[0].Status)
	}
	if !strings.HasPrefix(rows[1].Status.Summary, "Rate limited") {
		t.Errorf("pypi = %+v, want its hold", rows[1].Status)
	}
	if rows[2].Status.Level != statusIdle {
		t.Errorf("crates = %+v, want a closed breaker to change nothing", rows[2].Status)
	}
	if ecosystems[0].Status.Level != statusError {
		t.Errorf("ecosystem = %+v, want its worst upstream", ecosystems[0].Status)
	}
}
