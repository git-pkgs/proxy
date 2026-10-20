package gitcache

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
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
	base, _, unregister := rl.register(upstream.URL+"/repo", 0, nil)

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
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.Copy(w, io.LimitReader(zeros{}, size))
	}))
	defer upstream.Close()
	rl := newTestRelay(t)
	base, route, unregister := rl.register(upstream.URL+"/repo", limit, nil)
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
		t.Errorf("exceeded %v after relaying %d of %d bytes", route.exceeded.Load(), route.relayed.Load(), size)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
