package gitcache

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestRelay(t *testing.T) *relay {
	t.Helper()
	rl, err := newRelay(&http.Client{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rl.close() })
	return rl
}

func TestRelayOnlyForwardsFetchRequests(t *testing.T) {
	var forwarded atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		forwarded.Add(1)
	}))
	defer upstream.Close()
	rl := newTestRelay(t)
	base, _, unregister := rl.register(t.Context(), upstream.URL+"/repo", 0, nil)

	refused := []struct{ method, url string }{
		{http.MethodGet, rl.base + "/unknown/info/refs?service=git-upload-pack"},
		{http.MethodGet, base + "/info/refs?service=git-receive-pack"},
		{http.MethodPost, base + "/git-receive-pack"},
		{http.MethodGet, base + "/HEAD"},
		{http.MethodGet, base + "/git-upload-pack"},
	}
	for _, r := range refused {
		req, _ := http.NewRequest(r.method, r.url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s: status %d, want 404", r.method, r.url, resp.StatusCode)
		}
	}
	if got := forwarded.Load(); got != 0 {
		t.Errorf("forwarded %d refused requests", got)
	}

	unregister()
	resp, err := http.Get(base + "/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || forwarded.Load() != 0 {
		t.Errorf("unregistered route: status %d, forwarded %d", resp.StatusCode, forwarded.Load())
	}
}

// TestRelayStopsAtLimit checks a fetch over git.max_repo_size is cut off
// rather than downloaded in full and thrown away.
func TestRelayStopsAtLimit(t *testing.T) {
	const size, limit = 4 << 20, 64 << 10
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.Copy(w, io.LimitReader(zeros{}, size))
	}))
	defer upstream.Close()
	var logs strings.Builder
	var logMu sync.Mutex
	rl, err := newRelay(&http.Client{}, slog.New(slog.NewTextHandler(lockedWriter{&logMu, &logs}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rl.close() })
	base, route, unregister := rl.register(t.Context(), upstream.URL+"/repo", limit, nil)
	defer unregister()

	resp, err := http.Post(base+"/git-upload-pack", requestType, strings.NewReader(pktFlush))
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if err == nil {
		t.Errorf("read %d bytes without an error", n)
	}
	if !route.exceeded.Load() || route.relayed.Load() > size/2 {
		logMu.Lock()
		defer logMu.Unlock()
		t.Errorf("exceeded %v after relaying %d of %d bytes; relay log:\n%s", route.exceeded.Load(), route.relayed.Load(), size, logs.String())
	}
}

// TestRelayReadsTheRequestWhileResponding checks the relay keeps passing on
// the request body after the response has started, as an HTTP/1 server
// otherwise closes the body on the first flush and the relay cuts off the
// upstream mid-response.
func TestRelayReadsTheRequestWhileResponding(t *testing.T) {
	head := strings.Repeat("x", 64<<10)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
			t.Error(err)
		}
		_, _ = io.WriteString(w, head)
		_ = http.NewResponseController(w).Flush()
		body, _ := io.ReadAll(r.Body)
		_, _ = io.WriteString(w, "got:"+string(body))
	}))
	defer upstream.Close()
	rl := newTestRelay(t)
	base, _, unregister := rl.register(t.Context(), upstream.URL+"/repo", 0, nil)
	defer unregister()

	pr, pw := io.Pipe()
	responses := make(chan *http.Response, 1)
	go func() {
		resp, err := http.Post(base+"/git-upload-pack", requestType, pr)
		if err != nil {
			t.Error(err)
			close(responses)
			return
		}
		responses <- resp
	}()
	defer func() { _ = pw.Close() }()
	_, _ = io.WriteString(pw, "first")
	var resp *http.Response
	select {
	case r, ok := <-responses:
		if !ok {
			return
		}
		resp = r
	case <-time.After(5 * time.Second):
		t.Fatal("no response while the request body was still open")
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.WriteString(pw, "second")
	_ = pw.Close()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response after %d bytes: %v", len(got), err)
	}
	if want := head + "got:firstsecond"; string(got) != want {
		t.Errorf("response ends %q, want it to end %q", tail(string(got)), tail(want))
	}
}

func tail(s string) string { return s[max(0, len(s)-32):] }

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
