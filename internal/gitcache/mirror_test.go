package gitcache

import (
	"context"
	"errors"
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
