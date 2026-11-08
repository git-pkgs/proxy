package gitcache

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// cachedRepo adds a repository of the given size to the cache, still in use
// when busy.
func cachedRepo(t *testing.T, tc *testCache, name string, size int64, busy bool) *repo {
	t.Helper()
	rp := tc.acquire("up/" + name + ".git")
	if err := os.MkdirAll(rp.dir, dirPermissions); err != nil {
		t.Fatal(err)
	}
	rp.size.Store(size)
	if !busy {
		tc.release(rp)
	}
	return rp
}

func (tc *testCache) budget() (reserved int64, cutPending int) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	return tc.reserved, tc.cutPending
}

func TestCoverEvictsIdleRepositories(t *testing.T) {
	tc := newTestCache(t, "", func(o *Options) { o.MaxSize = 100 })
	old := cachedRepo(t, tc, "old", 60, false)
	busy := cachedRepo(t, tc, "busy", 30, true)

	res := tc.newReservation()
	if err := res.cover(t.Context(), 20); err != nil {
		t.Fatal(err)
	}
	if fileExists(old.dir) {
		t.Error("idle repository was not evicted")
	}
	if !fileExists(busy.dir) {
		t.Error("evicted a repository in use")
	}
	if reserved, _ := tc.budget(); reserved < 20 {
		t.Errorf("reserved %d, want at least 20", reserved)
	}
	res.release()
	if reserved, _ := tc.budget(); reserved != 0 {
		t.Errorf("reserved %d after release", reserved)
	}
}

func TestCoverCutsOffOnceOnlyRepositoriesInUseAreLeft(t *testing.T) {
	tc := newTestCache(t, "", func(o *Options) { o.MaxSize = 100 })
	busy := cachedRepo(t, tc, "busy", 90, true)

	res := tc.newReservation()
	if err := res.cover(t.Context(), 20); !errors.Is(err, ErrCacheFull) {
		t.Fatalf("cover: %v, want ErrCacheFull", err)
	}
	if !fileExists(busy.dir) {
		t.Error("evicted a repository in use")
	}
	res.release()
	if reserved, cutPending := tc.budget(); reserved != 0 || cutPending != 0 {
		t.Errorf("reserved %d, cut off %d after release", reserved, cutPending)
	}
}

// TestCoverKeepsIdleRepositoriesItCannotUse cuts off a fetch that would not
// fit even with every idle repository evicted, so none is.
func TestCoverKeepsIdleRepositoriesItCannotUse(t *testing.T) {
	tc := newTestCache(t, "", func(o *Options) { o.MaxSize = 100 })
	idle := cachedRepo(t, tc, "idle", 5, false)
	cachedRepo(t, tc, "busy", 90, true)

	res := tc.newReservation()
	if err := res.cover(t.Context(), 20); !errors.Is(err, ErrCacheFull) {
		t.Fatalf("cover: %v, want ErrCacheFull", err)
	}
	if !fileExists(idle.dir) {
		t.Error("evicted an idle repository without making room")
	}
	res.release()
}

// TestCoverWaitsForCutOffFetch runs two fetches that do not fit together.
// Once one is cut off, the other waits for its space instead of failing too.
func TestCoverWaitsForCutOffFetch(t *testing.T) {
	tc := newTestCache(t, "", func(o *Options) { o.MaxSize = 100 })
	cachedRepo(t, tc, "busy", 40, true)

	first, second := tc.newReservation(), tc.newReservation()
	if err := first.cover(t.Context(), 30); err != nil {
		t.Fatal(err)
	}
	if err := second.cover(t.Context(), 20); err != nil {
		t.Fatal(err)
	}
	if err := first.cover(t.Context(), 40); !errors.Is(err, ErrCacheFull) {
		t.Fatalf("first: %v, want ErrCacheFull", err)
	}

	done := make(chan error, 1)
	go func() { done <- second.cover(context.Background(), 30) }()
	select {
	case err := <-done:
		t.Fatalf("second did not wait for the first to be released: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	first.release()
	if err := <-done; err != nil {
		t.Fatalf("second: %v", err)
	}
	second.release()
	if reserved, cutPending := tc.budget(); reserved != 0 || cutPending != 0 {
		t.Errorf("reserved %d, cut off %d after release", reserved, cutPending)
	}
}

// TestConcurrentClonesStayWithinMaxSize clones two repositories at once that
// do not fit in MaxSize together. Both are kept in use, so neither can be
// evicted: one clone succeeds, and the other is cut off without being
// marked too large.
func TestConcurrentClonesStayWithinMaxSize(t *testing.T) {
	upstream := newFakeUpstream(t)
	for _, name := range []string{"a", "b"} {
		work := upstream.newRepo(name)
		big := make([]byte, 256<<10)
		_, _ = rand.Read(big)
		if err := os.WriteFile(filepath.Join(work, "big.bin"), big, 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, work, "add", "big.bin")
		runGit(t, work, "commit", "--quiet", "-m", "big")
		runGit(t, work, "push", "--quiet", "origin", "main")
	}
	size := repoSize(t, upstream, "a")
	maxSize := size * 5 / 3
	tc := newTestCache(t, upstream.server.URL, func(o *Options) { o.MaxSize = maxSize })
	for _, name := range []string{"a", "b"} {
		defer tc.release(tc.acquire("up/" + name + ".git"))
	}

	// Sample what the cache counts while the clones run.
	var peak int64
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			tc.mu.Lock()
			total := tc.reserved
			for _, rp := range tc.repos {
				total += rp.size.Load()
			}
			tc.mu.Unlock()
			peak = max(peak, total)
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Microsecond):
			}
		}
	}()

	// Held back, both fetches are under way before either receives a pack.
	upstream.delayFetches.Store(int64(200 * time.Millisecond))
	statuses := make(map[string]int)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, name := range []string{"a", "b"} {
		wg.Go(func() {
			resp, err := http.Get(tc.url(name) + "/info/refs?service=git-upload-pack")
			if err != nil {
				t.Error(err)
				return
			}
			_ = resp.Body.Close()
			mu.Lock()
			statuses[name] = resp.StatusCode
			mu.Unlock()
		})
	}
	wg.Wait()
	close(stop)
	<-sampled

	if (statuses["a"] == http.StatusOK) == (statuses["b"] == http.StatusOK) {
		t.Fatalf("statuses %v, want one 200 and one 507", statuses)
	}
	for name, status := range statuses {
		if status != http.StatusOK && status != http.StatusInsufficientStorage {
			t.Errorf("%s: status %d, want 507", name, status)
		}
		if tc.isTooLarge("up/" + name + ".git") {
			t.Errorf("%s was marked too large", name)
		}
	}
	// What a clone writes beyond the bytes it receives, such as its index,
	// is counted only once it is measured.
	if slack := size / 4; peak > maxSize+slack {
		t.Errorf("cache counted %d, past max_size %d", peak, maxSize)
	}
	if reserved, cutPending := tc.budget(); reserved != 0 || cutPending != 0 {
		t.Errorf("reserved %d, cut off %d once the clones ended", reserved, cutPending)
	}
	if entries, _ := os.ReadDir(tc.tmpDir()); len(entries) != 0 {
		t.Errorf("left %d entries in the temporary directory", len(entries))
	}
}

// TestCoverAfterReleaseReservesNothing has a relayed read arrive after the
// fetch gave back its reservation, as one from an orphaned git helper would.
func TestCoverAfterReleaseReservesNothing(t *testing.T) {
	tc := newTestCache(t, "", func(o *Options) { o.MaxSize = 100 })
	res := tc.newReservation()
	if err := res.cover(t.Context(), 10); err != nil {
		t.Fatal(err)
	}
	res.release()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_ = res.cover(ctx, 20)
	if reserved, _ := tc.budget(); reserved != 0 {
		t.Errorf("reserved %d after release", reserved)
	}
}

// TestCancelledFetchStopsRelaying cancels a fetch while the upstream is
// midway through sending the pack: the upstream connection is closed, and
// nothing stays reserved when the rest arrives.
func TestCancelledFetchStopsRelaying(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.newRepo("repo")
	upstreamURL, err := url.Parse(upstream.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	arrived := make(chan struct{})
	release := make(chan struct{})
	result := make(chan string, 1)
	forward := httputil.NewSingleHostReverseProxy(upstreamURL)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		if r.Method != http.MethodPost || !bytes.Contains(body, []byte("command=fetch")) {
			forward.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		forward.ServeHTTP(rec, r.Clone(context.Background()))
		pack := rec.Body.Bytes()
		w.Header().Set("Content-Type", rec.Header().Get("Content-Type"))
		_, _ = w.Write(pack[:len(pack)/2])
		w.(http.Flusher).Flush()
		close(arrived)
		select {
		case <-r.Context().Done():
			result <- "closed"
		case <-release:
			_, _ = w.Write(pack[len(pack)/2:])
			w.(http.Flusher).Flush()
			result <- "sent"
		}
	}))
	defer front.Close()
	defer close(release)
	tc := newTestCache(t, front.URL, func(o *Options) { o.MaxSize = 1 << 30 })

	dir := t.TempDir()
	if _, err := tc.run(t.Context(), dir, "init", "--quiet", "--bare"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	res := tc.newReservation()
	done := make(chan error, 1)
	go func() { done <- tc.fetch(ctx, dir, front.URL+"/repo", 0, res, nil, branchesAndTags...) }()
	<-arrived
	waitFor(t, func() bool { reserved, _ := tc.budget(); return reserved > 0 })
	cancel()
	if err := <-done; err == nil {
		t.Error("cancelled fetch succeeded")
	}
	res.release()

	select {
	case <-result:
	case <-time.After(10 * time.Second):
		t.Error("upstream connection still open after the fetch was cancelled")
		release <- struct{}{}
		<-result
	}
	// Give a relay still running the time to read what the upstream sent.
	time.Sleep(200 * time.Millisecond)
	if reserved, _ := tc.budget(); reserved != 0 {
		t.Errorf("reserved %d after the fetch ended", reserved)
	}
}
