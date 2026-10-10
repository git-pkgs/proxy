package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

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
		out, _, err := c.rewrite(context.Background(), key, []byte("doc"), r.fn)
		if err != nil || string(out) != "DOC" {
			t.Fatalf("rewrite = %q, %v", out, err)
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
		out, _, err := c.rewrite(context.Background(), key, []byte(doc), r.fn)
		if err != nil || string(out) != strings.ToUpper(doc) {
			t.Fatalf("rewrite(%s) = %q, %v", doc, out, err)
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
		out, _, err := c.rewrite(context.Background(), key, []byte("doc"), r.fn)
		if err != nil {
			t.Error(err)
		}
		outs <- string(out)
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
		_, _, err := c.rewrite(context.Background(), key, []byte("doc"), r.fn)
		first <- err
	}()
	<-r.entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := c.rewrite(ctx, key, []byte("doc"), r.fn); !errors.Is(err, context.Canceled) {
		t.Errorf("waiter err = %v, want context.Canceled", err)
	}
	close(r.release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if out, _, err := c.rewrite(context.Background(), key, []byte("doc"), r.fn); err != nil || string(out) != "DOC" {
		t.Errorf("cached rewrite = %q, %v", out, err)
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
		_, _, _ = c.rewrite(context.Background(), keyFor(doc), []byte(doc), r.fn)
	}
	_, _, _ = c.rewrite(context.Background(), keyFor("aaa"), []byte("aaa"), r.fn) // aaa is now the most recent
	_, _, _ = c.rewrite(context.Background(), keyFor("ccc"), []byte("ccc"), r.fn) // evicts bbb

	before := r.calls.Load()
	_, _, _ = c.rewrite(context.Background(), keyFor("aaa"), []byte("aaa"), r.fn)
	_, _, _ = c.rewrite(context.Background(), keyFor("ccc"), []byte("ccc"), r.fn)
	if got := r.calls.Load() - before; got != 0 {
		t.Errorf("aaa and ccc rewritten %d times, want both still cached", got)
	}
	_, _, _ = c.rewrite(context.Background(), keyFor("bbb"), []byte("bbb"), r.fn)
	if got := r.calls.Load() - before; got != 1 {
		t.Errorf("bbb should have been evicted and rewritten")
	}
}

func TestRewriteCache_OutputLargerThanCacheIsNotStored(t *testing.T) {
	c := newRewriteCache(2)
	var r countingRewrite
	key := rewriteCacheKey("npm", "http://proxy", "big", []byte("big"))

	_, _, _ = c.rewrite(context.Background(), key, []byte("big"), r.fn)
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
		if _, _, err := c.rewrite(context.Background(), key, []byte("doc"), failing); err == nil {
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

// TestCachedRewrite_CooldownBypassesCache asserts that with cooldown on every
// request is rewritten: cooldown filtering depends on the current time, so a
// cached rewrite could keep hiding a version after its cooldown ends.
func TestCachedRewrite_CooldownBypassesCache(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	proxy.SetMetadataRewriteCacheSize(1 << 20)
	proxy.Cooldown = &cooldown.Config{Default: "3d"}
	var r countingRewrite

	for range 2 {
		_, _ = proxy.cachedRewrite(context.Background(), "npm", "http://proxy", "left-pad", []byte("doc"), "", r.keeping)
	}
	if got := r.calls.Load(); got != 2 {
		t.Errorf("rewrites = %d, want 2", got)
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
