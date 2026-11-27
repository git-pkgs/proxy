package handler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/git-pkgs/purl"
)

const acquireURL = "https://example.com/demo-1.0.0.tgz"

func acquireDemo(ctx context.Context, proxy *Proxy, req ArtifactRequest) (*ApprovedArtifact, error) {
	if req.Ecosystem == "" {
		req.Ecosystem, req.Name, req.Version, req.Filename = "npm", "demo", "1.0.0", "demo-1.0.0.tgz"
	}
	return proxy.Acquire(ctx, req)
}

func hitCount(t *testing.T, proxy *Proxy) int64 {
	t.Helper()
	art, err := proxy.DB.GetArtifact(purl.MakePURLString("npm", "demo", "1.0.0"), "demo-1.0.0.tgz")
	if err != nil || art == nil {
		t.Fatalf("artifact record = %v, %v", art, err)
	}
	return art.HitCount
}

func openAndRead(t *testing.T, proxy *Proxy, a *ApprovedArtifact) string {
	t.Helper()
	res, err := proxy.Open(context.Background(), a)
	if err != nil || res == nil || res.Reader == nil {
		t.Fatalf("Open = %+v, %v", res, err)
	}
	got, err := readAll(t, res.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestAcquireCachedOpensNothing(t *testing.T) {
	proxy, db, store, fetcher := setupTestProxy(t)
	seedPackage(t, db, store, "npm", "demo", "1.0.0", "demo-1.0.0.tgz", openTestContent)

	for _, req := range []ArtifactRequest{{}, {DownloadURL: acquireURL}} {
		before := hitCount(t, proxy)
		a, err := acquireDemo(context.Background(), proxy, req)
		if err != nil || a == nil || !a.cached {
			t.Fatalf("%+v: Acquire = %+v, %v; want the cached artifact", req, a, err)
		}
		if store.opens != 0 || fetcher.fetchCalled {
			t.Errorf("%+v: %d opens, fetched %v; want neither", req, store.opens, fetcher.fetchCalled)
		}
		if got := hitCount(t, proxy); got != before+1 {
			t.Errorf("%+v: hits %d, want %d", req, got, before+1)
		}
	}
}

func TestAcquireFetchesOnMissWithoutOpening(t *testing.T) {
	proxy, _, store, _ := setupTestProxy(t)
	proxy.Fetcher = &countingFetcher{content: openTestContent}

	a, err := acquireDemo(context.Background(), proxy, ArtifactRequest{DownloadURL: acquireURL})
	if err != nil || a == nil || a.cached || a.StoragePath == "" {
		t.Fatalf("Acquire = %+v, %v; want a fetched artifact", a, err)
	}
	if store.opens != 0 {
		t.Errorf("%d opens; Acquire delivers nothing", store.opens)
	}
	if got := recordedStoragePath(t, proxy.DB, purl.MakePURLString("npm", "demo", "1.0.0"), "demo-1.0.0.tgz"); got != a.StoragePath {
		t.Errorf("recorded at %q, Acquire returned %q", got, a.StoragePath)
	}
	if got := openAndRead(t, proxy, a); got != openTestContent {
		t.Errorf("read %q", got)
	}
}

func TestAcquireRefetchesWhenTheDigestDiffers(t *testing.T) {
	proxy, db, store, _ := setupTestProxy(t)
	seedPackage(t, db, store, "npm", "demo", "1.0.0", "demo-1.0.0.tgz", "old bytes")
	proxy.Fetcher = &countingFetcher{content: openTestContent}

	a, err := acquireDemo(context.Background(), proxy, ArtifactRequest{DownloadURL: acquireURL, Digest: "sha256:" + sha256Hex(openTestContent)})
	if err != nil || a == nil || a.cached {
		t.Fatalf("Acquire = %+v, %v; want a fresh fetch", a, err)
	}
	if got := openAndRead(t, proxy, a); got != openTestContent {
		t.Errorf("read %q, want the new bytes", got)
	}
}

func TestAcquireRefusals(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	proxy.StreamArtifacts = true
	if _, err := acquireDemo(context.Background(), proxy, ArtifactRequest{DownloadURL: acquireURL}); !errors.Is(err, ErrArtifactsStreamed) {
		t.Errorf("streaming: err = %v, want ErrArtifactsStreamed", err)
	}
	proxy.StreamArtifacts = false
	setTestDenylist(t, proxy, "pkg:npm/demo@1.0.0")
	for _, req := range []ArtifactRequest{{}, {DownloadURL: acquireURL}} {
		if _, err := acquireDemo(context.Background(), proxy, req); !errors.Is(err, ErrVersionDenied) {
			t.Errorf("%+v: err = %v, want ErrVersionDenied", req, err)
		}
	}
	setTestDenylist(t, proxy)
	if _, err := acquireDemo(context.Background(), proxy, ArtifactRequest{DownloadURL: acquireURL, Digest: "md5:abc"}); !errors.Is(err, ErrUnsupportedDigest) {
		t.Errorf("md5 digest: err = %v, want ErrUnsupportedDigest", err)
	}
}

func TestAcquireSharesAFetch(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	fetcher := &countingFetcher{content: openTestContent, delay: 50 * time.Millisecond}
	proxy.Fetcher = fetcher

	var wg sync.WaitGroup
	results := make([]*ApprovedArtifact, 4)
	for i := range results {
		wg.Go(func() {
			a, err := acquireDemo(context.Background(), proxy, ArtifactRequest{DownloadURL: acquireURL})
			if err != nil {
				t.Error(err)
			}
			results[i] = a
		})
	}
	wg.Wait()
	if got := fetcher.calls.Load(); got != 1 {
		t.Errorf("%d fetches, want one shared", got)
	}
	for _, a := range results {
		if a == nil || openAndRead(t, proxy, a) != openTestContent {
			t.Errorf("result %+v", a)
		}
	}
}

func TestAcquireNeverStreamsAMiss(t *testing.T) {
	proxy, _, store, _ := setupTestProxy(t)
	proxy.Fetcher = &countingFetcher{content: openTestContent}
	a, err := acquireDemo(StreamCacheMiss(context.Background()), proxy, ArtifactRequest{DownloadURL: acquireURL})
	if err != nil || a == nil || a.StoragePath == "" {
		t.Fatalf("Acquire = %+v, %v; want the stored artifact", a, err)
	}
	if _, ok := store.files[a.StoragePath]; !ok {
		t.Error("nothing stored")
	}
}

func TestAcquireUnrecordedIsDeletedOnRelease(t *testing.T) {
	proxy, store := setupUnrecorded(t)
	proxy.Fetcher = &countingFetcher{content: unrecordedContent}
	a, err := proxy.Acquire(context.Background(), ArtifactRequest{
		Ecosystem: "npm", Name: "gone", Version: "1.0.0", Filename: "gone-1.0.0.tgz", DownloadURL: unrecordedURL,
	})
	if err != nil || a == nil || a.release == nil {
		t.Fatalf("Acquire = %+v, %v; want an unrecorded artifact it holds", a, err)
	}
	if _, ok := store.files[a.StoragePath]; !ok {
		t.Fatal("unrecorded object deleted while held")
	}
	a.Release()
	a.Release()
	if _, ok := store.files[a.StoragePath]; ok {
		t.Error("unrecorded object kept after release")
	}
}

func TestAcquireUnrecordedIsDeletedWhenItsReaderCloses(t *testing.T) {
	proxy, store := setupUnrecorded(t)
	proxy.Fetcher = &countingFetcher{content: unrecordedContent}
	a, err := proxy.Acquire(context.Background(), ArtifactRequest{
		Ecosystem: "npm", Name: "gone", Version: "1.0.0", Filename: "gone-1.0.0.tgz", DownloadURL: unrecordedURL,
	})
	if err != nil || a == nil {
		t.Fatal(err)
	}
	if got := openAndRead(t, proxy, a); got != unrecordedContent {
		t.Errorf("read %q", got)
	}
	a.Release()
	if _, ok := store.files[a.StoragePath]; ok {
		t.Error("unrecorded object kept after its reader closed")
	}
}

// TestReleaseAfterOpenLeavesTheHoldWithTheReader checks that releasing an
// opened artifact, as a deferred Release does, keeps its unrecorded object
// until the reader closes.
func TestReleaseAfterOpenLeavesTheHoldWithTheReader(t *testing.T) {
	proxy, store := setupUnrecorded(t)
	proxy.Fetcher = &countingFetcher{content: unrecordedContent}
	a, err := proxy.Acquire(context.Background(), ArtifactRequest{
		Ecosystem: "npm", Name: "gone", Version: "1.0.0", Filename: "gone-1.0.0.tgz", DownloadURL: unrecordedURL,
	})
	if err != nil || a == nil {
		t.Fatal(err)
	}
	result, err := proxy.Open(context.Background(), a)
	if err != nil || result == nil || result.Reader == nil {
		t.Fatalf("Open = %+v, %v", result, err)
	}
	a.Release()
	if _, ok := store.files[a.StoragePath]; !ok {
		t.Error("unrecorded object deleted while its reader is open")
	}
	if err := result.Reader.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.files[a.StoragePath]; ok {
		t.Error("unrecorded object kept after its reader closed")
	}
}

func TestAcquireRefetchesWhenTheObjectIsGone(t *testing.T) {
	proxy, db, store, _ := setupTestProxy(t)
	seedPackage(t, db, store, "npm", "demo", "1.0.0", "demo-1.0.0.tgz", openTestContent)
	delete(store.files, "npm/demo/1.0.0/demo-1.0.0.tgz")
	fetcher := &countingFetcher{content: openTestContent}
	proxy.Fetcher = fetcher

	a, err := acquireDemo(context.Background(), proxy, ArtifactRequest{DownloadURL: acquireURL})
	if err != nil || a == nil || a.cached || fetcher.calls.Load() != 1 {
		t.Fatalf("Acquire = %+v, %v after %d fetches; want a fresh fetch", a, err, fetcher.calls.Load())
	}
	if got := openAndRead(t, proxy, a); got != openTestContent {
		t.Errorf("read %q", got)
	}
}
