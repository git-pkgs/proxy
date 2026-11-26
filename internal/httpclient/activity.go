package httpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ActivityWindow is how far back UpstreamActivity counts requests.
const ActivityWindow = 15 * time.Minute

// activityBuckets splits ActivityWindow into one bucket per minute.
const activityBuckets = int(ActivityWindow / time.Minute)

// UpstreamActivity records, per configured upstream host, how its recent
// requests went, for the upstreams page. It lives in process memory, so each
// instance sees only its own requests and starts empty.
//
// Only hosts it was created with are tracked: others come from upstream
// content, such as Composer dist URLs, and would grow it without bound.
type UpstreamActivity struct {
	now func() time.Time

	mu    sync.Mutex
	hosts map[string]*hostActivity
}

type hostActivity struct {
	lastSuccess, lastFailure time.Time
	lastFailureKind          string
	buckets                  [activityBuckets]activityBucket
}

type activityBucket struct {
	minute             int64
	requests, failures int
	total, slowest     time.Duration
}

// HostActivity is how a host's requests went within ActivityWindow, and when
// it last answered and last failed at all.
type HostActivity struct {
	Requests, Failures int
	Mean, Slowest      time.Duration
	LastSuccess        time.Time
	LastFailure        time.Time
	// LastFailureKind says how the last failure failed, such as timeout or
	// HTTP 503, without the error text: that can name internal hosts.
	LastFailureKind string
}

// NewUpstreamActivity tracks the given hosts, matched without case.
func NewUpstreamActivity(hosts []string) *UpstreamActivity {
	a := &UpstreamActivity{now: time.Now, hosts: make(map[string]*hostActivity, len(hosts))}
	for _, host := range hosts {
		if host != "" {
			a.hosts[strings.ToLower(host)] = &hostActivity{}
		}
	}
	return a
}

// Transport records the requests that pass through base. A nil activity
// returns base.
func (a *UpstreamActivity) Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if a == nil {
		return base
	}
	return activityTransport{activity: a, base: base}
}

type activityTransport struct {
	activity *UpstreamActivity
	base     http.RoundTripper
}

// RoundTrip records the exchange once it is over: at once when it failed or
// has no body, and otherwise when the body is read to its end, fails, or is
// closed. Response time is to the headers either way.
func (t activityTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := hostKey(req.URL)
	start := t.activity.now()
	resp, err := t.base.RoundTrip(req)
	took := t.activity.now().Sub(start)
	kind := failureKind(resp, err)
	if kind != "" || resp.Body == nil || resp.Body == http.NoBody || !t.activity.tracks(host) {
		t.activity.record(host, took, kind)
		return resp, err
	}
	resp.Body = &activityBody{ReadCloser: resp.Body, done: func(kind string) { t.activity.record(host, took, kind) }}
	return resp, err
}

// activityBody records its response when it ends. A body closed before its
// end was being delivered, so it counts as answered.
type activityBody struct {
	io.ReadCloser
	once sync.Once
	done func(kind string)
}

func (b *activityBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	switch {
	case errors.Is(err, io.EOF):
		b.finish("")
	case err != nil:
		b.finish(failureKind(nil, err))
	}
	return n, err
}

func (b *activityBody) Close() error {
	b.finish("")
	return b.ReadCloser.Close()
}

func (b *activityBody) finish(kind string) {
	b.once.Do(func() { b.done(kind) })
}

// failureKind returns how an exchange failed, "" when the upstream answered,
// or "-" when the client gave up, which says nothing about the upstream.
// Answers other than 429 and 5xx count as answered: a 404 is a missing
// package and a 401 an auth challenge.
func failureKind(resp *http.Response, err error) string {
	if err != nil {
		var netErr net.Error
		switch {
		case errors.Is(err, context.Canceled):
			return "-"
		case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
			return "timeout"
		default:
			return "connection failed"
		}
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return "rate limited"
	case resp.StatusCode >= http.StatusInternalServerError:
		return fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return ""
}

func (a *UpstreamActivity) record(host string, took time.Duration, kind string) {
	if kind == "-" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	h, ok := a.hosts[host]
	if !ok {
		return
	}
	// Read under the lock, so later outcomes never get earlier times.
	now := a.now()
	b := h.bucket(now)
	b.requests++
	b.total += took
	b.slowest = max(b.slowest, took)
	if kind == "" {
		h.lastSuccess = now
		return
	}
	b.failures++
	h.lastFailure, h.lastFailureKind = now, kind
}

// bucket returns the bucket for now's minute, emptied if it last held an
// older minute.
func (h *hostActivity) bucket(now time.Time) *activityBucket {
	minute := minuteOf(now)
	b := &h.buckets[minute%int64(activityBuckets)]
	if b.minute != minute {
		*b = activityBucket{minute: minute}
	}
	return b
}

// minuteOf numbers the minute t falls in.
func minuteOf(t time.Time) int64 {
	return t.Unix() / int64(time.Minute/time.Second)
}

// tracks reports whether host is one of the configured upstreams.
func (a *UpstreamActivity) tracks(host string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.hosts[host]
	return ok
}

// Host returns a host's activity, and false for a host that isn't tracked.
func (a *UpstreamActivity) Host(host string) (HostActivity, bool) {
	if a == nil {
		return HostActivity{}, false
	}
	oldest := minuteOf(a.now()) - int64(activityBuckets) + 1
	a.mu.Lock()
	defer a.mu.Unlock()
	h, ok := a.hosts[strings.ToLower(host)]
	if !ok {
		return HostActivity{}, false
	}
	out := HostActivity{LastSuccess: h.lastSuccess, LastFailure: h.lastFailure, LastFailureKind: h.lastFailureKind}
	var total time.Duration
	for _, b := range h.buckets {
		if b.minute < oldest {
			continue
		}
		out.Requests += b.requests
		out.Failures += b.failures
		total += b.total
		out.Slowest = max(out.Slowest, b.slowest)
	}
	if out.Requests > 0 {
		out.Mean = total / time.Duration(out.Requests)
	}
	return out, true
}
