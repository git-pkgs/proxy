package gitcache

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// offlineFront serves tc's cache with OpenOptions.Offline, and FetchWants
// on, so the test shows Offline turns that off.
func offlineFront(t *testing.T, tc *testCache, upstreamURL string) string {
	t.Helper()
	server := httptest.NewServer(testFrontWith(tc.Cache, upstreamURL, OpenOptions{FetchWants: true, Offline: true}))
	t.Cleanup(server.Close)
	return server.URL + "/git/up/"
}

// ageCopy makes the cached copy of repo look last checked long ago.
func ageCopy(t *testing.T, c *Cache, key string, age time.Duration) {
	t.Helper()
	waitIdle(t, c)
	rp := c.acquire(key)
	rp.lastChecked.Store(time.Now().Add(-age).UnixNano())
	c.release(rp)
}

func TestOfflineServesACopyPastStaleMaxAgeWithoutTheUpstream(t *testing.T) {
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, nil)
	head := runGit(t, tc.clone(t, "repo"), "rev-parse", "HEAD")
	ageCopy(t, tc.Cache, "up/repo.git", 48*time.Hour)

	// A newer commit upstream is not fetched, nor is the upstream asked.
	upstream.commit(work, "second")
	runGit(t, work, "push", "--quiet", "origin", "main")
	requests := upstream.requests.Load()
	url := offlineFront(t, tc, upstream.server.URL) + "repo"
	dir := filepath.Join(t.TempDir(), "clone")
	runGit(t, "", "clone", "--quiet", url, dir)
	if got := runGit(t, dir, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD = %s, want the cached %s", got, head)
	}
	// ls-refs and fetch go straight to git-upload-pack.
	runGit(t, dir, "fetch", "--quiet", "origin")
	if got := upstream.requests.Load() - requests; got != 0 {
		t.Errorf("the upstream got %d requests, want none", got)
	}
	// The online clone and the two offline ref listings count as requests.
	if stats := tc.Repositories(); len(stats) != 1 || stats[0].Requests != 3 {
		t.Errorf("stats %+v, want 3 requests", stats)
	}
	if logs := tc.logs.String(); !strings.Contains(logs, "serving offline") || !strings.Contains(logs, "checked=") {
		t.Errorf("no log of the offline clone and the copy's age:\n%s", logs)
	}
}

func TestOfflineRefusesARepositoryNotCachedWithoutTheUpstream(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, nil)

	for _, opts := range []OpenOptions{{Refresh: true, Offline: true}, {Offline: true}} {
		lease, err := tc.Open(context.Background(), "up/repo.git", upstream.server.URL+"/repo", opts)
		if !errors.Is(err, ErrNotCached) {
			t.Errorf("Open(%+v) = %v, want ErrNotCached", opts, err)
		}
		if lease != nil {
			lease.Release()
		}
	}
	if got := upstream.requests.Load(); got != 0 {
		t.Errorf("the upstream got %d requests, want none", got)
	}
	waitForRepos(t, tc.Cache, 0)
	if _, err := os.Stat(filepath.Join(tc.dir, "up", "repo.git")); !os.IsNotExist(err) {
		t.Errorf("repository directory created: %v", err)
	}
}

func TestOfflineDoesNotFetchWantsTheCacheLacks(t *testing.T) {
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	runGit(t, upstream.bare(), "config", "uploadpack.allowAnySHA1InWant", "true")
	tc := newTestCache(t, upstream.server.URL, nil)
	tc.clone(t, "repo")
	sha := upstream.pushPullRequest(work, 1, nil)
	waitIdle(t, tc.Cache)

	requests := upstream.requests.Load()
	if err := fetchByID(t, offlineFront(t, tc, upstream.server.URL)+"repo", sha); err == nil {
		t.Error("fetched a commit the cache lacks while offline")
	}
	if got := upstream.requests.Load() - requests; got != 0 {
		t.Errorf("the upstream got %d requests, want none", got)
	}
}
