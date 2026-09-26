package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/registries/fetch"
)

// These tests cover fetches of one artifact that do not share a coalescing
// key because their URLs differ. Each must keep its own stored object.

const (
	fetchPathURLA = "https://mirror-a.example/pkg-1.0.0.tgz"
	fetchPathURLB = "https://mirror-b.example/pkg-1.0.0.tgz"
)

// barrierFetcher serves a body per URL and holds each fetch until both have
// started, so both requests are past the cache check before either commits.
type barrierFetcher struct {
	bodies  map[string]string
	arrived sync.WaitGroup
}

func newBarrierFetcher(bodies map[string]string) *barrierFetcher {
	f := &barrierFetcher{bodies: bodies}
	f.arrived.Add(len(bodies))
	return f
}

func (f *barrierFetcher) Fetch(ctx context.Context, url string) (*fetch.Artifact, error) {
	return f.FetchWithHeaders(ctx, url, nil)
}

func (f *barrierFetcher) FetchWithHeaders(_ context.Context, url string, _ http.Header) (*fetch.Artifact, error) {
	f.arrived.Done()
	f.arrived.Wait()
	return artifactBody(f.bodies[url]), nil
}

func (f *barrierFetcher) Head(_ context.Context, _ string) (int64, string, error) {
	return 0, "", nil
}

// gatedStorage holds the first Open until ready reports true, to order it
// after the other request's store or delete.
type gatedStorage struct {
	*mockStorage
	ready     func(stores, deletes int32) bool
	stores    atomic.Int32
	deletes   atomic.Int32
	opened    atomic.Bool
	release   chan struct{}
	releaseMu sync.Once
}

func newGatedStorage(store *mockStorage, ready func(stores, deletes int32) bool) *gatedStorage {
	return &gatedStorage{mockStorage: store, ready: ready, release: make(chan struct{})}
}

func (g *gatedStorage) check() {
	if g.ready(g.stores.Load(), g.deletes.Load()) {
		g.releaseMu.Do(func() { close(g.release) })
	}
}

func (g *gatedStorage) Store(ctx context.Context, path string, r io.Reader) (int64, string, error) {
	size, hash, err := g.mockStorage.Store(ctx, path, r)
	g.stores.Add(1)
	g.check()
	return size, hash, err
}

func (g *gatedStorage) Delete(ctx context.Context, path string) error {
	err := g.mockStorage.Delete(ctx, path)
	g.deletes.Add(1)
	g.check()
	return err
}

func (g *gatedStorage) Open(ctx context.Context, path string) (io.ReadCloser, error) {
	if g.opened.CompareAndSwap(false, true) {
		select {
		case <-g.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return g.mockStorage.Open(ctx, path)
}

func readResult(res *CacheResult) string {
	b, _ := io.ReadAll(res.Reader)
	_ = res.Reader.Close()
	return string(b)
}

// TestFetchesWithDifferentURLsServeTheirOwnBytes has both fetches store before
// either opens. Sharing one object, one of them would serve the other's bytes.
func TestFetchesWithDifferentURLsServeTheirOwnBytes(t *testing.T) {
	proxy, _, store, _ := setupTestProxy(t)
	bodies := map[string]string{fetchPathURLA: "bytes from a", fetchPathURLB: "bytes from b"}
	proxy.Fetcher = newBarrierFetcher(bodies)
	proxy.Storage = newGatedStorage(store, func(stores, _ int32) bool { return stores == 2 })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for url, want := range bodies {
		wg.Go(func() {
			res, err := proxy.GetOrFetchArtifactFromURL(ctx, "npm", "pkg", "1.0.0", staleFilename, url)
			if err != nil {
				t.Errorf("fetch of %s: %v", url, err)
				return
			}
			if got := readResult(res); got != want {
				t.Errorf("fetch of %s served %q, want %q", url, got, want)
			}
		})
	}
	wg.Wait()
}

// TestDigestMismatchLeavesAnotherFetchsObject has one fetch discard bytes that
// fail the declared digest before another fetch, which stored good bytes,
// opens them. Sharing one object, the discard would delete the good bytes.
func TestDigestMismatchLeavesAnotherFetchsObject(t *testing.T) {
	proxy, _, store, _ := setupTestProxy(t)
	proxy.Fetcher = newBarrierFetcher(map[string]string{fetchPathURLA: "good bytes", fetchPathURLB: "tampered bytes"})
	proxy.Storage = newGatedStorage(store, func(_, deletes int32) bool { return deletes == 1 })
	declared := "sha256:" + sha256Hex("good bytes")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Go(func() {
		res, err := proxy.GetOrFetchArtifactFromURLWithDigest(ctx, "npm", "pkg", "1.0.0", staleFilename, fetchPathURLA, declared)
		if err != nil {
			t.Errorf("good fetch: %v", err)
			return
		}
		if got := readResult(res); got != "good bytes" {
			t.Errorf("good fetch served %q", got)
		}
	})
	wg.Go(func() {
		_, err := proxy.GetOrFetchArtifactFromURLWithDigest(ctx, "npm", "pkg", "1.0.0", staleFilename, fetchPathURLB, declared)
		if !errors.Is(err, ErrArtifactDigestMismatch) {
			t.Errorf("tampered fetch: got %v, want a digest mismatch", err)
		}
	})
	wg.Wait()

	if _, ok := store.files[recordedStoragePath(t, proxy.DB, staleVersionPURL, staleFilename)]; !ok {
		t.Error("the recorded object was deleted")
	}
}
