package gitcache

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// pushPullRequest commits on a new branch from main, with data in a file if
// given, and pushes it only as refs/pull/{n}/head, so the commit is on no
// branch or tag upstream.
func (u *fakeUpstream) pushPullRequest(work string, n int, data []byte) string {
	u.t.Helper()
	branch := "pr" + strconv.Itoa(n)
	runGit(u.t, work, "checkout", "--quiet", "-b", branch, "main")
	if data != nil {
		if err := os.WriteFile(filepath.Join(work, branch+".bin"), data, 0o600); err != nil {
			u.t.Fatal(err)
		}
		runGit(u.t, work, "add", branch+".bin")
	}
	runGit(u.t, work, "commit", "--quiet", "--allow-empty", "-m", branch)
	runGit(u.t, work, "push", "--quiet", "origin", "HEAD:refs/pull/"+strconv.Itoa(n)+"/head")
	runGit(u.t, work, "checkout", "--quiet", "main")
	return runGit(u.t, work, "rev-parse", branch)
}

// fetchByID fetches one commit by ID from url, as actions/checkout does.
func fetchByID(t *testing.T, url, sha string) error {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	_, err := tryGit(t, dir, "fetch", "--quiet", "--depth=1", url, sha)
	return err
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func (tc *testCache) wantedRefs(t *testing.T) []string {
	t.Helper()
	var refs []string
	for line := range strings.Lines(tc.cachedRefs(t)) {
		if _, name, _ := strings.Cut(strings.TrimSpace(line), " "); strings.HasPrefix(name, wantedPrefix) {
			refs = append(refs, name)
		}
	}
	return refs
}

func TestMergedWantedCommitIsUnpinned(t *testing.T) {
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	runGit(t, upstream.bare(), "config", "uploadpack.allowAnySHA1InWant", "true")
	sha := upstream.pushPullRequest(work, 1, nil)
	tc := newTestCache(t, upstream.server.URL, nil)
	tc.clone(t, "repo")
	if err := fetchByID(t, tc.url("repo"), sha); err != nil {
		t.Fatalf("fetch by ID: %v", err)
	}
	if refs := tc.wantedRefs(t); len(refs) != 1 {
		t.Fatalf("wanted refs %v, want one", refs)
	}

	runGit(t, work, "push", "--quiet", "origin", "pr1:main")
	tc.clone(t, "repo")
	if refs := tc.wantedRefs(t); len(refs) != 0 {
		t.Errorf("wanted refs %v after the commit was merged, want none", refs)
	}
}

func TestOldWantedCommitIsUnpinned(t *testing.T) {
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	runGit(t, upstream.bare(), "config", "uploadpack.allowAnySHA1InWant", "true")
	old := upstream.pushPullRequest(work, 1, nil)
	recent := upstream.pushPullRequest(work, 2, nil)
	tc := newTestCache(t, upstream.server.URL, nil)
	tc.clone(t, "repo")
	for _, sha := range []string{old, recent} {
		if err := fetchByID(t, tc.url("repo"), sha); err != nil {
			t.Fatalf("fetch by ID: %v", err)
		}
	}

	cached := filepath.Join(tc.dir, "up", "repo.git")
	for _, ref := range tc.wantedRefs(t) {
		if strings.HasSuffix(ref, "/"+old) {
			runGit(t, cached, "update-ref", "-d", ref)
		}
	}
	runGit(t, cached, "update-ref", wantedRef(old, time.Now().Add(-wantedMaxAge-time.Hour)), old)

	tc.clone(t, "repo")
	refs := tc.wantedRefs(t)
	if len(refs) != 1 || !strings.HasSuffix(refs[0], "/"+recent) {
		t.Errorf("wanted refs %v, want only the recent commit's", refs)
	}
}

// TestRepositoryTooLargeToRefreshIsClonedAgain fills a repository with a
// commit fetched by ID, then grows its branch upstream past what is left of
// max_repo_size. Rather than stop following the upstream, the repository is
// dropped once idle and cloned again with only its branches and tags, which
// fit.
func TestRepositoryTooLargeToRefreshIsClonedAgain(t *testing.T) {
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	runGit(t, upstream.bare(), "config", "uploadpack.allowAnySHA1InWant", "true")
	pinned := upstream.pushPullRequest(work, 1, randomBytes(100<<10))
	base := repoSize(t, upstream, "repo")
	tc := newTestCache(t, upstream.server.URL, func(o *Options) { o.MaxRepoSize = base + 150<<10 })
	tc.clone(t, "repo")
	if err := fetchByID(t, tc.url("repo"), pinned); err != nil {
		t.Fatalf("fetch by ID: %v", err)
	}

	if err := os.WriteFile(filepath.Join(work, "grown.bin"), randomBytes(80<<10), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "grown.bin")
	head := upstream.commit(work, "grown")
	runGit(t, work, "push", "--quiet", "origin", "main")

	// This clone's refresh does not fit. It is served the cached copy, or,
	// if the repository is dropped between its requests, the new clone.
	tc.clone(t, "repo")
	if got := runGit(t, tc.clone(t, "repo"), "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD = %s, want %s once cloned again", got, head)
	}
	waitIdle(t, tc.Cache)
	if _, err := tryGit(t, filepath.Join(tc.dir, "up", "repo.git"), "cat-file", "-e", pinned); err == nil {
		t.Error("the repository still holds the commit fetched by ID, so it was not cloned again")
	}
}

// TestTooLargeWantKeepsRepository asks for a commit by ID that does not fit
// in what max_repo_size leaves. The commit is refused, for a while, and the
// repository is kept, with nothing of the cut-off transfer left in it.
func TestTooLargeWantKeepsRepository(t *testing.T) {
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	runGit(t, upstream.bare(), "config", "uploadpack.allowAnySHA1InWant", "true")
	large := upstream.pushPullRequest(work, 1, randomBytes(200<<10))
	small := upstream.pushPullRequest(work, 2, nil)
	base := repoSize(t, upstream, "repo")
	tc := newTestCache(t, upstream.server.URL, func(o *Options) { o.MaxRepoSize = base + 100<<10 })
	tc.clone(t, "repo")

	if err := fetchByID(t, tc.url("repo"), large); err == nil {
		t.Fatal("fetched a commit larger than max_repo_size leaves")
	}
	waitIdle(t, tc.Cache)
	cached := filepath.Join(tc.dir, "up", "repo.git")
	if !fileExists(cached) {
		t.Fatal("the repository was dropped")
	}
	if _, err := tryGit(t, cached, "cat-file", "-e", large); err == nil {
		t.Error("the cut-off transfer left the commit behind")
	}
	if leftovers, _ := filepath.Glob(filepath.Join(cached, "objects", "pack", "tmp_*")); len(leftovers) != 0 {
		t.Errorf("the cut-off transfer left %v", leftovers)
	}
	tc.mu.Lock()
	until := tc.failedWants["up/repo.git "+large]
	tc.mu.Unlock()
	if time.Until(until) <= wantRetryAfter {
		t.Errorf("the large commit is refused until %s, want for tooLargeRetryAfter", until)
	}

	before := upstream.gitRequests.Load()
	if err := fetchByID(t, tc.url("repo"), large); err == nil {
		t.Fatal("fetched the large commit on a second try")
	}
	if got := upstream.gitRequests.Load() - before; got != 0 {
		t.Errorf("the second try made %d upstream fetch requests, want none", got)
	}
	if err := fetchByID(t, tc.url("repo"), small); err != nil {
		t.Errorf("fetch of another commit by ID: %v", err)
	}
}

// TestWantsFetchedTogetherFailSeparately asks for a good and a bad commit
// in one request. The good one is still fetched, and only the bad one is
// refused for a while.
func TestWantsFetchedTogetherFailSeparately(t *testing.T) {
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	runGit(t, upstream.bare(), "config", "uploadpack.allowAnySHA1InWant", "true")
	good := upstream.pushPullRequest(work, 1, nil)
	bad := hex.EncodeToString(randomBytes(20))
	tc := newTestCache(t, upstream.server.URL, nil)
	tc.clone(t, "repo")

	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	if _, err := tryGit(t, dir, "fetch", "--quiet", tc.url("repo"), good, bad); err == nil {
		t.Fatal("fetched a commit the upstream does not have")
	}
	waitIdle(t, tc.Cache)
	if _, err := tryGit(t, filepath.Join(tc.dir, "up", "repo.git"), "cat-file", "-e", good); err != nil {
		t.Error("the good commit was not fetched")
	}
	tc.mu.Lock()
	_, goodRefused := tc.failedWants["up/repo.git "+good]
	_, badRefused := tc.failedWants["up/repo.git "+bad]
	tc.mu.Unlock()
	if goodRefused || !badRefused {
		t.Errorf("refused good=%v bad=%v, want only the bad commit", goodRefused, badRefused)
	}
}

func TestFailedWantIsNotRetriedAtOnce(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, nil)
	tc.clone(t, "repo")
	missing := hex.EncodeToString(randomBytes(20))

	before := upstream.gitRequests.Load()
	if err := fetchByID(t, tc.url("repo"), missing); err == nil {
		t.Fatal("fetched a commit the upstream does not have")
	}
	if upstream.gitRequests.Load() == before {
		t.Fatal("the first request did not ask the upstream")
	}
	before = upstream.gitRequests.Load()
	if err := fetchByID(t, tc.url("repo"), missing); err == nil {
		t.Fatal("fetched a commit the upstream does not have")
	}
	if got := upstream.gitRequests.Load() - before; got != 0 {
		t.Errorf("the second request made %d upstream fetch requests, want none", got)
	}
}

func TestWantsAreNotFetchedWhenTurnedOff(t *testing.T) {
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	runGit(t, upstream.bare(), "config", "uploadpack.allowAnySHA1InWant", "true")
	sha := upstream.pushPullRequest(work, 1, nil)
	tc := newTestCache(t, upstream.server.URL, nil)
	server := httptest.NewServer(testFront(tc.Cache, upstream.server.URL, false))
	defer server.Close()
	url := server.URL + "/git/up/repo"
	runGit(t, "", "clone", "--quiet", url, filepath.Join(t.TempDir(), "clone"))

	sessions := upstream.gitSessions.Load()
	if err := fetchByID(t, url, sha); err == nil {
		t.Error("fetched a commit by ID with FetchWants off")
	}
	if got := upstream.gitSessions.Load() - sessions; got != 0 {
		t.Errorf("upstream saw %d fetches, want none", got)
	}
}

// TestWantsDuringAnOutageAreNotFetchedOneByOne asks for two commits while
// the upstream is down. One failed fetch records both, rather than a fetch
// per commit, each waiting on the upstream.
func TestWantsDuringAnOutageAreNotFetchedOneByOne(t *testing.T) {
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	runGit(t, upstream.bare(), "config", "uploadpack.allowAnySHA1InWant", "true")
	first := upstream.pushPullRequest(work, 1, nil)
	second := upstream.pushPullRequest(work, 2, nil)
	tc := newTestCache(t, upstream.server.URL, nil)
	tc.clone(t, "repo")

	upstream.status.Store(http.StatusServiceUnavailable)
	sessions := upstream.gitSessions.Load()
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	if _, err := tryGit(t, dir, "fetch", "--quiet", tc.url("repo"), first, second); err == nil {
		t.Fatal("fetched commits while the upstream was down")
	}
	if got := upstream.gitSessions.Load() - sessions; got != 1 {
		t.Errorf("upstream saw %d fetches, want 1", got)
	}
	tc.mu.Lock()
	_, firstRefused := tc.failedWants["up/repo.git "+first]
	_, secondRefused := tc.failedWants["up/repo.git "+second]
	tc.mu.Unlock()
	if !firstRefused || !secondRefused {
		t.Errorf("refused first=%v second=%v, want both", firstRefused, secondRefused)
	}
}

// TestLargeBatchOfFailedWantsIsNotSplit asks for more unknown commits at
// once than are fetched again one by one, which would hold the repository's
// write lock for a fetch per commit.
func TestLargeBatchOfFailedWantsIsNotSplit(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, nil)
	tc.clone(t, "repo")

	args := []string{"fetch", "--quiet", tc.url("repo")}
	for range maxWantSplit + 1 {
		args = append(args, hex.EncodeToString(randomBytes(20)))
	}
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	sessions := upstream.gitSessions.Load()
	if _, err := tryGit(t, dir, args...); err == nil {
		t.Fatal("fetched commits the upstream does not have")
	}
	if got := upstream.gitSessions.Load() - sessions; got != 1 {
		t.Errorf("upstream saw %d fetches, want 1", got)
	}
}
