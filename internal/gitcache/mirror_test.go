package gitcache

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMirrorClonesRepository(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, nil)

	got, err := tc.Mirror(context.Background(), "up/repo.git", upstream.server.URL+"/repo", "", true)
	if err != nil {
		t.Fatalf("Mirror: %v", err)
	}
	if got.Cached || got.Bytes <= 0 {
		t.Errorf("first mirror = %+v, want fetched bytes", got)
	}
	if !strings.Contains(tc.cachedRefs(t), "refs/tags/v1") {
		t.Errorf("cached refs %q lack the tag", tc.cachedRefs(t))
	}

	got, err = tc.Mirror(context.Background(), "up/repo.git", upstream.server.URL+"/repo", "", true)
	if err != nil || !got.Cached {
		t.Errorf("second mirror = %+v, %v, want cached", got, err)
	}
}

func TestMirrorFetchesCommitOnNoBranch(t *testing.T) {
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	runGit(t, upstream.bare(), "config", "uploadpack.allowAnySHA1InWant", "true")
	sha := upstream.pushPullRequest(work, 1, nil)
	tc := newTestCache(t, upstream.server.URL, nil)

	got, err := tc.Mirror(context.Background(), "up/repo.git", upstream.server.URL+"/repo", sha, true)
	if err != nil {
		t.Fatalf("Mirror: %v", err)
	}
	if got.Cached {
		t.Errorf("mirror = %+v, want fetched", got)
	}
	if refs := tc.wantedRefs(t); len(refs) != 1 {
		t.Errorf("wanted refs %v, want one", refs)
	}

	sessions := upstream.gitSessions.Load()
	got, err = tc.Mirror(context.Background(), "up/repo.git", upstream.server.URL+"/repo", sha, true)
	if err != nil || !got.Cached {
		t.Errorf("second mirror = %+v, %v, want cached", got, err)
	}
	if n := upstream.gitSessions.Load() - sessions; n != 0 {
		t.Errorf("second mirror fetched %d times, want none", n)
	}
}

func TestMirrorFindsCommitOnABranch(t *testing.T) {
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	sha := upstream.commit(work, "second")
	runGit(t, work, "push", "--quiet", "origin", "main")
	tc := newTestCache(t, upstream.server.URL, nil)

	got, err := tc.Mirror(context.Background(), "up/repo.git", upstream.server.URL+"/repo", sha, false)
	if err != nil || got.Cached {
		t.Errorf("mirror = %+v, %v, want fetched", got, err)
	}
}

func TestMirrorDoesNotFetchByIDWhenTurnedOff(t *testing.T) {
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	runGit(t, upstream.bare(), "config", "uploadpack.allowAnySHA1InWant", "true")
	sha := upstream.pushPullRequest(work, 1, nil)
	tc := newTestCache(t, upstream.server.URL, nil)

	_, err := tc.Mirror(context.Background(), "up/repo.git", upstream.server.URL+"/repo", sha, false)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("mirror of a commit on no branch with fetching by ID off = %v, want ErrNotFound", err)
	}
	if refs := tc.wantedRefs(t); len(refs) != 0 {
		t.Errorf("wanted refs %v, want none", refs)
	}
}

func TestMirrorReportsMissingCommit(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, nil)

	_, err := tc.Mirror(context.Background(), "up/repo.git", upstream.server.URL+"/repo", strings.Repeat("ab", 20), true)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("mirror of a commit the upstream lacks = %v, want ErrNotFound", err)
	}
}

func TestMirrorRefusesInvalidCommit(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, nil)

	if _, err := tc.Mirror(context.Background(), "up/repo.git", upstream.server.URL+"/repo", "main", true); err == nil {
		t.Error("mirrored a branch name as a commit")
	}
}

func TestMirrorCountsRefreshThatFetched(t *testing.T) {
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, nil)
	if _, err := tc.Mirror(context.Background(), "up/repo.git", upstream.server.URL+"/repo", "", true); err != nil {
		t.Fatalf("Mirror: %v", err)
	}
	upstream.commit(work, "second")
	runGit(t, work, "push", "--quiet", "origin", "main")

	got, err := tc.Mirror(context.Background(), "up/repo.git", upstream.server.URL+"/repo", "", true)
	if err != nil {
		t.Fatalf("Mirror: %v", err)
	}
	if got.Cached || got.Bytes <= 0 {
		t.Errorf("mirror after a push = %+v, want fetched bytes", got)
	}
}

// TestMirrorRenewsWantedCommit checks mirroring a commit on no branch again
// makes its 7 days count from that run, not from the first fetch.
func TestMirrorRenewsWantedCommit(t *testing.T) {
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	runGit(t, upstream.bare(), "config", "uploadpack.allowAnySHA1InWant", "true")
	sha := upstream.pushPullRequest(work, 1, nil)
	tc := newTestCache(t, upstream.server.URL, nil)
	if _, err := tc.Mirror(context.Background(), "up/repo.git", upstream.server.URL+"/repo", sha, true); err != nil {
		t.Fatalf("Mirror: %v", err)
	}
	cached := filepath.Join(tc.dir, "up", "repo.git")
	for _, ref := range tc.wantedRefs(t) {
		runGit(t, cached, "update-ref", "-d", ref)
	}
	old := wantedRef(sha, time.Now().Add(-wantedMaxAge+time.Hour))
	runGit(t, cached, "update-ref", old, sha)

	start := time.Now().Add(-time.Second)
	got, err := tc.Mirror(context.Background(), "up/repo.git", upstream.server.URL+"/repo", sha, true)
	if err != nil || !got.Cached {
		t.Fatalf("second mirror = %+v, %v, want cached", got, err)
	}
	if !slices.ContainsFunc(tc.wantedRefs(t), func(ref string) bool {
		return ref != old && wantedRefTime(t, ref, sha).After(start)
	}) {
		t.Errorf("wanted refs %v, want one written by the second mirror", tc.wantedRefs(t))
	}
}

func TestMirrorDoesNotPinCommitOnABranch(t *testing.T) {
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	sha := upstream.commit(work, "second")
	runGit(t, work, "push", "--quiet", "origin", "main")
	tc := newTestCache(t, upstream.server.URL, nil)
	for range 2 {
		if _, err := tc.Mirror(context.Background(), "up/repo.git", upstream.server.URL+"/repo", sha, true); err != nil {
			t.Fatalf("Mirror: %v", err)
		}
	}
	if refs := tc.wantedRefs(t); len(refs) != 0 {
		t.Errorf("wanted refs %v, want none for a commit on a branch", refs)
	}
}

func wantedRefTime(t *testing.T, ref, oid string) time.Time {
	t.Helper()
	created, got, _ := strings.Cut(strings.TrimPrefix(ref, wantedPrefix), "/")
	unix, err := strconv.ParseInt(created, 10, 64)
	if err != nil || got != oid {
		return time.Time{}
	}
	return time.Unix(unix, 0)
}

// TestMirrorReportsRefreshTooLarge checks a commit that is cached still
// fails to mirror when the refresh doesn't fit in max_repo_size, since the
// repository is dropped once released.
func TestMirrorReportsRefreshTooLarge(t *testing.T) {
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	runGit(t, upstream.bare(), "config", "uploadpack.allowAnySHA1InWant", "true")
	pinned := upstream.pushPullRequest(work, 1, randomBytes(100<<10))
	base := repoSize(t, upstream, "repo")
	tc := newTestCache(t, upstream.server.URL, func(o *Options) { o.MaxRepoSize = base + 150<<10 })
	if _, err := tc.Mirror(context.Background(), "up/repo.git", upstream.server.URL+"/repo", pinned, true); err != nil {
		t.Fatalf("Mirror: %v", err)
	}
	if err := os.WriteFile(filepath.Join(work, "grown.bin"), randomBytes(80<<10), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "grown.bin")
	upstream.commit(work, "grown")
	runGit(t, work, "push", "--quiet", "origin", "main")

	_, err := tc.Mirror(context.Background(), "up/repo.git", upstream.server.URL+"/repo", pinned, true)
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("mirror with a refresh past max_repo_size = %v, want ErrTooLarge", err)
	}
}

func TestMirrorResolvesTag(t *testing.T) {
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	tagged := runGit(t, work, "rev-parse", "v1^{commit}")
	tc := newTestCache(t, upstream.server.URL, nil)

	got, err := tc.Mirror(context.Background(), "up/repo.git", upstream.server.URL+"/repo", "v1", true)
	if err != nil {
		t.Fatalf("Mirror: %v", err)
	}
	if got.Commit != tagged {
		t.Errorf("commit %q, want %s the tag points at", got.Commit, tagged)
	}
}

func TestMirrorReportsMissingTag(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, nil)

	for _, version := range []string{"v9", "main"} {
		_, err := tc.Mirror(context.Background(), "up/repo.git", upstream.server.URL+"/repo", version, true)
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("mirror at %q = %v, want ErrNotFound", version, err)
		}
	}
}

func TestMirrorRefusesInvalidTag(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, nil)

	for _, version := range []string{"a..b", "v1^{tree}", "v1~1", "v 1", "v1:x"} {
		_, err := tc.Mirror(context.Background(), "up/repo.git", upstream.server.URL+"/repo", version, true)
		if err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("mirror at %q = %v, want an invalid version error", version, err)
		}
	}
}

func TestMirrorListsSubmodules(t *testing.T) {
	upstream := newFakeUpstream(t)
	libWork := upstream.newRepo("lib")
	libCommit := runGit(t, libWork, "rev-parse", "HEAD")
	work := upstream.newRepo("repo")
	gitmodules := "[submodule \"vendor/lib\"]\n\tpath = vendor/lib\n\turl = ../lib.git\n" +
		"[submodule \"other\"]\n\tpath = third_party/other\n\turl = https://example.com/other.git\n" +
		"[submodule \"colon\"]\n\tpath = :colon\n\turl = ../lib.git\n"
	if err := os.WriteFile(filepath.Join(work, ".gitmodules"), []byte(gitmodules), 0o600); err != nil {
		t.Fatal(err)
	}
	otherCommit := strings.Repeat("ab", 20)
	runGit(t, work, "add", ".gitmodules")
	runGit(t, work, "update-index", "--add", "--cacheinfo", "160000,"+libCommit+",vendor/lib")
	runGit(t, work, "update-index", "--add", "--cacheinfo", "160000,"+otherCommit+",third_party/other")
	// Git for Windows refuses ':' in paths unless NTFS protection is off.
	runGit(t, work, "-c", "core.protectNTFS=false", "update-index", "--add", "--cacheinfo", "160000,"+libCommit+",:colon")
	runGit(t, work, "commit", "--quiet", "-m", "submodules")
	runGit(t, work, "push", "--quiet", "origin", "main")
	commit := runGit(t, work, "rev-parse", "HEAD")
	tc := newTestCache(t, upstream.server.URL, nil)

	got, err := tc.Mirror(context.Background(), "up/repo.git", upstream.server.URL+"/repo", commit, true)
	if err != nil {
		t.Fatalf("Mirror: %v", err)
	}
	want := []Submodule{
		{Path: ":colon", URL: "../lib.git", Commit: libCommit},
		{Path: "third_party/other", URL: "https://example.com/other.git", Commit: otherCommit},
		{Path: "vendor/lib", URL: "../lib.git", Commit: libCommit},
	}
	if !slices.Equal(got.Submodules, want) {
		t.Errorf("submodules %+v, want %+v", got.Submodules, want)
	}

	// Without a version, nothing names a commit to read submodules from.
	got, err = tc.Mirror(context.Background(), "up/repo.git", upstream.server.URL+"/repo", "", true)
	if err != nil || len(got.Submodules) != 0 {
		t.Errorf("unversioned mirror = %+v, %v, want no submodules", got, err)
	}
}

// TestMirrorWithEmptyGitmodules checks a .gitmodules with no path or url,
// for which git config --get-regexp finds nothing, lists no submodules.
func TestMirrorWithEmptyGitmodules(t *testing.T) {
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	for i, content := range []string{"", "[submodule \"x\"]\n\tbranch = main\n"} {
		if err := os.WriteFile(filepath.Join(work, ".gitmodules"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, work, "add", ".gitmodules")
		runGit(t, work, "commit", "--quiet", "--allow-empty", "-m", "gitmodules "+strconv.Itoa(i))
	}
	runGit(t, work, "push", "--quiet", "origin", "main")
	tc := newTestCache(t, upstream.server.URL, nil)

	for _, rev := range []string{"HEAD~1", "HEAD"} {
		commit := runGit(t, work, "rev-parse", rev)
		got, err := tc.Mirror(context.Background(), "up/repo.git", upstream.server.URL+"/repo", commit, true)
		if err != nil || len(got.Submodules) != 0 {
			t.Errorf("mirror of %s = %+v, %v, want no submodules", rev, got, err)
		}
	}
}

// TestSubmodulesReportsCancel checks a cancelled context is reported, not
// read as a commit without .gitmodules.
func TestSubmodulesReportsCancel(t *testing.T) {
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	commit := runGit(t, work, "rev-parse", "HEAD")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tc := newTestCache(t, upstream.server.URL, nil)

	if _, err := tc.submodules(ctx, "up/repo.git", work, commit); !errors.Is(err, context.Canceled) {
		t.Errorf("submodules with a cancelled context = %v, want context.Canceled", err)
	}
}

// TestMirrorSkipsBrokenGitmodules checks a .gitmodules git can't read, one
// with a syntax error or committed as a symlink, lists no submodules and
// logs a warning instead of failing the mirror.
func TestMirrorSkipsBrokenGitmodules(t *testing.T) {
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	gitmodules := filepath.Join(work, ".gitmodules")
	if err := os.WriteFile(gitmodules, []byte("[submodule \"x\"\n\tpath = x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", ".gitmodules")
	runGit(t, work, "commit", "--quiet", "-m", "broken gitmodules")
	if err := os.Remove(gitmodules); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../outside/.gitmodules", gitmodules); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", ".gitmodules")
	runGit(t, work, "commit", "--quiet", "-m", "symlinked gitmodules")
	runGit(t, work, "push", "--quiet", "origin", "main")
	tc := newTestCache(t, upstream.server.URL, nil)

	for _, rev := range []string{"HEAD~1", "HEAD"} {
		commit := runGit(t, work, "rev-parse", rev)
		got, err := tc.Mirror(context.Background(), "up/repo.git", upstream.server.URL+"/repo", commit, true)
		if err != nil || len(got.Submodules) != 0 {
			t.Errorf("mirror of %s = %+v, %v, want no submodules", rev, got, err)
		}
		if !strings.Contains(tc.logs.String(), "commit="+commit) {
			t.Errorf("no warning naming %s in the log", commit)
		}
	}
}

func TestMirrorAcceptsUppercaseCommit(t *testing.T) {
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	commit := runGit(t, work, "rev-parse", "HEAD")
	tc := newTestCache(t, upstream.server.URL, nil)

	got, err := tc.Mirror(context.Background(), "up/repo.git", upstream.server.URL+"/repo", strings.ToUpper(commit), true)
	if err != nil || got.Commit != commit {
		t.Errorf("mirror of the uppercase commit ID = %+v, %v, want commit %s", got, err, commit)
	}
}

func TestMirrorExplainsShortCommit(t *testing.T) {
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	commit := runGit(t, work, "rev-parse", "HEAD")
	tc := newTestCache(t, upstream.server.URL, nil)

	_, err := tc.Mirror(context.Background(), "up/repo.git", upstream.server.URL+"/repo", commit[:12], true)
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "full commit ID") {
		t.Errorf("mirror of a short commit ID = %v, want ErrNotFound saying it is no full commit ID", err)
	}
}

// TestMirrorWarnsOfStaleTag checks a tag served from the cache when the
// refresh fails is mirrored with a warning, since the tag may have moved.
func TestMirrorWarnsOfStaleTag(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, nil)
	if _, err := tc.Mirror(context.Background(), "up/repo.git", upstream.server.URL+"/repo", "v1", true); err != nil {
		t.Fatalf("Mirror: %v", err)
	}
	upstream.status.Store(http.StatusServiceUnavailable)

	if _, err := tc.Mirror(context.Background(), "up/repo.git", upstream.server.URL+"/repo", "v1", true); err != nil {
		t.Fatalf("mirror of a cached tag with the upstream down: %v", err)
	}
	if !strings.Contains(tc.logs.String(), "tag=v1") {
		t.Errorf("no warning naming the tag in the log:\n%s", tc.logs.String())
	}
}
