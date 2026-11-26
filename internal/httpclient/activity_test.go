package httpclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"
)

type stubTripper struct {
	status int
	err    error
	delay  time.Duration
	clock  *fakeClock
	body   io.ReadCloser
}

func (s stubTripper) RoundTrip(*http.Request) (*http.Response, error) {
	if s.clock != nil {
		s.clock.advance(s.delay)
	}
	if s.err != nil {
		return nil, s.err
	}
	body := s.body
	if body == nil {
		body = http.NoBody
	}
	return &http.Response{StatusCode: s.status, Body: body}, nil
}

// failingBody returns data and then err.
type failingBody struct {
	data []byte
	err  error
}

func (b *failingBody) Read(p []byte) (int, error) {
	if len(b.data) > 0 {
		n := copy(p, b.data)
		b.data = b.data[n:]
		return n, nil
	}
	return 0, b.err
}

func (b *failingBody) Close() error { return nil }

type timeoutError struct{}

func (timeoutError) Error() string   { return "read: i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// rawRoundTrip returns the response without reading or closing its body.
func rawRoundTrip(t *testing.T, a *UpstreamActivity, base stubTripper, rawURL string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Transport(base).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestActivity(hosts ...string) (*UpstreamActivity, *fakeClock) {
	clock := &fakeClock{t: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)}
	a := NewUpstreamActivity(hosts)
	a.now = clock.now
	return a, clock
}

func roundTrip(t *testing.T, a *UpstreamActivity, base stubTripper, rawURL string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, _ := a.Transport(base).RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
}

func TestUpstreamActivityCountsOutcomes(t *testing.T) {
	a, clock := newTestActivity("registry.npmjs.org")
	u := "https://registry.npmjs.org/lodash"

	roundTrip(t, a, stubTripper{status: 200, delay: 100 * time.Millisecond, clock: clock}, u)
	// A missing package means the upstream answered.
	roundTrip(t, a, stubTripper{status: 404, delay: 300 * time.Millisecond, clock: clock}, u)
	// A bearer challenge is part of a normal exchange.
	roundTrip(t, a, stubTripper{status: 401, clock: clock}, u)
	roundTrip(t, a, stubTripper{status: 503, clock: clock}, u)

	got, ok := a.Host("registry.npmjs.org")
	if !ok {
		t.Fatal("no activity recorded")
	}
	if got.Requests != 4 || got.Failures != 1 {
		t.Errorf("requests = %d, failures = %d; want 4 and 1", got.Requests, got.Failures)
	}
	if got.LastFailureKind != "HTTP 503" {
		t.Errorf("last failure kind = %q, want HTTP 503", got.LastFailureKind)
	}
	if got.LastSuccess.IsZero() || got.LastFailure.Before(got.LastSuccess) {
		t.Errorf("last success %v, last failure %v; want the failure last", got.LastSuccess, got.LastFailure)
	}
	if got.Slowest != 300*time.Millisecond || got.Mean != 100*time.Millisecond {
		t.Errorf("slowest = %v, mean = %v; want 300ms and 100ms", got.Slowest, got.Mean)
	}
}

func TestUpstreamActivityFailureKinds(t *testing.T) {
	for _, tc := range []struct {
		base stubTripper
		want string
	}{
		{stubTripper{status: 429}, "rate limited"},
		{stubTripper{status: 502}, "HTTP 502"},
		{stubTripper{err: context.DeadlineExceeded}, "timeout"},
		{stubTripper{err: errors.New("dial tcp: connection refused")}, "connection failed"},
	} {
		a, _ := newTestActivity("example.org")
		roundTrip(t, a, tc.base, "https://example.org/x")
		got, _ := a.Host("example.org")
		if got.Failures != 1 || got.LastFailureKind != tc.want {
			t.Errorf("%+v: failures = %d, kind = %q; want 1 and %q", tc.base, got.Failures, got.LastFailureKind, tc.want)
		}
	}
}

// A client that goes away cancels the upstream request; that says nothing
// about the upstream.
func TestUpstreamActivityIgnoresCancelledRequests(t *testing.T) {
	a, _ := newTestActivity("example.org")
	roundTrip(t, a, stubTripper{err: context.Canceled}, "https://example.org/x")
	if got, ok := a.Host("example.org"); ok && got.Requests != 0 {
		t.Errorf("requests = %d, want a cancelled request left out", got.Requests)
	}
}

func TestUpstreamActivityWindow(t *testing.T) {
	a, clock := newTestActivity("example.org")
	roundTrip(t, a, stubTripper{status: 500}, "https://example.org/x")
	clock.advance(10 * time.Minute)
	roundTrip(t, a, stubTripper{status: 200}, "https://example.org/x")

	got, _ := a.Host("example.org")
	if got.Requests != 2 || got.Failures != 1 {
		t.Errorf("within the window: requests = %d, failures = %d; want 2 and 1", got.Requests, got.Failures)
	}

	clock.advance(6 * time.Minute)
	got, _ = a.Host("example.org")
	if got.Requests != 1 || got.Failures != 0 {
		t.Errorf("after the failure left the window: requests = %d, failures = %d; want 1 and 0", got.Requests, got.Failures)
	}
	if got.LastFailure.IsZero() || got.LastFailureKind != "HTTP 500" {
		t.Errorf("last failure is kept after the window: %+v", got)
	}

	clock.advance(time.Hour)
	got, _ = a.Host("example.org")
	if got.Requests != 0 || got.LastSuccess.IsZero() {
		t.Errorf("an hour later: %+v; want no requests in the window and the last success kept", got)
	}
}

// Hosts come from upstream content too, such as Composer dist URLs, so only
// configured upstreams are tracked.
func TestUpstreamActivityTracksOnlyConfiguredHosts(t *testing.T) {
	a, _ := newTestActivity("Registry.NPMjs.org")
	roundTrip(t, a, stubTripper{status: 200}, "https://codeload.github.com/x")
	roundTrip(t, a, stubTripper{status: 200}, "https://REGISTRY.npmjs.org/x")
	if _, ok := a.Host("codeload.github.com"); ok {
		t.Error("tracked a host that isn't a configured upstream")
	}
	if got, ok := a.Host("registry.npmjs.org"); !ok || got.Requests != 1 {
		t.Errorf("configured host: %+v, %v; want one request, matched without case", got, ok)
	}
}

func TestUpstreamActivityNil(t *testing.T) {
	var a *UpstreamActivity
	base := stubTripper{status: 200}
	if a.Transport(base) == nil {
		t.Fatal("nil activity returned no transport")
	}
	if _, ok := a.Host("example.org"); ok {
		t.Error("nil activity reported a host")
	}
}

// Each minute's bucket is reused a window later; it must start empty.
func TestUpstreamActivityReusesBuckets(t *testing.T) {
	a, clock := newTestActivity("example.org")
	roundTrip(t, a, stubTripper{status: 500}, "https://example.org/x")
	clock.advance(ActivityWindow)
	roundTrip(t, a, stubTripper{status: 200}, "https://example.org/x")
	got, _ := a.Host("example.org")
	if got.Requests != 1 || got.Failures != 0 {
		t.Errorf("requests = %d, failures = %d; want the old minute's failure gone", got.Requests, got.Failures)
	}
}

// A body is recorded when it ends, so one that fails after the headers
// counts as the failure it is, and once.
func TestUpstreamActivityRecordsBodyFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		failures int
		kind     string
		counted  bool
	}{
		{"read to the end", io.EOF, 0, "", true},
		{"timed out", timeoutError{}, 1, "timeout", true},
		{"connection lost", io.ErrUnexpectedEOF, 1, "connection failed", true},
		{"client went away", context.Canceled, 0, "", false},
	} {
		a, _ := newTestActivity("example.org")
		resp := rawRoundTrip(t, a, stubTripper{status: 200, body: &failingBody{data: []byte("partial"), err: tc.err}}, "https://example.org/x")
		if got, _ := a.Host("example.org"); got.Requests != 0 {
			t.Errorf("%s: recorded before the body ended: %+v", tc.name, got)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		got, _ := a.Host("example.org")
		wantRequests := 0
		if tc.counted {
			wantRequests = 1
		}
		if got.Requests != wantRequests || got.Failures != tc.failures || got.LastFailureKind != tc.kind {
			t.Errorf("%s: %+v, want %d request, %d failures, kind %q", tc.name, got, wantRequests, tc.failures, tc.kind)
		}
	}
}

// A body the caller closes before its end was being delivered, so it
// counts as answered.
func TestUpstreamActivityBodyClosedEarly(t *testing.T) {
	a, _ := newTestActivity("example.org")
	resp := rawRoundTrip(t, a, stubTripper{status: 200, body: &failingBody{data: []byte("more"), err: timeoutError{}}}, "https://example.org/x")
	_ = resp.Body.Close()
	_ = resp.Body.Close()
	if got, _ := a.Host("example.org"); got.Requests != 1 || got.Failures != 0 {
		t.Errorf("%+v, want one answered request", got)
	}
}

// A failing status is known from the headers.
func TestUpstreamActivityRecordsFailingStatusAtOnce(t *testing.T) {
	a, _ := newTestActivity("example.org")
	rawRoundTrip(t, a, stubTripper{status: 503, body: &failingBody{data: []byte("busy"), err: io.EOF}}, "https://example.org/x")
	if got, _ := a.Host("example.org"); got.Requests != 1 || got.Failures != 1 {
		t.Errorf("%+v, want the 503 recorded without reading its body", got)
	}
}
