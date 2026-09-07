package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/git-pkgs/registries/fetch"
)

// runConcurrent runs fn in n goroutines released together and returns their errors.
func runConcurrent(n int, fn func(i int) error) []error {
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = fn(i)
		}(i)
	}
	close(start)
	wg.Wait()
	return errs
}

// artifactBody builds a one-shot upstream artifact carrying the given bytes.
func artifactBody(content string) *fetch.Artifact {
	return &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader(content)),
		ContentType: "application/gzip",
	}
}

// drain consumes and closes a CacheResult reader, if there is one.
func drain(res *CacheResult) {
	if res != nil && res.Reader != nil {
		_, _ = io.Copy(io.Discard, res.Reader)
		_ = res.Reader.Close()
	}
}

// TestCoalesceKey_DifferentUpstreamHashDoesNotShare is the safety property that
// makes coalescing sound: callers expecting different bytes must never share a
// fetch, so a re-published version cannot serve stale bytes to a caller that
// asked for the new digest.
func TestCoalesceKey_DifferentUpstreamHashDoesNotShare(t *testing.T) {
	const content = "artifact bytes"
	proxy, _, _, _ := setupTestProxy(t)
	fetcher := &countingFetcher{content: content, delay: fetchHoldTime}
	proxy.Fetcher = fetcher

	// The digest must carry the "sha256:" prefix; without it the API treats the
	// value as unverifiable and clears the hash, which would legitimately let
	// the two callers share one fetch.
	hashes := []string{
		"sha256:" + sha256Hex(content),
		"sha256:" + sha256Hex("something else entirely"),
	}

	_ = runConcurrent(2, func(i int) error {
		res, err := proxy.GetOrFetchArtifactFromURLWithDigest(context.Background(),
			"npm", "pkg", "1.0.0", "pkg-1.0.0.tgz",
			"https://registry.npmjs.org/pkg/-/pkg-1.0.0.tgz", hashes[i])
		drain(res)
		return err
	})

	if got := fetcher.calls.Load(); got != 2 {
		t.Errorf("upstream fetches = %d, want 2: callers expecting different digests must not share a fetch", got)
	}
}

// TestCoalesceKey_DifferentDownloadURLDoesNotShare covers the other half of the
// key: same package, different upstream URL, must not collapse into one fetch.
func TestCoalesceKey_DifferentDownloadURLDoesNotShare(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	fetcher := &countingFetcher{content: "artifact bytes", delay: fetchHoldTime}
	proxy.Fetcher = fetcher

	urls := []string{
		"https://registry.npmjs.org/pkg/-/pkg-1.0.0.tgz",
		"https://mirror.example.com/pkg/-/pkg-1.0.0.tgz",
	}

	_ = runConcurrent(2, func(i int) error {
		res, err := proxy.GetOrFetchArtifactFromURL(context.Background(),
			"npm", "pkg", "1.0.0", "pkg-1.0.0.tgz", urls[i])
		drain(res)
		return err
	})

	if got := fetcher.calls.Load(); got != 2 {
		t.Errorf("upstream fetches = %d, want 2: different upstream URLs must not share a fetch", got)
	}
}

// TestCoalesceKey_DistinctArtifactsDoNotSerialize guards against an over-broad
// key: four packages fetched at once must still produce four fetches.
func TestCoalesceKey_DistinctArtifactsDoNotSerialize(t *testing.T) {
	const n = 4
	proxy, _, _, _ := setupTestProxy(t)
	fetcher := &countingFetcher{content: "artifact bytes", delay: fetchHoldTime}
	proxy.Fetcher = fetcher

	names := []string{"alpha", "beta", "gamma", "delta"}
	errs := runConcurrent(n, func(i int) error {
		res, err := proxy.GetOrFetchArtifactFromURL(context.Background(),
			"npm", names[i], "1.0.0", names[i]+"-1.0.0.tgz",
			"https://registry.npmjs.org/"+names[i]+"/-/"+names[i]+"-1.0.0.tgz")
		drain(res)
		return err
	})
	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d (%s): %v", i, names[i], err)
		}
	}
	if got := fetcher.calls.Load(); got != n {
		t.Errorf("upstream fetches = %d, want %d: distinct artifacts must not share a fetch", got, n)
	}
}

// TestCoalesce_FailedFetchReachesEveryCallerAndIsRetriable verifies both claims
// in coalesceFetch's doc comment: a failed fetch reaches every caller sharing
// it, and the key is released so a later request retries.
func TestCoalesce_FailedFetchReachesEveryCallerAndIsRetriable(t *testing.T) {
	const callers = 8
	proxy, _, _, fetcher := setupTestProxy(t)
	boom := errors.New("upstream unavailable")
	fetcher.fetchErr = boom

	errs := runConcurrent(callers, func(int) error {
		res, err := proxy.GetOrFetchArtifactFromURL(context.Background(),
			"npm", "pkg", "1.0.0", "pkg-1.0.0.tgz",
			"https://registry.npmjs.org/pkg/-/pkg-1.0.0.tgz")
		drain(res)
		return err
	})
	for i, err := range errs {
		if err == nil {
			t.Errorf("caller %d: got nil error, want the shared fetch's failure", i)
		} else if !errors.Is(err, boom) {
			t.Errorf("caller %d: got %v, want it to wrap %v", i, err, boom)
		}
	}

	// The key must be released: a later request retries rather than inheriting
	// the failure.
	fetcher.fetchErr = nil
	fetcher.artifact = artifactBody("recovered bytes")
	res, err := proxy.GetOrFetchArtifactFromURL(context.Background(),
		"npm", "pkg", "1.0.0", "pkg-1.0.0.tgz",
		"https://registry.npmjs.org/pkg/-/pkg-1.0.0.tgz")
	if err != nil {
		t.Fatalf("retry after failed coalesced fetch: %v", err)
	}
	body, _ := io.ReadAll(res.Reader)
	_ = res.Reader.Close()
	if string(body) != "recovered bytes" {
		t.Errorf("retry body = %q, want %q", body, "recovered bytes")
	}
}

// TestCoalesce_ResolverPath covers the other entry point: GetOrFetchArtifact
// resolves the URL itself, so it is keyed without one.
func TestCoalesce_ResolverPath(t *testing.T) {
	const callers = 8
	proxy, _, _, _ := setupTestProxy(t)
	fetcher := &countingFetcher{content: "resolved artifact bytes", delay: fetchHoldTime}
	proxy.Fetcher = fetcher

	errs := runConcurrent(callers, func(int) error {
		res, err := proxy.GetOrFetchArtifact(context.Background(),
			"npm", "left-pad", "1.3.0", "left-pad-1.3.0.tgz")
		drain(res)
		return err
	})
	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: %v", i, err)
		}
	}
	if got := fetcher.calls.Load(); got != 1 {
		t.Errorf("upstream fetches = %d, want 1", got)
	}
}

// TestCoalesce_ResolverPathEmptyFilename exercises that path when the filename
// is left to be resolved, which the key cannot know up front.
func TestCoalesce_ResolverPathEmptyFilename(t *testing.T) {
	const callers = 8
	proxy, _, _, _ := setupTestProxy(t)
	fetcher := &countingFetcher{content: "resolved artifact bytes", delay: fetchHoldTime}
	proxy.Fetcher = fetcher

	errs := runConcurrent(callers, func(int) error {
		res, err := proxy.GetOrFetchArtifact(context.Background(), "npm", "left-pad", "1.3.0", "")
		drain(res)
		return err
	})
	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: %v", i, err)
		}
	}
	if got := fetcher.calls.Load(); got != 1 {
		t.Errorf("upstream fetches = %d, want 1", got)
	}
}

// TestCoalesce_SubsequentRequestIsACacheHit confirms the coalesced fetch was
// committed and is visible later, not just streamed to the waiting callers.
func TestCoalesce_SubsequentRequestIsACacheHit(t *testing.T) {
	const callers = 8
	const url = "https://registry.npmjs.org/pkg/-/pkg-1.0.0.tgz"
	proxy, _, _, _ := setupTestProxy(t)
	fetcher := &countingFetcher{content: "artifact bytes", delay: fetchHoldTime}
	proxy.Fetcher = fetcher

	_ = runConcurrent(callers, func(int) error {
		res, err := proxy.GetOrFetchArtifactFromURL(context.Background(),
			"npm", "pkg", "1.0.0", "pkg-1.0.0.tgz", url)
		drain(res)
		return err
	})

	res, err := proxy.GetOrFetchArtifactFromURL(context.Background(),
		"npm", "pkg", "1.0.0", "pkg-1.0.0.tgz", url)
	if err != nil {
		t.Fatalf("follow-up request: %v", err)
	}
	defer func() { _ = res.Reader.Close() }()
	if !res.Cached {
		t.Error("follow-up request should be served from cache")
	}
	if got := fetcher.calls.Load(); got != 1 {
		t.Errorf("upstream fetches = %d, want 1 after a follow-up cache hit", got)
	}
}

// TestCoalesce_ReadersAreIndependent guards openStoredArtifact: callers sharing
// a fetch each need their own reader, or one closing early breaks the rest.
func TestCoalesce_ReadersAreIndependent(t *testing.T) {
	const callers = 8
	const content = "artifact bytes that every caller must receive intact"
	proxy, _, _, _ := setupTestProxy(t)
	fetcher := &countingFetcher{content: content, delay: fetchHoldTime}
	proxy.Fetcher = fetcher

	results := make([]*CacheResult, callers)
	errs := runConcurrent(callers, func(i int) error {
		res, err := proxy.GetOrFetchArtifactFromURL(context.Background(),
			"npm", "pkg", "1.0.0", "pkg-1.0.0.tgz",
			"https://registry.npmjs.org/pkg/-/pkg-1.0.0.tgz")
		results[i] = res
		return err
	})
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}

	// Close the first caller's reader before anyone else has read a byte.
	_ = results[0].Reader.Close()

	for i := 1; i < callers; i++ {
		body, err := io.ReadAll(results[i].Reader)
		_ = results[i].Reader.Close()
		if err != nil {
			t.Errorf("caller %d read after another caller closed: %v", i, err)
			continue
		}
		if string(body) != content {
			t.Errorf("caller %d got %q, want %q", i, body, content)
		}
	}
}

// TestCoalesce_CanceledWaiterDoesNotWaitForTheSharedFetch checks that joining a
// coalesced fetch does not cost a caller its own cancellation. Without the
// leader/waiter split a waiter is pinned until the shared fetch resolves,
// bounded only by the artifact client timeout, so clients that have already
// gone away keep handler goroutines alive for minutes.
func TestCoalesce_CanceledWaiterDoesNotWaitForTheSharedFetch(t *testing.T) {
	const leaderFetch = 2 * time.Second
	const url = "https://registry.npmjs.org/pkg/-/pkg-1.0.0.tgz"

	proxy, _, _, _ := setupTestProxy(t)
	fetcher := &countingFetcher{content: "artifact bytes", delay: leaderFetch}
	proxy.Fetcher = fetcher

	leaderDone := make(chan error, 1)
	go func() {
		res, err := proxy.GetOrFetchArtifactFromURL(context.Background(),
			"npm", "pkg", "1.0.0", "pkg-1.0.0.tgz", url)
		drain(res)
		leaderDone <- err
	}()

	time.Sleep(200 * time.Millisecond) // let the leader take the key
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	_, err := proxy.GetOrFetchArtifactFromURL(ctx, "npm", "pkg", "1.0.0", "pkg-1.0.0.tgz", url)
	blocked := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("waiter error = %v, want context.Canceled", err)
	}
	if blocked > leaderFetch/4 {
		t.Errorf("canceled waiter blocked %v, want well under %v: it is pinned to the shared fetch",
			blocked, leaderFetch/4)
	}

	// A waiter leaving must not disturb the fetch the others share.
	if err := <-leaderDone; err != nil {
		t.Fatalf("leader failed after a waiter canceled: %v", err)
	}
	res, err := proxy.GetOrFetchArtifactFromURL(context.Background(),
		"npm", "pkg", "1.0.0", "pkg-1.0.0.tgz", url)
	if err != nil {
		t.Fatalf("follow-up after leader completed: %v", err)
	}
	defer func() { _ = res.Reader.Close() }()
	if !res.Cached {
		t.Error("leader's fetch should have been committed to the cache")
	}
	if got := fetcher.calls.Load(); got != 1 {
		t.Errorf("upstream fetches = %d, want 1", got)
	}
}

// inFlightLen reports how many coalesced fetches are currently registered.
func inFlightLen(p *Proxy) int {
	p.fetchMu.Lock()
	defer p.fetchMu.Unlock()
	return len(p.inFlight)
}

// TestCoalesce_KeyIsReleasedAfterFetch guards the bug this hand-rolled map can
// have that singleflight could not: a key left behind means later callers join
// a finished entry, see its closed done channel, and are served that stale
// result forever, while the map grows without bound.
func TestCoalesce_KeyIsReleasedAfterFetch(t *testing.T) {
	const url = "https://registry.npmjs.org/pkg/-/pkg-1.0.0.tgz"
	proxy, _, _, _ := setupTestProxy(t)
	fetcher := &countingFetcher{content: "artifact bytes", delay: fetchHoldTime}
	proxy.Fetcher = fetcher

	_ = runConcurrent(8, func(int) error {
		res, err := proxy.GetOrFetchArtifactFromURL(context.Background(),
			"npm", "pkg", "1.0.0", "pkg-1.0.0.tgz", url)
		drain(res)
		return err
	})
	if n := inFlightLen(proxy); n != 0 {
		t.Errorf("in-flight entries after a successful fetch = %d, want 0", n)
	}

	// A fresh miss for the same key must start a new fetch, not rejoin the old
	// entry. Clearing the cache record forces the miss path again.
	if err := proxy.ClearCachedArtifact(context.Background(), "npm", "pkg", "1.0.0", "pkg-1.0.0.tgz"); err != nil {
		t.Fatalf("clear cached artifact: %v", err)
	}
	res, err := proxy.GetOrFetchArtifactFromURL(context.Background(),
		"npm", "pkg", "1.0.0", "pkg-1.0.0.tgz", url)
	if err != nil {
		t.Fatalf("second miss for the same key: %v", err)
	}
	drain(res)
	if got := fetcher.calls.Load(); got != 2 {
		t.Errorf("upstream fetches = %d, want 2: the second miss must not reuse the finished entry", got)
	}
	if n := inFlightLen(proxy); n != 0 {
		t.Errorf("in-flight entries at end = %d, want 0", n)
	}
}

// panickingFetcher blows up mid-fetch, after waiters have had time to join.
type panickingFetcher struct{ countingFetcher }

func (f *panickingFetcher) Fetch(ctx context.Context, url string) (*fetch.Artifact, error) {
	return f.FetchWithHeaders(ctx, url, nil)
}

func (f *panickingFetcher) FetchWithHeaders(context.Context, string, http.Header) (*fetch.Artifact, error) {
	f.calls.Add(1)
	time.Sleep(fetchHoldTime)
	panic("upstream fetch exploded")
}

// TestCoalesce_PanicInSharedFetchDoesNotStrandWaiters checks the failure mode
// that matters most: a waiter must never be left blocked forever on a fetch
// that died.
func TestCoalesce_PanicInSharedFetchDoesNotStrandWaiters(t *testing.T) {
	const url = "https://registry.npmjs.org/pkg/-/pkg-1.0.0.tgz"
	proxy, _, _, _ := setupTestProxy(t)
	proxy.Fetcher = &panickingFetcher{}

	leaderPanicked := make(chan struct{})
	go func() {
		defer func() {
			_ = recover() // the panic surfaces in the leader, as it would in a handler
			close(leaderPanicked)
		}()
		res, _ := proxy.GetOrFetchArtifactFromURL(context.Background(),
			"npm", "pkg", "1.0.0", "pkg-1.0.0.tgz", url)
		drain(res)
	}()

	time.Sleep(fetchHoldTime / 2) // join while the doomed fetch is still running
	done := make(chan error, 1)
	go func() {
		res, err := proxy.GetOrFetchArtifactFromURL(context.Background(),
			"npm", "pkg", "1.0.0", "pkg-1.0.0.tgz", url)
		drain(res)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, errSharedFetchAbandoned) {
			t.Errorf("waiter error = %v, want errSharedFetchAbandoned", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter stranded: a panicking shared fetch never released its waiters")
	}
	<-leaderPanicked
	if n := inFlightLen(proxy); n != 0 {
		t.Errorf("in-flight entries after a panic = %d, want 0", n)
	}
}
