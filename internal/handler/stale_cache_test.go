package handler

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// These tests cover an upstream re-publishing a version: the cache holds one
// artifact and upstream now declares another digest for it.

const (
	stalePkgPURL     = "pkg:npm/pkg"
	staleVersionPURL = "pkg:npm/pkg@1.0.0"
	staleFilename    = "pkg-1.0.0.tgz"
	staleStoragePath = "npm/pkg/1.0.0/pkg-1.0.0.tgz"
	staleURL         = "https://registry.npmjs.org/pkg/-/pkg-1.0.0.tgz"
)

// seedCachedArtifact commits content the way a fetch does.
func seedCachedArtifact(t *testing.T, proxy *Proxy, store *mockStorage, content string) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := store.Store(ctx, staleStoragePath, strings.NewReader(content)); err != nil {
		t.Fatalf("seeding storage: %v", err)
	}
	artifact := testArtifact(content, staleVersionPURL, staleFilename, "application/gzip")
	if err := proxy.updateCacheDB("npm", "pkg", stalePkgPURL, staleURL, staleStoragePath, artifact); err != nil {
		t.Fatalf("seeding cache record: %v", err)
	}
}

// cachedDigest reports the digest the cache record holds, or "" without one.
func cachedDigest(t *testing.T, proxy *Proxy) string {
	t.Helper()
	record, err := proxy.DB.GetCachedArtifact(stalePkgPURL, staleVersionPURL, staleFilename)
	if err != nil {
		t.Fatalf("reading cache record: %v", err)
	}
	if record == nil {
		return ""
	}
	return record.Artifact.Digest.Encoded()
}

func bytesPresent(store *mockStorage) bool {
	r, err := store.Open(context.Background(), staleStoragePath)
	if err != nil {
		return false
	}
	_ = r.Close()
	return true
}

func TestStaleCacheCheckHasNoSideEffects(t *testing.T) {
	proxy, _, store, _ := setupTestProxy(t)
	seedCachedArtifact(t, proxy, store, "old bytes")

	res, err := proxy.getCachedArtifactWithUpstreamHash(context.Background(),
		stalePkgPURL, staleVersionPURL, staleFilename, sha256Hex("new bytes"))
	if err != nil {
		t.Fatalf("cache check failed: %v", err)
	}
	if res != nil {
		drain(res)
		t.Fatal("stale entry was served")
	}
	if got := cachedDigest(t, proxy); got != sha256Hex("old bytes") {
		t.Errorf("record digest = %q, want the stale one kept: the check must not discard", got)
	}
	if !bytesPresent(store) {
		t.Error("stale bytes were deleted by the check")
	}
}

func TestStaleCacheIsDiscardedBeforeTheFetch(t *testing.T) {
	proxy, _, store, fetcher := setupTestProxy(t)
	seedCachedArtifact(t, proxy, store, "old bytes")
	boom := errors.New("upstream unavailable")
	fetcher.fetchErr = boom

	_, err := proxy.GetOrFetchArtifactFromURLWithDigest(context.Background(),
		"npm", "pkg", "1.0.0", staleFilename, staleURL, "sha256:"+sha256Hex("new bytes"))
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want the fetch failure", err)
	}
	if got := cachedDigest(t, proxy); got != "" {
		t.Errorf("stale record survived a failed refresh, digest = %q", got)
	}
	if bytesPresent(store) {
		t.Error("stale bytes survived a failed refresh")
	}
}

func TestStaleCacheIsReplacedByTheFetch(t *testing.T) {
	proxy, _, store, fetcher := setupTestProxy(t)
	seedCachedArtifact(t, proxy, store, "old bytes")
	fetcher.artifact = artifactBody("new bytes")
	upstream := sha256Hex("new bytes")

	res, err := proxy.GetOrFetchArtifactFromURLWithDigest(context.Background(),
		"npm", "pkg", "1.0.0", staleFilename, staleURL, "sha256:"+upstream)
	if err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	got, err := io.ReadAll(res.Reader)
	_ = res.Reader.Close()
	if err != nil || string(got) != "new bytes" {
		t.Fatalf("got %q (err %v), want the refreshed bytes", got, err)
	}
	if !fetcher.fetchCalled {
		t.Error("stale entry was served without a fetch")
	}
	if d := cachedDigest(t, proxy); d != upstream {
		t.Errorf("record digest = %q, want %q", d, upstream)
	}

	fetcher.fetchCalled = false
	res, err = proxy.GetOrFetchArtifactFromURLWithDigest(context.Background(),
		"npm", "pkg", "1.0.0", staleFilename, staleURL, "sha256:"+upstream)
	if err != nil {
		t.Fatalf("request after refresh failed: %v", err)
	}
	drain(res)
	if fetcher.fetchCalled || !res.Cached {
		t.Errorf("request after refresh: fetched=%v cached=%v, want served from cache", fetcher.fetchCalled, res.Cached)
	}
}

// TestLateLeaderKeepsRefreshedEntry is the race, at the point it would happen:
// a caller whose cache check saw a stale entry reaches the coalescing step
// after another caller's fetch replaced it. It must serve the replacement.
func TestLateLeaderKeepsRefreshedEntry(t *testing.T) {
	proxy, _, store, fetcher := setupTestProxy(t)
	seedCachedArtifact(t, proxy, store, "new bytes")
	fetcher.fetchErr = errors.New("must not fetch")
	upstream := sha256Hex("new bytes")

	res, err := proxy.coalescedFetchFromURL(context.Background(),
		"npm", "pkg", "1.0.0", staleFilename, stalePkgPURL, staleVersionPURL, staleURL, nil, upstream)
	if err != nil {
		t.Fatalf("late leader failed: %v", err)
	}
	got, err := io.ReadAll(res.Reader)
	_ = res.Reader.Close()
	if err != nil || string(got) != "new bytes" {
		t.Fatalf("got %q (err %v), want the refreshed bytes", got, err)
	}
	if fetcher.fetchCalled {
		t.Error("refreshed entry was fetched again")
	}
	if d := cachedDigest(t, proxy); d != upstream {
		t.Errorf("refreshed record was discarded, digest = %q", d)
	}
	if !bytesPresent(store) {
		t.Error("refreshed bytes were deleted")
	}
}
