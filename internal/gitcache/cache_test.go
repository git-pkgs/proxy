package gitcache

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/git-pkgs/registries/safehttp"
)

func TestCloneMatchesUpstream(t *testing.T) {
	for _, version := range []string{"0", "1", "2"} {
		t.Run("protocol v"+version, func(t *testing.T) {
			upstream := newFakeUpstream(t)
			upstream.newRepo("repo")
			tc := newTestCache(t, upstream.server.URL, nil)

			clone := tc.clone(t, "repo.git", "-c", "protocol.version="+version)

			want := runGit(t, "", "ls-remote", "--heads", "--tags", upstream.bare())
			if got := runGit(t, "", "-c", "protocol.version="+version, "ls-remote", "--heads", "--tags", tc.url("repo.git")); got != want {
				t.Errorf("ls-remote through the cache:\n%s\nwant:\n%s", got, want)
			}
			if got, want := runGit(t, clone, "rev-parse", "HEAD"), runGit(t, upstream.bare(), "rev-parse", "main"); got != want {
				t.Errorf("HEAD = %s, want %s", got, want)
			}
			runGit(t, clone, "fsck", "--no-dangling", "--no-progress")
		})
	}
}

func TestUpstreamWithoutProtocolV2(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.v0Only = true
	upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, nil)

	clone := tc.clone(t, "repo")
	if got, want := runGit(t, clone, "rev-parse", "HEAD"), runGit(t, upstream.bare(), "rev-parse", "main"); got != want {
		t.Errorf("HEAD = %s, want %s", got, want)
	}
}

func TestUnchangedRepositoryIsNotFetchedAgain(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, nil)

	tc.clone(t, "repo")
	fetches := upstream.gitRequests.Load()
	tc.clone(t, "repo")
	if got := upstream.gitRequests.Load(); got != fetches {
		t.Errorf("second clone made %d upstream fetch requests, want none", got-fetches)
	}
}

func TestPushedCommitIsServedOnNextClone(t *testing.T) {
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, nil)
	existing := tc.clone(t, "repo")

	head := upstream.commit(work, "second")
	runGit(t, work, "push", "--quiet", "origin", "main")

	if got := runGit(t, tc.clone(t, "repo"), "rev-parse", "HEAD"); got != head {
		t.Errorf("clone after push: HEAD = %s, want %s", got, head)
	}
	runGit(t, existing, "pull", "--quiet", "--ff-only")
	if got := runGit(t, existing, "rev-parse", "HEAD"); got != head {
		t.Errorf("pull after push: HEAD = %s, want %s", got, head)
	}
	if got := runGit(t, tc.clone(t, "repo", "--depth=1"), "rev-list", "--count", "HEAD"); got != "1" {
		t.Errorf("shallow clone has %s commits, want 1", got)
	}
}

func TestOnlyBranchesAndTagsAreCachedAndPruned(t *testing.T) {
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	runGit(t, work, "push", "--quiet", "origin", "main:refs/changes/01/1/1", "main:refs/heads/gone", "main:refs/tags/gone")
	tc := newTestCache(t, upstream.server.URL, nil)
	tc.clone(t, "repo")

	refs := tc.cachedRefs(t)
	if strings.Contains(refs, "refs/changes/") {
		t.Errorf("cached refs/changes:\n%s", refs)
	}
	if !strings.Contains(refs, "refs/heads/gone") || !strings.Contains(refs, "refs/tags/gone") {
		t.Fatalf("branch and tag not cached:\n%s", refs)
	}

	runGit(t, work, "push", "--quiet", "origin", ":refs/heads/gone", ":refs/tags/gone")
	tc.clone(t, "repo")
	if refs := tc.cachedRefs(t); strings.Contains(refs, "gone") {
		t.Errorf("deleted branch or tag still cached:\n%s", refs)
	}

	// Gerrit advertises a ref per patch set, so asking for every ref would
	// list hundreds of thousands for a large repository.
	bodies := upstream.lsRefsBodies()
	if len(bodies) == 0 {
		t.Fatal("no ls-refs request reached the upstream")
	}
	for _, body := range bodies {
		if !strings.Contains(body, "ref-prefix refs/heads/") || !strings.Contains(body, "ref-prefix refs/tags/") {
			t.Errorf("ls-refs asked for every ref, want branches and tags only:\n%q", body)
		}
	}
}

func TestDefaultBranchChangeIsFollowed(t *testing.T) {
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, nil)
	tc.clone(t, "repo")

	runGit(t, work, "checkout", "--quiet", "-b", "trunk")
	head := upstream.commit(work, "on trunk")
	runGit(t, work, "push", "--quiet", "origin", "trunk")
	runGit(t, upstream.bare(), "symbolic-ref", "HEAD", "refs/heads/trunk")

	clone := tc.clone(t, "repo")
	if got := runGit(t, clone, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD = %s, want trunk's %s", got, head)
	}
}

func TestConcurrentColdClonesShareOneFetch(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, nil)

	release := upstream.holdRequests()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			dir := filepath.Join(t.TempDir(), "clone")
			if out, err := tryGit(t, "", "clone", "--quiet", tc.url("repo"), dir); err != nil {
				t.Errorf("clone: %v\n%s", err, out)
			}
		})
	}
	time.Sleep(200 * time.Millisecond)
	release()
	wg.Wait()

	if got := upstream.gitSessions.Load(); got != 1 {
		t.Errorf("upstream saw %d fetches, want 1", got)
	}
}

func TestMissingRepositoryLeavesNothingBehind(t *testing.T) {
	upstream := newFakeUpstream(t)
	tc := newTestCache(t, upstream.server.URL, nil)

	resp, err := http.Get(tc.url("missing") + "/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status %d, want 404", resp.StatusCode)
	}
	waitForRepos(t, tc.Cache, 0)
	if _, err := os.Stat(filepath.Join(tc.dir, "up", "missing.git")); !os.IsNotExist(err) {
		t.Errorf("repository directory left behind: %v", err)
	}
}

func TestStaleCopyServedOnlyWithinStaleMaxAge(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, nil)
	tc.clone(t, "repo")

	upstream.status.Store(http.StatusServiceUnavailable)
	tc.clone(t, "repo")

	waitIdle(t, tc.Cache)
	rp := tc.acquire("up/repo.git")
	rp.lastChecked.Store(time.Now().Add(-2 * time.Hour).UnixNano())
	tc.release(rp)
	if out, err := tryGit(t, "", "ls-remote", tc.url("repo")); err == nil {
		t.Errorf("served a copy older than stale_max_age:\n%s", out)
	}
}

func TestRefusedRefreshIsNotServedStale(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusMovedPermanently} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			upstream := newFakeUpstream(t)
			upstream.newRepo("repo")
			tc := newTestCache(t, upstream.server.URL, nil)
			tc.clone(t, "repo")

			upstream.status.Store(int32(status))
			if out, err := tryGit(t, "", "ls-remote", tc.url("repo")); err == nil {
				t.Errorf("served the cached copy after status %d:\n%s", status, out)
			}
		})
	}
}

func TestSlowUpstreamServesCachedCopyAfterRefreshWait(t *testing.T) {
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, func(o *Options) { o.RefreshWait = 100 * time.Millisecond })
	old := runGit(t, tc.clone(t, "repo"), "rev-parse", "HEAD")
	head := upstream.commit(work, "second")
	runGit(t, work, "push", "--quiet", "origin", "main")

	release := upstream.holdRequests()
	defer release()
	if got := runGit(t, tc.clone(t, "repo"), "rev-parse", "HEAD"); got != old {
		t.Errorf("HEAD = %s, want the cached %s", got, old)
	}
	release()
	waitFor(t, func() bool { return strings.Contains(tc.cachedRefs(t), head) })
}

// TestSlowFetchAfterPushIsWaitedFor has the upstream answer the ref check at
// once and the fetch after a push take longer than RefreshWait: a clone
// still gets the pushed commit. RefreshWait leaves the check time to finish
// on a slow machine.
func TestSlowFetchAfterPushIsWaitedFor(t *testing.T) {
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, func(o *Options) { o.RefreshWait = 2 * time.Second })
	tc.clone(t, "repo")
	head := upstream.commit(work, "second")
	runGit(t, work, "push", "--quiet", "origin", "main")

	upstream.delayFetches.Store(int64(4 * time.Second))
	if got := runGit(t, tc.clone(t, "repo"), "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD = %s, want the pushed %s", got, head)
	}
}

// TestStalledFetchAfterPushServesCachedCopy has the fetch after a push
// stall: once it is abandoned, the clone gets the cached copy.
func TestStalledFetchAfterPushServesCachedCopy(t *testing.T) {
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, func(o *Options) {
		o.RefreshWait = 100 * time.Millisecond
		o.lowSpeedTime = 1
	})
	old := runGit(t, tc.clone(t, "repo"), "rev-parse", "HEAD")
	upstream.commit(work, "second")
	runGit(t, work, "push", "--quiet", "origin", "main")

	upstream.stallFetches.Store(true)
	if got := runGit(t, tc.clone(t, "repo"), "rev-parse", "HEAD"); got != old {
		t.Errorf("HEAD = %s, want the cached %s", got, old)
	}
}

func TestUpstreamCredentialsReachEveryUpstreamRequest(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.requireAuth = "Bearer secret"
	upstream.newRepo("repo")
	client := &http.Client{Transport: headerTransport{"Authorization", "Bearer secret"}}
	tc := newTestCache(t, upstream.server.URL, func(o *Options) { o.Client = client })

	tc.clone(t, "repo")
	if got := upstream.unauthed.Load(); got != 0 {
		t.Errorf("%d upstream requests lacked credentials", got)
	}
	if upstream.gitRequests.Load() == 0 {
		t.Error("no fetch went through the relay")
	}
}

type headerTransport struct{ name, value string }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(h.name, h.value)
	return http.DefaultTransport.RoundTrip(r)
}

// TestUpstreamAddressChecksApplyToGit uses a client that refuses loopback
// addresses, as the proxy's does by default. git must not reach the loopback
// upstream around it, whether the ref check or only git's fetch is refused.
func TestUpstreamAddressChecksApplyToGit(t *testing.T) {
	blocked := safehttp.New(nil, safehttp.Options{})
	for name, client := range map[string]*http.Client{
		"every request": blocked,
		"git's fetch":   {Transport: gitOnlyTransport{blocked.Transport}},
	} {
		t.Run(name, func(t *testing.T) {
			upstream := newFakeUpstream(t)
			upstream.newRepo("repo")
			tc := newTestCache(t, upstream.server.URL, func(o *Options) { o.Client = client })

			if out, err := tryGit(t, "", "ls-remote", tc.url("repo")); err == nil {
				t.Fatalf("clone through a blocked upstream succeeded:\n%s", out)
			}
			if got := upstream.gitRequests.Load(); got != 0 {
				t.Errorf("git reached the upstream with %d requests", got)
			}
		})
	}
}

// gitOnlyTransport sends the cache's own ref checks with the default
// transport, and git's relayed requests with next.
type gitOnlyTransport struct{ next http.RoundTripper }

func (g gitOnlyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.UserAgent() == gitUserAgent {
		return http.DefaultTransport.RoundTrip(r)
	}
	return g.next.RoundTrip(r)
}

// TestRedirectIsNotFollowed redirects the ref check, then only git's fetch.
func TestRedirectIsNotFollowed(t *testing.T) {
	for _, redirectRefCheck := range []bool{true, false} {
		t.Run(fmt.Sprint("ref check redirected: ", redirectRefCheck), func(t *testing.T) {
			target := newFakeUpstream(t)
			target.newRepo("repo")
			targetURL, _ := url.Parse(target.server.URL)
			passThrough := httputil.NewSingleHostReverseProxy(targetURL)
			redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.UserAgent() == gitUserAgent && !redirectRefCheck {
					passThrough.ServeHTTP(w, r)
					return
				}
				http.Redirect(w, r, target.server.URL+r.URL.RequestURI(), http.StatusFound)
			}))
			defer redirect.Close()
			tc := newTestCache(t, redirect.URL, nil)

			if out, err := tryGit(t, "", "ls-remote", tc.url("repo")); err == nil {
				t.Fatalf("followed a redirect:\n%s", out)
			}
			if got := target.gitRequests.Load(); got != 0 {
				t.Errorf("redirect target received %d requests from git", got)
			}
		})
	}
}

func TestRepositoryOverMaxRepoSizeIsRefused(t *testing.T) {
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	big := make([]byte, 256<<10)
	_, _ = rand.Read(big)
	if err := os.WriteFile(filepath.Join(work, "big.bin"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "big.bin")
	runGit(t, work, "commit", "--quiet", "-m", "big")
	runGit(t, work, "push", "--quiet", "origin", "main")

	// max_size caps max_repo_size.
	for name, configure := range map[string]func(*Options){
		"max_repo_size": func(o *Options) { o.MaxRepoSize = 64 << 10 },
		"max_size":      func(o *Options) { o.MaxSize = 64 << 10 },
	} {
		t.Run(name, func(t *testing.T) {
			tc := newTestCache(t, upstream.server.URL, configure)

			resp, err := http.Get(tc.url("repo") + "/info/refs?service=git-upload-pack")
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusInsufficientStorage {
				t.Errorf("status %d, want 507", resp.StatusCode)
			}
			waitForRepos(t, tc.Cache, 0)
			if entries, _ := os.ReadDir(tc.tmpDir()); len(entries) != 0 {
				t.Errorf("left %d entries in the temporary directory", len(entries))
			}

			requests := upstream.requests.Load()
			if out, err := tryGit(t, "", "ls-remote", tc.url("repo")); err == nil {
				t.Errorf("second attempt succeeded:\n%s", out)
			}
			if got := upstream.requests.Load(); got != requests {
				t.Errorf("second attempt made %d upstream requests, want none", got-requests)
			}
		})
	}
}

// TestEvictionRemovesLeastRecentlyUsedIdleRepositories makes the least
// recently used repository one a request is reading, so the next one is
// evicted instead.
func TestEvictionRemovesLeastRecentlyUsedIdleRepositories(t *testing.T) {
	upstream := newFakeUpstream(t)
	for _, name := range []string{"old", "busy", "new"} {
		upstream.newRepo(name)
	}
	size := repoSize(t, upstream, "new")
	tc := newTestCache(t, upstream.server.URL, func(o *Options) { o.MaxSize = 2*size + size/2 })

	tc.clone(t, "busy")
	busy := tc.acquire("up/busy.git")
	defer tc.release(busy)
	busy.lastUsed.Store(0)
	tc.clone(t, "old")
	waitFor(t, func() bool { return tc.usersOf("up/old.git") == 0 })
	tc.repo("up/old.git").lastUsed.Store(1)

	// The third clone takes the cache past MaxSize.
	tc.clone(t, "new")
	waitFor(t, func() bool { return !fileExists(filepath.Join(tc.dir, "up", "old.git")) })
	for _, name := range []string{"busy", "new"} {
		if !fileExists(filepath.Join(tc.dir, "up", name+".git")) {
			t.Errorf("%s was evicted", name)
		}
	}
	tc.clone(t, "old")
}

// repoSize is the size a cache gives repository name once cloned.
func repoSize(t *testing.T, upstream *fakeUpstream, name string) int64 {
	t.Helper()
	probe := newTestCache(t, upstream.server.URL, nil)
	probe.clone(t, name)
	return probe.repo("up/" + name + ".git").size.Load()
}

func TestNewLoadsCachedRepositories(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, nil)
	tc.clone(t, "repo")
	if err := os.MkdirAll(filepath.Join(tc.tmpDir(), "clone-leftover"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := tc.Close(); err != nil {
		t.Fatal(err)
	}

	reloaded, err := New(Options{
		Upstreams:   map[string]string{"up": "up"},
		CacheDir:    tc.dir,
		StaleMaxAge: time.Hour,
		Client:      &http.Client{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reloaded.Close() }()

	rp := reloaded.repos["up/repo.git"] // nothing else uses reloaded yet
	if rp == nil || rp.size.Load() == 0 || rp.lastChecked.Load() == 0 {
		t.Fatalf("repository not loaded: %+v", rp)
	}
	if entries, _ := os.ReadDir(reloaded.tmpDir()); len(entries) != 0 {
		t.Errorf("temporary directory not cleared: %v", entries)
	}
}

func waitForRepos(t *testing.T, c *Cache, n int) {
	t.Helper()
	waitFor(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.repos) == n
	})
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for !cond() {
		select {
		case <-ctx.Done():
			t.Fatal("condition not met in time")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestRewoundCacheIsBroughtBackInLine changes the cache behind the upstream's
// back, so it only matches again if the next request refreshes branches and
// tags and prunes what the upstream does not have.
func TestRewoundCacheIsBroughtBackInLine(t *testing.T) {
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	upstream.commit(work, "second")
	runGit(t, work, "push", "--quiet", "origin", "main")
	tc := newTestCache(t, upstream.server.URL, nil)
	tc.clone(t, "repo")

	cached := filepath.Join(tc.dir, "up", "repo.git")
	runGit(t, cached, "update-ref", "refs/heads/main", "main~1")
	runGit(t, cached, "update-ref", "refs/heads/extra", "main")
	runGit(t, cached, "tag", "extra", "main")

	want := runGit(t, "", "ls-remote", "--heads", "--tags", upstream.bare())
	if got := runGit(t, "", "ls-remote", "--heads", "--tags", tc.url("repo")); got != want {
		t.Errorf("ls-remote through the cache:\n%s\nwant:\n%s", got, want)
	}
}

// TestConcurrentWarmClonesDuringRefresh serves clones from the cache while a
// fetch writes to it.
func TestConcurrentWarmClonesDuringRefresh(t *testing.T) {
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, func(o *Options) { o.RefreshWait = time.Millisecond })
	old := runGit(t, tc.clone(t, "repo"), "rev-parse", "HEAD")
	head := upstream.commit(work, "second")
	runGit(t, work, "push", "--quiet", "origin", "main")

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			dir := filepath.Join(t.TempDir(), "clone")
			if out, err := tryGit(t, "", "clone", "--quiet", tc.url("repo"), dir); err != nil {
				t.Errorf("clone: %v\n%s", err, out)
				return
			}
			if got := runGit(t, dir, "rev-parse", "HEAD"); got != old && got != head {
				t.Errorf("HEAD = %s, want %s or %s", got, old, head)
			}
			runGit(t, dir, "fsck", "--no-dangling", "--no-progress")
		})
	}
	wg.Wait()
	waitFor(t, func() bool { return strings.Contains(tc.cachedRefs(t), head) })
}

// TestStalledRefCheckFailsInsteadOfHanging has the upstream send headers and
// then nothing, as a broken load balancer might.
func TestStalledRefCheckFailsInsteadOfHanging(t *testing.T) {
	stall := make(chan struct{})
	defer close(stall)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", advertisementType)
		w.WriteHeader(http.StatusOK)
		_ = http.NewResponseController(w).Flush()
		select {
		case <-stall:
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	tc := newTestCache(t, upstream.URL, func(o *Options) { o.refCheckTimeout = 200 * time.Millisecond })

	start := time.Now()
	if out, err := tryGit(t, "", "ls-remote", tc.url("repo")); err == nil {
		t.Fatalf("ls-remote succeeded:\n%s", out)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("took %s to fail", elapsed)
	}
}

// TestStalledFetchFailsInsteadOfHanging has the upstream answer the ref check
// but never git's fetch, as a connection that stalls mid-pack would.
func TestStalledFetchFailsInsteadOfHanging(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.newRepo("repo")
	upstream.stallFetches.Store(true)
	tc := newTestCache(t, upstream.server.URL, func(o *Options) { o.lowSpeedTime = 1 })

	start := time.Now()
	if out, err := tryGit(t, "", "ls-remote", tc.url("repo")); err == nil {
		t.Fatalf("ls-remote succeeded:\n%s", out)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("took %s to fail", elapsed)
	}
}

// TestFetchEvictsToMaxSize checks eviction runs on its own once a fetch has
// grown the cache past MaxSize.
func TestFetchEvictsToMaxSize(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.newRepo("first")
	upstream.newRepo("second")
	size := repoSize(t, upstream, "first")

	tc := newTestCache(t, upstream.server.URL, func(o *Options) { o.MaxSize = size + size/2 })
	tc.clone(t, "first")
	waitIdle(t, tc.Cache)
	tc.clone(t, "second")
	waitFor(t, func() bool { return !fileExists(filepath.Join(tc.dir, "up", "first.git")) })
	if !fileExists(filepath.Join(tc.dir, "up", "second.git")) {
		t.Error("evicted the repository just fetched")
	}
}

// TestReleaseEvictsWhatEvictionHadToSkip fills the cache past MaxSize while
// every repository is in use, so eviction after the fetch can remove none.
// The first release brings the cache back within MaxSize.
func TestReleaseEvictsWhatEvictionHadToSkip(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.newRepo("first")
	upstream.newRepo("second")
	size := repoSize(t, upstream, "first")

	tc := newTestCache(t, upstream.server.URL, func(o *Options) { o.MaxSize = size + size/2 })
	first := tc.acquire("up/first.git")
	tc.clone(t, "first")
	second := tc.acquire("up/second.git")
	defer tc.release(second)
	tc.clone(t, "second")
	waitFor(t, func() bool {
		tc.mu.Lock()
		defer tc.mu.Unlock()
		return tc.overBudget
	})

	tc.release(first)
	if fileExists(first.dir) {
		t.Error("first is still cached once released")
	}
	if !fileExists(second.dir) {
		t.Error("evicted second while in use")
	}
	tc.clone(t, "second")
}

// TestNewRemovesRepointedUpstreams starts a cache whose directory holds an
// upstream's cache at its old URL, and one of an upstream no longer
// configured. Only the first is known to be the cache's to remove.
func TestNewRemovesRepointedUpstreams(t *testing.T) {
	dir := t.TempDir()
	current := UpstreamDir("up", "https://new.example")
	repointed := UpstreamDir("up", "https://old.example")
	unconfigured := UpstreamDir("gone", "https://gone.example")
	// Named after a configured upstream's name, but another upstream's.
	longer := UpstreamDir("up-api", "https://api.example")
	for _, name := range []string{current, repointed, unconfigured, longer} {
		runGit(t, "", "init", "--quiet", "--bare", filepath.Join(dir, name, "repo.git"))
	}
	c, err := New(Options{
		CacheDir:    dir,
		StaleMaxAge: time.Hour,
		Client:      &http.Client{},
		Upstreams:   map[string]string{"up": current},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	for name, want := range map[string]bool{current: true, repointed: false, unconfigured: true, longer: true} {
		if got := fileExists(filepath.Join(dir, name)); got != want {
			t.Errorf("%s present = %v, want %v", name, got, want)
		}
	}
	if keys := slices.Collect(maps.Keys(c.repos)); len(keys) != 1 || keys[0] != current+"/repo.git" {
		t.Errorf("loaded %v, want only %s/repo.git", keys, current)
	}
}

// TestCacheDirSharedWithOtherData points the cache at a directory that also
// holds an artifact store and a database, as git.cache_dir: ./cache would.
func TestCacheDirSharedWithOtherData(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "artifacts", "npm"), 0o750)
	_ = os.WriteFile(filepath.Join(dir, "artifacts", "npm", "pkg.tgz"), []byte("x"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "proxy.db"), []byte("db"), 0o600)
	runGit(t, "", "init", "--quiet", "--bare", filepath.Join(dir, "artifacts", "foo", "bar.git"))
	c, err := New(Options{
		CacheDir:  dir,
		Client:    &http.Client{},
		Upstreams: map[string]string{"github": UpstreamDir("github", "https://github.com")},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	for _, p := range []string{"artifacts/npm/pkg.tgz", "proxy.db", "artifacts/foo/bar.git"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("%s was deleted at startup: %v", p, err)
		}
	}
	if len(c.repos) != 0 {
		t.Errorf("loaded %v from outside the upstream directories", slices.Collect(maps.Keys(c.repos)))
	}
}

// TestCloseWaitsForRefreshes closes the cache while a clone is held up
// upstream. When Close returns, the clone must have finished, temporary
// directory included, so nothing is left writing to the cache.
func TestCloseWaitsForRefreshes(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.newRepo("repo")
	upstream.stallFetches.Store(true)
	tc := newTestCache(t, upstream.server.URL, nil)
	clones := func() []string {
		names, _ := filepath.Glob(filepath.Join(tc.tmpDir(), "clone-*"))
		return names
	}

	go func() { _, _ = tryGit(t, "", "ls-remote", tc.url("repo")) }()
	waitFor(t, func() bool { return len(clones()) > 0 })
	if err := tc.Close(); err != nil {
		t.Fatal(err)
	}
	if left := clones(); len(left) != 0 {
		t.Errorf("Close returned with the clone still running: %v", left)
	}

	before := upstream.requests.Load()
	if _, err := tc.Open(context.Background(), "up/other.git", upstream.server.URL+"/other", OpenOptions{}); !errors.Is(err, errClosed) {
		t.Errorf("Open after Close: %v, want errClosed", err)
	}
	if got := upstream.requests.Load() - before; got != 0 {
		t.Errorf("Open after Close made %d upstream requests", got)
	}
}

// TestCacheDirInUse opens a second cache on a directory one already uses,
// as a second pod on a shared volume would, then again once the first has
// closed.
func TestCacheDirInUse(t *testing.T) {
	dir := t.TempDir()
	open := func() (*Cache, error) { return New(Options{CacheDir: dir, Client: &http.Client{}}) }
	first, err := open()
	if err != nil {
		t.Fatal(err)
	}
	if second, err := open(); err == nil {
		_ = second.Close()
		t.Fatal("a second cache opened on the same directory")
	} else if !strings.Contains(err.Error(), "in use by another process") {
		t.Errorf("error = %v, want one naming the directory as in use", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := open()
	if err != nil {
		t.Fatalf("reopening after Close: %v", err)
	}
	_ = again.Close()
}

// TestCloseTwice checks closing again does nothing, as a cleanup that also
// closes the cache may.
func TestCloseTwice(t *testing.T) {
	c, err := New(Options{CacheDir: t.TempDir(), Client: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(c.Close(), c.Close()); err != nil {
		t.Errorf("closing twice: %v", err)
	}
}
