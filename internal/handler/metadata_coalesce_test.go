package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// metadataUpstream is a stub registry that counts metadata requests. It
// signals entered when the first request arrives and holds every request until
// release is closed, so a test can keep the first fetch in flight while others
// arrive.
type metadataUpstream struct {
	*httptest.Server
	calls   atomic.Int64
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newMetadataUpstream(t *testing.T, status int) *metadataUpstream {
	t.Helper()
	u := &metadataUpstream{entered: make(chan struct{}), release: make(chan struct{})}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.calls.Add(1)
		u.once.Do(func() { close(u.entered) })
		<-u.release
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Echo the Accept header so a test can tell which variant it got.
		_, _ = w.Write([]byte(`{"accept":"` + r.Header.Get("Accept") + `"}`))
	}))
	t.Cleanup(u.Close)
	return u
}

func metadataTestProxy(t *testing.T, u *metadataUpstream) *Proxy {
	t.Helper()
	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = u.Client()
	return proxy
}

// waitForMetadataWaiters returns once n callers are waiting on the fetch for
// key, so a test can release upstream knowing every caller joined it.
func waitForMetadataWaiters(t *testing.T, p *Proxy, key string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.metaMu.Lock()
		f := p.inFlightMeta[key]
		joined := 0
		if f != nil {
			joined = f.waiters
		}
		p.metaMu.Unlock()
		if joined >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d callers to join the metadata fetch", n)
}

type metadataCall struct {
	body []byte
	err  error
}

// startMetadataCalls starts n FetchOrCacheMetadata calls for one package.
func startMetadataCalls(p *Proxy, ctx context.Context, n int, url, accept string) []chan metadataCall {
	out := make([]chan metadataCall, n)
	for i := range out {
		out[i] = make(chan metadataCall, 1)
		go func(ch chan metadataCall) {
			body, _, err := p.FetchOrCacheMetadata(ctx, "npm", "left-pad", url, accept)
			ch <- metadataCall{body, err}
		}(out[i])
	}
	return out
}

// TestFetchOrCacheMetadata_ConcurrentMissesCoalesce asserts that concurrent
// misses for one package make a single upstream request: the CI shape, where
// parallel jobs resolve the same Composer or npm dependencies at once.
func TestFetchOrCacheMetadata_ConcurrentMissesCoalesce(t *testing.T) {
	u := newMetadataUpstream(t, http.StatusOK)
	p := metadataTestProxy(t, u)
	const n = 10

	calls := startMetadataCalls(p, context.Background(), 1, u.URL, contentTypeJSON)
	<-u.entered
	calls = append(calls, startMetadataCalls(p, context.Background(), n-1, u.URL, contentTypeJSON)...)
	key := metadataCoalesceKey("npm", "left-pad", u.URL, contentTypeJSON, "", false)
	waitForMetadataWaiters(t, p, key, n-1)
	close(u.release)

	for _, ch := range calls {
		c := <-ch
		if c.err != nil {
			t.Fatalf("FetchOrCacheMetadata: %v", c.err)
		}
		if string(c.body) != `{"accept":"application/json"}` {
			t.Errorf("body = %s", c.body)
		}
	}
	if got := u.calls.Load(); got != 1 {
		t.Errorf("upstream requests = %d, want 1", got)
	}
}

// TestFetchOrCacheMetadata_DifferentAcceptDoNotShare asserts that requests
// for different variants of one package, such as npm's abbreviated and full
// documents, each get their own fetch and their own bytes.
func TestFetchOrCacheMetadata_DifferentAcceptDoNotShare(t *testing.T) {
	u := newMetadataUpstream(t, http.StatusOK)
	p := metadataTestProxy(t, u)
	const abbreviated = "application/vnd.npm.install-v1+json"

	full := startMetadataCalls(p, context.Background(), 1, u.URL, contentTypeJSON)
	<-u.entered
	abbr := startMetadataCalls(p, context.Background(), 1, u.URL, abbreviated)
	deadline := time.Now().Add(5 * time.Second)
	for u.calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(u.release)

	if c := <-full[0]; c.err != nil || string(c.body) != `{"accept":"application/json"}` {
		t.Errorf("full document: body %s, err %v", c.body, c.err)
	}
	if c := <-abbr[0]; c.err != nil || string(c.body) != `{"accept":"`+abbreviated+`"}` {
		t.Errorf("abbreviated document: body %s, err %v", c.body, c.err)
	}
	if got := u.calls.Load(); got != 2 {
		t.Errorf("upstream requests = %d, want 2", got)
	}
}

// TestFetchOrCacheMetadata_FirstCallerLeavingDoesNotFailOthers asserts that
// when the caller running the shared fetch disconnects, the fetch continues
// and everyone waiting on it still gets the metadata.
func TestFetchOrCacheMetadata_FirstCallerLeavingDoesNotFailOthers(t *testing.T) {
	u := newMetadataUpstream(t, http.StatusOK)
	p := metadataTestProxy(t, u)
	const n = 5

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leader := startMetadataCalls(p, leaderCtx, 1, u.URL, contentTypeJSON)
	<-u.entered
	waiters := startMetadataCalls(p, context.Background(), n, u.URL, contentTypeJSON)
	key := metadataCoalesceKey("npm", "left-pad", u.URL, contentTypeJSON, "", false)
	waitForMetadataWaiters(t, p, key, n)
	cancelLeader()
	close(u.release)

	<-leader[0]
	for _, ch := range waiters {
		if c := <-ch; c.err != nil {
			t.Errorf("waiter failed after the first caller left: %v", c.err)
		}
	}
	if got := u.calls.Load(); got != 1 {
		t.Errorf("upstream requests = %d, want 1", got)
	}
}

// TestFetchOrCacheMetadata_WaiterLeavingKeepsFetch asserts that a waiter whose
// client disconnects returns its own context error without disturbing the
// fetch the others are waiting on.
func TestFetchOrCacheMetadata_WaiterLeavingKeepsFetch(t *testing.T) {
	u := newMetadataUpstream(t, http.StatusOK)
	p := metadataTestProxy(t, u)

	leader := startMetadataCalls(p, context.Background(), 1, u.URL, contentTypeJSON)
	<-u.entered
	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	leaving := startMetadataCalls(p, waiterCtx, 1, u.URL, contentTypeJSON)
	key := metadataCoalesceKey("npm", "left-pad", u.URL, contentTypeJSON, "", false)
	waitForMetadataWaiters(t, p, key, 1)
	cancelWaiter()

	if c := <-leaving[0]; !errors.Is(c.err, context.Canceled) {
		t.Errorf("leaving waiter err = %v, want context.Canceled", c.err)
	}
	close(u.release)
	if c := <-leader[0]; c.err != nil {
		t.Errorf("first caller failed: %v", c.err)
	}
}

// TestFetchOrCacheMetadata_SharedNotFound asserts that an upstream 404 reaches
// every caller sharing the fetch as ErrUpstreamNotFound, from one request.
func TestFetchOrCacheMetadata_SharedNotFound(t *testing.T) {
	u := newMetadataUpstream(t, http.StatusNotFound)
	p := metadataTestProxy(t, u)
	const n = 5

	calls := startMetadataCalls(p, context.Background(), 1, u.URL, contentTypeJSON)
	<-u.entered
	calls = append(calls, startMetadataCalls(p, context.Background(), n-1, u.URL, contentTypeJSON)...)
	key := metadataCoalesceKey("npm", "left-pad", u.URL, contentTypeJSON, "", false)
	waitForMetadataWaiters(t, p, key, n-1)
	close(u.release)

	for _, ch := range calls {
		if c := <-ch; !errors.Is(c.err, ErrUpstreamNotFound) {
			t.Errorf("err = %v, want ErrUpstreamNotFound", c.err)
		}
	}
	if got := u.calls.Load(); got != 1 {
		t.Errorf("upstream requests = %d, want 1", got)
	}
}

// TestCoalescedMetadataMiss_ServesRowCommittedSinceLookup asserts that a
// caller which missed the cache, but finds the row fresh once it takes the
// key, serves that row instead of fetching again. That is a fetch finishing
// between a caller's lookup and its turn at the key.
func TestCoalescedMetadataMiss_ServesRowCommittedSinceLookup(t *testing.T) {
	u := newMetadataUpstream(t, http.StatusOK)
	close(u.release)
	p := metadataTestProxy(t, u)
	p.CacheMetadata = true
	p.MetadataTTL = time.Hour

	if _, _, err := p.FetchOrCacheMetadata(context.Background(), "npm", "left-pad", u.URL); err != nil {
		t.Fatalf("priming fetch: %v", err)
	}

	res := p.coalescedMetadataMiss(context.Background(), "npm", "left-pad", u.URL, contentTypeJSON, "", nil)
	if res.err != nil {
		t.Fatalf("coalescedMetadataMiss: %v", res.err)
	}
	if string(res.body) != `{"accept":"application/json"}` {
		t.Errorf("body = %s", res.body)
	}
	if got := u.calls.Load(); got != 1 {
		t.Errorf("upstream requests = %d, want 1 (the priming fetch only)", got)
	}
}
