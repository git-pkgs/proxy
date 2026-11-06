package gitcache

import (
	"context"
	"errors"
	"strings"
	"testing"
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
