package handler

import (
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"
)

// These tests refetch an artifact whose stored file is gone while
// pending_deletes refuses writes. The record can't move to the new file and
// the file can't be queued, so the fetch must serve it to every caller sharing
// it and delete it once the last of them is done.

const (
	unrecordedContent = "refetched content"
	unrecordedURL     = "https://example.com/gone.tgz"
)

func setupUnrecorded(t *testing.T) (*Proxy, *mockStorage) {
	t.Helper()
	proxy, db, store, _ := setupTestProxy(t)
	seedPackage(t, db, store, "npm", "gone", "1.0.0", "gone-1.0.0.tgz", "old content")
	oldPath := recordedStoragePath(t, db, "pkg:npm/gone@1.0.0", "gone-1.0.0.tgz")
	delete(store.files, oldPath)
	if _, err := db.Exec(`CREATE TRIGGER refuse_queue BEFORE INSERT ON pending_deletes
		BEGIN SELECT RAISE(ABORT, 'queue refused'); END`); err != nil {
		t.Fatalf("creating trigger: %v", err)
	}
	t.Cleanup(func() {
		if got := recordedStoragePath(t, db, "pkg:npm/gone@1.0.0", "gone-1.0.0.tgz"); got != oldPath {
			t.Errorf("record points at %q, want it kept at %q", got, oldPath)
		}
	})
	return proxy, store
}

func fetchGone(ctx context.Context, proxy *Proxy) (*CacheResult, error) {
	return proxy.GetOrFetchArtifactFromURL(ctx, "npm", "gone", "1.0.0", "gone-1.0.0.tgz", unrecordedURL)
}

func storedFiles(store *mockStorage) []string {
	store.mu.Lock()
	defer store.mu.Unlock()
	return slices.Collect(maps.Keys(store.files))
}

func readAndClose(t *testing.T, res *CacheResult) {
	t.Helper()
	body, err := io.ReadAll(res.Reader)
	_ = res.Reader.Close()
	if err != nil || string(body) != unrecordedContent {
		t.Errorf("body = %q, %v, want %q", body, err, unrecordedContent)
	}
}

// waitForWaiters blocks until n callers wait on the one fetch in flight.
func waitForWaiters(t *testing.T, proxy *Proxy, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		proxy.fetchMu.Lock()
		got := -1
		for _, f := range proxy.inFlight {
			got = f.waiters
		}
		proxy.fetchMu.Unlock()
		if got == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("never saw %d waiters on the fetch", n)
}

func TestUnrecordedRefetchIsDeletedOnceServed(t *testing.T) {
	proxy, store := setupUnrecorded(t)
	fetcher := &countingFetcher{content: unrecordedContent}
	proxy.Fetcher = fetcher

	for range 2 {
		res, err := fetchGone(context.Background(), proxy)
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if got := storedFiles(store); len(got) != 1 {
			t.Errorf("stored files while serving = %v, want the refetched one", got)
		}
		readAndClose(t, res)
		if got := storedFiles(store); len(got) != 0 {
			t.Errorf("stored files after serving = %v, want none left untracked", got)
		}
	}
}

func TestUnrecordedRefetchServesRangeRequests(t *testing.T) {
	proxy, store := setupUnrecorded(t)
	store.seekable = true
	proxy.Fetcher = &countingFetcher{content: unrecordedContent}

	res, err := fetchGone(context.Background(), proxy)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/gone-1.0.0.tgz", nil)
	req.Header.Set("Range", "bytes=2-5")
	rec := httptest.NewRecorder()
	ServeArtifactRequest(rec, req, res)

	if rec.Code != http.StatusPartialContent || rec.Body.String() != unrecordedContent[2:6] {
		t.Errorf("range response = %d %q, want 206 %q", rec.Code, rec.Body.String(), unrecordedContent[2:6])
	}
	if got := storedFiles(store); len(got) != 0 {
		t.Errorf("stored files after serving = %v, want none left untracked", got)
	}
}

// TestUnrecordedRefetchWaitsForEveryCaller keeps the object until the caller
// that ran the fetch and every caller that waited on it have closed.
func TestUnrecordedRefetchWaitsForEveryCaller(t *testing.T) {
	const waiters = 4
	proxy, store := setupUnrecorded(t)
	fetcher := newGatedFetcher(unrecordedContent)
	proxy.Fetcher = fetcher

	leader := make(chan *CacheResult, 1)
	go func() {
		res, err := fetchGone(context.Background(), proxy)
		if err != nil {
			t.Errorf("leader: %v", err)
		}
		leader <- res
	}()
	<-fetcher.entered

	results := make(chan *CacheResult, waiters)
	var wg sync.WaitGroup
	for range waiters {
		wg.Go(func() {
			res, err := fetchGone(context.Background(), proxy)
			if err != nil {
				t.Errorf("waiter: %v", err)
				return
			}
			results <- res
		})
	}
	waitForWaiters(t, proxy, waiters)
	close(fetcher.release)

	// The leader holds its reader open while every waiter reads and closes.
	lead := <-leader
	wg.Wait()
	close(results)
	for res := range results {
		readAndClose(t, res)
	}
	if got := storedFiles(store); len(got) != 1 {
		t.Fatalf("stored files with the leader still reading = %v, want the refetched one", got)
	}
	readAndClose(t, lead)
	if got := storedFiles(store); len(got) != 0 {
		t.Errorf("stored files after every caller closed = %v, want none", got)
	}
}

func TestUnrecordedRefetchSurvivesDoubleClose(t *testing.T) {
	proxy, store := setupUnrecorded(t)
	fetcher := newGatedFetcher(unrecordedContent)
	proxy.Fetcher = fetcher

	leader := make(chan *CacheResult, 1)
	go func() {
		res, err := fetchGone(context.Background(), proxy)
		if err != nil {
			t.Errorf("leader: %v", err)
		}
		leader <- res
	}()
	<-fetcher.entered

	waiter := make(chan *CacheResult, 1)
	go func() {
		res, err := fetchGone(context.Background(), proxy)
		if err != nil {
			t.Errorf("waiter: %v", err)
		}
		waiter <- res
	}()
	waitForWaiters(t, proxy, 1)
	close(fetcher.release)

	lead, wait := <-leader, <-waiter
	readAndClose(t, lead)
	_ = lead.Reader.Close()
	if got := storedFiles(store); len(got) != 1 {
		t.Fatalf("stored files with the waiter still reading = %v, want the refetched one", got)
	}
	readAndClose(t, wait)
	if got := storedFiles(store); len(got) != 0 {
		t.Errorf("stored files after every caller closed = %v, want none", got)
	}
}

// ctxStorage fails to open under a cancelled context, as cloud backends do.
type ctxStorage struct{ *mockStorage }

func (s ctxStorage) Open(ctx context.Context, path string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.mockStorage.Open(ctx, path)
}

// TestUnrecordedRefetchOutlivesDetachedCaller is the retry a detached fetch
// exists for: the client that started it gave up, and its retry waits on the
// same fetch. The retry must get the bytes rather than a deleted object.
func TestUnrecordedRefetchOutlivesDetachedCaller(t *testing.T) {
	proxy, store := setupUnrecorded(t)
	proxy.Storage = ctxStorage{store}
	fetcher := newGatedFetcher(unrecordedContent)
	proxy.Fetcher = fetcher

	ctx, cancel := context.WithCancel(DetachUpstreamFetch(context.Background()))
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		res, _ := fetchGone(ctx, proxy)
		drain(res)
	}()
	<-fetcher.entered
	cancel()

	retry := make(chan *CacheResult, 1)
	go func() {
		res, err := fetchGone(context.Background(), proxy)
		if err != nil {
			t.Errorf("retry: %v", err)
		}
		retry <- res
	}()
	waitForWaiters(t, proxy, 1)
	close(fetcher.release)
	<-leaderDone

	if res := <-retry; res != nil {
		readAndClose(t, res)
	}
	if got := storedFiles(store); len(got) != 0 {
		t.Errorf("stored files after the retry closed = %v, want none", got)
	}
}

// TestUnrecordedRefetchIgnoresWaiterThatLeft checks that a waiter giving up
// before the fetch ends does not keep the object forever.
func TestUnrecordedRefetchIgnoresWaiterThatLeft(t *testing.T) {
	proxy, store := setupUnrecorded(t)
	fetcher := newGatedFetcher(unrecordedContent)
	proxy.Fetcher = fetcher

	leader := make(chan *CacheResult, 1)
	go func() {
		res, err := fetchGone(context.Background(), proxy)
		if err != nil {
			t.Errorf("leader: %v", err)
		}
		leader <- res
	}()
	<-fetcher.entered

	ctx, cancel := context.WithCancel(context.Background())
	left := make(chan error, 1)
	go func() {
		_, err := fetchGone(ctx, proxy)
		left <- err
	}()
	waitForWaiters(t, proxy, 1)
	cancel()
	if err := <-left; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter that left: err = %v, want context.Canceled", err)
	}
	close(fetcher.release)

	readAndClose(t, <-leader)
	if got := storedFiles(store); len(got) != 0 {
		t.Errorf("stored files after the leader closed = %v, want none", got)
	}
}

// TestLeaveFetch covers both ways a waiter can give up: before the fetch
// finished, when it was not handed a hold yet, and after, when it was.
func TestLeaveFetch(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	deleted := false
	newFetch := func() *inflightFetch {
		f := &inflightFetch{done: make(chan struct{}), waiters: 1}
		f.unrecorded = &unrecordedObject{delete: func() { deleted = true }}
		f.unrecorded.refs.Add(1)
		return f
	}

	f := newFetch()
	proxy.leaveFetch(f)
	if f.waiters != 0 || deleted {
		t.Errorf("before done: waiters = %d, deleted = %v, want 0 and false", f.waiters, deleted)
	}

	f = newFetch()
	close(f.done)
	proxy.leaveFetch(f)
	if f.waiters != 1 || !deleted {
		t.Errorf("after done: waiters = %d, deleted = %v, want 1 and true", f.waiters, deleted)
	}
}

// TestRecordedAt is the check that keeps an object whose record update
// committed despite reporting an error.
func TestRecordedAt(t *testing.T) {
	proxy, db, store, _ := setupTestProxy(t)
	seedPackage(t, db, store, "npm", "kept", "1.0.0", "kept-1.0.0.tgz", "content")
	path := recordedStoragePath(t, db, "pkg:npm/kept@1.0.0", "kept-1.0.0.tgz")

	if !proxy.recordedAt("pkg:npm/kept@1.0.0", "kept-1.0.0.tgz", path) {
		t.Error("record pointing at the path not seen")
	}
	if proxy.recordedAt("pkg:npm/kept@1.0.0", "kept-1.0.0.tgz", "other/path.tgz") {
		t.Error("record pointing elsewhere taken as recorded")
	}
	if proxy.recordedAt("pkg:npm/none@1.0.0", "none-1.0.0.tgz", path) {
		t.Error("missing record taken as recorded")
	}
	_ = db.Close()
	if proxy.recordedAt("pkg:npm/kept@1.0.0", "kept-1.0.0.tgz", path) {
		t.Error("failed lookup taken as recorded")
	}
}

// TestUnrecordedStreamedMiss streams an unrecorded refetch to the caller
// that started it, and the waiters still get the stored object. A caller fed
// from upstream holds no reader on it, so it is deleted once the waiters are
// done; one that fell behind reads it, so it is deleted once that caller is
// done too.
func TestUnrecordedStreamedMiss(t *testing.T) {
	for _, behind := range []bool{false, true} {
		t.Run(map[bool]string{false: "fed", true: "behind"}[behind], func(t *testing.T) {
			testUnrecordedStreamedMiss(t, behind)
		})
	}
}

func testUnrecordedStreamedMiss(t *testing.T, behind bool) {
	t.Helper()
	if behind {
		old := clientFeedLimit
		clientFeedLimit = 4
		t.Cleanup(func() { clientFeedLimit = old })
	}
	proxy, store := setupUnrecorded(t)
	fetcher := newGatedFetcher(unrecordedContent)
	proxy.Fetcher = fetcher

	leader := make(chan *CacheResult, 1)
	go func() {
		res, err := fetchGone(StreamCacheMiss(context.Background()), proxy)
		if err != nil {
			t.Errorf("leader: %v", err)
		}
		leader <- res
	}()
	<-fetcher.entered
	waiter := make(chan *CacheResult, 1)
	go func() {
		res, err := fetchGone(context.Background(), proxy)
		if err != nil {
			t.Errorf("waiter: %v", err)
		}
		waiter <- res
	}()
	waitForWaiters(t, proxy, 1)
	close(fetcher.release)

	lead := <-leader
	if lead == nil || lead.Artifact.Size != -1 {
		t.Fatalf("leader = %+v, want a stream", lead)
	}
	w := <-waiter
	if w == nil {
		t.FailNow()
	}
	readAndClose(t, w)
	if behind {
		// The leader reads the stored copy, so it must outlive the
		// waiter.
		if len(storedFiles(store)) == 0 {
			t.Fatal("stored copy deleted while the leader still reads it")
		}
		readAndClose(t, lead)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(storedFiles(store)) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := storedFiles(store); len(got) != 0 {
		t.Errorf("stored files after every reader closed = %v, want none", got)
	}
	if !behind {
		readAndClose(t, lead)
	}
}
