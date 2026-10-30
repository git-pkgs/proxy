package handler

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

// countingRewrite upper-cases its input and counts calls. If entered is set it
// is closed when the first call starts, and the call then waits on release.
type countingRewrite struct {
	calls   atomic.Int64
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *countingRewrite) fn(in []byte) ([]byte, error) {
	r.calls.Add(1)
	if r.entered != nil {
		r.once.Do(func() { close(r.entered) })
		<-r.release
	}
	return []byte(strings.ToUpper(string(in))), nil
}

func (r *countingRewrite) keeping(in []byte) ([]byte, []string, error) {
	out, err := r.fn(in)
	return out, nil, err
}

func TestRewriteCache_RepeatServesCachedRewrite(t *testing.T) {
	c := newRewriteCache(1 << 20)
	var r countingRewrite
	key := rewriteCacheKey("npm", "http://proxy", "left-pad", []byte("doc"))

	for range 3 {
		m, err := c.rewrite(context.Background(), key, []byte("doc"), 0, r.fn)
		if err != nil || string(m.content()) != "DOC" {
			t.Fatalf("rewrite = %q, %v", m.content(), err)
		}
	}
	if got := r.calls.Load(); got != 1 {
		t.Errorf("rewrites = %d, want 1", got)
	}
}

func TestRewriteCache_NewUpstreamBytesRewriteAgain(t *testing.T) {
	c := newRewriteCache(1 << 20)
	var r countingRewrite

	for _, doc := range []string{"v1", "v2"} {
		key := rewriteCacheKey("npm", "http://proxy", "left-pad", []byte(doc))
		m, err := c.rewrite(context.Background(), key, []byte(doc), 0, r.fn)
		if err != nil || string(m.content()) != strings.ToUpper(doc) {
			t.Fatalf("rewrite(%s) = %q, %v", doc, m.content(), err)
		}
	}
	if got := r.calls.Load(); got != 2 {
		t.Errorf("rewrites = %d, want 2", got)
	}
}

// TestRewriteCache_ConcurrentRequestsShareRewrite asserts that requests
// arriving while a document is being rewritten wait for that rewrite instead
// of running their own. Callers that arrive after it finishes hit the cache,
// so the count holds however the goroutines are scheduled.
func TestRewriteCache_ConcurrentRequestsShareRewrite(t *testing.T) {
	c := newRewriteCache(1 << 20)
	r := countingRewrite{entered: make(chan struct{}), release: make(chan struct{})}
	key := rewriteCacheKey("npm", "http://proxy", "typescript", []byte("doc"))
	const n = 10

	outs := make(chan string, n)
	run := func() {
		m, err := c.rewrite(context.Background(), key, []byte("doc"), 0, r.fn)
		if err != nil {
			t.Error(err)
		}
		outs <- string(m.content())
	}
	go run()
	<-r.entered
	for range n - 1 {
		go run()
	}
	close(r.release)

	for range n {
		if out := <-outs; out != "DOC" {
			t.Errorf("out = %q", out)
		}
	}
	if got := r.calls.Load(); got != 1 {
		t.Errorf("rewrites = %d, want 1", got)
	}
}

// TestRewriteCache_WaiterLeavingKeepsRewrite asserts that a caller waiting on
// another's rewrite returns its own context error when its client leaves,
// while the rewrite completes and is cached for the next request.
func TestRewriteCache_WaiterLeavingKeepsRewrite(t *testing.T) {
	c := newRewriteCache(1 << 20)
	r := countingRewrite{entered: make(chan struct{}), release: make(chan struct{})}
	key := rewriteCacheKey("npm", "http://proxy", "typescript", []byte("doc"))

	first := make(chan error, 1)
	go func() {
		_, err := c.rewrite(context.Background(), key, []byte("doc"), 0, r.fn)
		first <- err
	}()
	<-r.entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.rewrite(ctx, key, []byte("doc"), 0, r.fn); !errors.Is(err, context.Canceled) {
		t.Errorf("waiter err = %v, want context.Canceled", err)
	}
	close(r.release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if m, err := c.rewrite(context.Background(), key, []byte("doc"), 0, r.fn); err != nil || string(m.content()) != "DOC" {
		t.Errorf("cached rewrite = %q, %v", m.content(), err)
	}
	if got := r.calls.Load(); got != 1 {
		t.Errorf("rewrites = %d, want 1", got)
	}
}

func TestRewriteCache_EvictsLeastRecentlyUsed(t *testing.T) {
	c := newRewriteCache(6) // room for two three-byte outputs
	var r countingRewrite
	keyFor := func(doc string) string { return rewriteCacheKey("npm", "http://proxy", doc, []byte(doc)) }

	for _, doc := range []string{"aaa", "bbb"} {
		_, _ = c.rewrite(context.Background(), keyFor(doc), []byte(doc), 0, r.fn)
	}
	_, _ = c.rewrite(context.Background(), keyFor("aaa"), []byte("aaa"), 0, r.fn) // aaa is now the most recent
	_, _ = c.rewrite(context.Background(), keyFor("ccc"), []byte("ccc"), 0, r.fn) // evicts bbb

	before := r.calls.Load()
	_, _ = c.rewrite(context.Background(), keyFor("aaa"), []byte("aaa"), 0, r.fn)
	_, _ = c.rewrite(context.Background(), keyFor("ccc"), []byte("ccc"), 0, r.fn)
	if got := r.calls.Load() - before; got != 0 {
		t.Errorf("aaa and ccc rewritten %d times, want both still cached", got)
	}
	_, _ = c.rewrite(context.Background(), keyFor("bbb"), []byte("bbb"), 0, r.fn)
	if got := r.calls.Load() - before; got != 1 {
		t.Errorf("bbb should have been evicted and rewritten")
	}
}

func TestRewriteCache_OutputLargerThanCacheIsNotStored(t *testing.T) {
	c := newRewriteCache(2)
	var r countingRewrite
	key := rewriteCacheKey("npm", "http://proxy", "big", []byte("big"))

	_, _ = c.rewrite(context.Background(), key, []byte("big"), 0, r.fn)
	if c.len() != 0 {
		t.Errorf("cached %d entries, want 0", c.len())
	}
}

func TestRewriteCache_ErrorsAreNotCached(t *testing.T) {
	c := newRewriteCache(1 << 20)
	var calls int
	failing := func([]byte) ([]byte, error) { calls++; return nil, errors.New("bad document") }
	key := rewriteCacheKey("npm", "http://proxy", "broken", []byte("doc"))

	for range 2 {
		if _, err := c.rewrite(context.Background(), key, []byte("doc"), 0, failing); err == nil {
			t.Fatal("expected the rewrite error")
		}
	}
	if calls != 2 {
		t.Errorf("rewrites = %d, want 2", calls)
	}
}

func TestCachedRewrite_DisabledRewritesEveryTime(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	proxy.SetMetadataRewriteCacheSize(0)
	var r countingRewrite

	for range 2 {
		_, _ = proxy.cachedRewrite(context.Background(), "npm", "http://proxy", "left-pad", []byte("doc"), "", r.keeping)
	}
	if got := r.calls.Load(); got != 2 {
		t.Errorf("rewrites = %d, want 2", got)
	}
}

// TestCachedRewrite_CooldownExpires asserts that with cooldown on a rewrite
// is cached for maxCooldownRewriteTTL only: cooldown filtering depends on the
// current time, so a cached rewrite would otherwise keep hiding a version
// after its cooldown ends.
func TestCachedRewrite_CooldownExpires(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	clock := &testClock{t: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}
	proxy.now = clock.now
	proxy.SetMetadataRewriteCacheSize(1 << 20)
	proxy.Cooldown = &cooldown.Config{Default: "3d"}
	var r countingRewrite

	for range 2 {
		_, _ = proxy.cachedRewrite(context.Background(), "npm", "http://proxy", "left-pad", []byte("doc"), "", r.keeping)
	}
	if got := r.calls.Load(); got != 1 {
		t.Errorf("rewrites within the TTL = %d, want 1", got)
	}

	clock.advance(maxCooldownRewriteTTL)
	_, _ = proxy.cachedRewrite(context.Background(), "npm", "http://proxy", "left-pad", []byte("doc"), "", r.keeping)
	if got := r.calls.Load(); got != 2 {
		t.Errorf("rewrites after the TTL = %d, want 2", got)
	}
}

// TestRewriteCacheExpiredEntryFreesRoom asserts that an expired entry is
// dropped when looked up, so its size no longer counts.
func TestRewriteCacheExpiredEntryFreesRoom(t *testing.T) {
	c := newRewriteCache(1 << 20)
	clock := &testClock{t: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}
	c.now = clock.now
	upper := func(b []byte) ([]byte, error) { return bytes.ToUpper(b), nil }
	if _, err := c.rewrite(context.Background(), "k", []byte("doc"), time.Minute, upper); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Minute)
	if _, ok := c.get("k"); ok {
		t.Fatal("expired entry was served")
	}
	if c.size != 0 || c.order.Len() != 0 {
		t.Errorf("size %d, entries %d after expiry; want 0", c.size, c.order.Len())
	}
}

// TestMetadataHandlers_ServeCachedRewrite asserts that repeated requests for
// the same npm or Composer metadata reuse one rewrite and return the same
// bytes, with the download URLs pointing at the proxy.
func TestMetadataHandlers_ServeCachedRewrite(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/left-pad":
			_, _ = w.Write([]byte(`{"name":"left-pad","versions":{"1.3.0":{"dist":{"tarball":"https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz"}}}}`))
		case "/p2/vendor/pkg.json":
			_, _ = w.Write([]byte(`{"packages":{"vendor/pkg":[{"version":"1.0.0","dist":{"type":"zip","url":"https://example.com/pkg-1.0.0.zip"}}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	proxy.SetMetadataRewriteCacheSize(1 << 20)

	cases := []struct {
		name    string
		handler http.Handler
		path    string
	}{
		{"npm", NewNPMHandler(proxy, "http://proxy.example", upstream.URL).Routes(), "/left-pad"},
		{"composer", NewComposerHandlerWithUpstreams(proxy, "http://proxy.example", upstream.URL, upstream.URL).Routes(), "/p2/vendor/pkg.json"},
	}
	for i, c := range cases {
		var bodies []string
		for range 2 {
			rec := httptest.NewRecorder()
			c.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status %d", c.name, rec.Code)
			}
			bodies = append(bodies, rec.Body.String())
		}
		if bodies[0] != bodies[1] || !strings.Contains(bodies[0], "http://proxy.example") {
			t.Errorf("%s: bodies %q and %q", c.name, bodies[0], bodies[1])
		}
		if got := proxy.rewrites.len(); got != i+1 {
			t.Errorf("%s: cached rewrites = %d, want %d", c.name, got, i+1)
		}
	}
}

// len reports how many rewrites are cached.
func (c *rewriteCache) len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// TestCooldownRewriteTTL checks that a rewrite filtered by cooldown is cached
// for a 24th of the shortest cooldown, between a minute and an hour.
func TestCooldownRewriteTTL(t *testing.T) {
	for _, tc := range []struct {
		shortest, want time.Duration
	}{
		{0, time.Hour},
		{72 * time.Hour, time.Hour},
		{24 * time.Hour, time.Hour},
		{2 * time.Hour, 5 * time.Minute},
		{10 * time.Minute, time.Minute},
	} {
		p := &Proxy{ShortestCooldown: tc.shortest}
		if got := p.cooldownRewriteTTL(); got != tc.want {
			t.Errorf("shortest cooldown %v: TTL = %v, want %v", tc.shortest, got, tc.want)
		}
	}
}
