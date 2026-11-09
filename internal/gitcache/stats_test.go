package gitcache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRepositoriesDescribesCachedRepositories(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, nil)
	if got := tc.Repositories(); len(got) != 0 {
		t.Fatalf("empty cache lists %+v", got)
	}

	start := time.Now().Add(-time.Second)
	tc.clone(t, "repo")
	tc.clone(t, "repo")
	waitIdle(t, tc.Cache)
	got := tc.Repositories()
	if len(got) != 1 {
		t.Fatalf("repositories %+v, want one", got)
	}
	r := got[0]
	if r.Key != "up/repo.git" || r.Size <= 0 || r.Requests != 2 || r.LastError != "" {
		t.Errorf("repository %+v, want up/repo.git with a size, 2 requests and no error", r)
	}
	if r.LastChecked.Before(start) || r.LastUsed.Before(start) {
		t.Errorf("checked %v, used %v, want both after %v", r.LastChecked, r.LastUsed, start)
	}

	// A refresh that fails is reported until one succeeds.
	upstream.server.Close()
	tc.clone(t, "repo")
	waitIdle(t, tc.Cache)
	r = tc.Repositories()[0]
	if r.LastError != "fetch_failed" || r.LastErrorAt.Before(start) || r.Requests != 3 {
		t.Errorf("after a failed refresh %+v, want fetch_failed and 3 requests", r)
	}
}

// TestRepositoriesListsRepositoriesFromDisk checks a cache opened over a
// cache_dir lists what is already there.
func TestRepositoriesListsRepositoriesFromDisk(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.newRepo("repo")
	dir := t.TempDir()
	first := newTestCache(t, upstream.server.URL, func(o *Options) { o.CacheDir = dir })
	first.clone(t, "repo")
	waitIdle(t, first.Cache)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second := newTestCache(t, upstream.server.URL, func(o *Options) { o.CacheDir = dir })
	got := second.Repositories()
	if len(got) != 1 || got[0].Key != "up/repo.git" || got[0].Size <= 0 || got[0].LastChecked.IsZero() || got[0].Requests != 0 {
		t.Errorf("repositories %+v, want up/repo.git with its size and last check", got)
	}
}

func TestRepositoryState(t *testing.T) {
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	head := runGit(t, work, "rev-parse", "HEAD")
	tc := newTestCache(t, upstream.server.URL, nil)
	tc.clone(t, "repo")
	waitIdle(t, tc.Cache)

	got, err := tc.RepositoryState(context.Background(), "up/repo.git")
	if err != nil {
		t.Fatalf("RepositoryState: %v", err)
	}
	if got.Branch != "main" || got.Commit != head || got.Subject != "first" || got.Branches != 1 || got.Tags != 1 ||
		time.Since(got.CommitTime) > time.Hour {
		t.Errorf("state %+v, want main at %s, first, 1 branch and 1 tag", got, head)
	}

	// The state follows a refresh that fetched.
	runGit(t, work, "checkout", "--quiet", "-b", "feature")
	runGit(t, work, "push", "--quiet", "origin", "feature")
	runGit(t, work, "checkout", "--quiet", "main")
	head = upstream.commit(work, "second")
	runGit(t, work, "push", "--quiet", "origin", "main")
	tc.clone(t, "repo")
	waitIdle(t, tc.Cache)
	if got, err = tc.RepositoryState(context.Background(), "up/repo.git"); err != nil || got.Commit != head || got.Subject != "second" || got.Branches != 2 {
		t.Errorf("after a push: %+v, %v; want main at %s, second, 2 branches", got, err, head)
	}

	if _, err := tc.RepositoryState(context.Background(), "up/missing.git"); !errors.Is(err, ErrNotFound) {
		t.Errorf("state of an uncached repository: %v, want ErrNotFound", err)
	}
}

// TestRepositoryStateRetriesAFailedRead checks a read that fails, here on
// a branch whose commit is missing, is reported as an error and read again
// a while later even if the upstream isn't checked in between.
// TestRepositoryStateFoldsCase checks that, where keys are folded, a
// repository's state is found under any spelling of its key.
func TestRepositoryStateFoldsCase(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, nil)
	tc.foldCase = true
	tc.clone(t, "repo")
	waitIdle(t, tc.Cache)

	if _, err := tc.RepositoryState(context.Background(), "up/REPO.git"); err != nil {
		t.Errorf("RepositoryState(up/REPO.git): %v", err)
	}
}

func TestRepositoryStateRetriesAFailedRead(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, nil)
	tc.clone(t, "repo")
	waitIdle(t, tc.Cache)
	rp := tc.repo("up/repo.git")
	ref := filepath.Join(rp.dir, "refs", "heads", "main")
	head := runGit(t, rp.dir, "rev-parse", "main")
	if err := os.WriteFile(ref, []byte(strings.Repeat("1", len(head))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if st, err := tc.RepositoryState(context.Background(), "up/repo.git"); err == nil {
		t.Fatalf("state %+v of a branch with no commit, want an error", st)
	}
	if err := os.WriteFile(ref, []byte(head+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := tc.RepositoryState(context.Background(), "up/repo.git"); err == nil {
		t.Error("failed read not kept for a while")
	}
	rp.state.Load().retryAt = time.Now().Add(-time.Second)
	if st, err := tc.RepositoryState(context.Background(), "up/repo.git"); err != nil || st.Commit != head {
		t.Errorf("state after the retry delay = %+v, %v; want %s", st, err, head)
	}
}

// TestReadStateOfAnEmptyRepository checks a repository with no commits has
// a state, not an error.
func TestReadStateOfAnEmptyRepository(t *testing.T) {
	upstream := newFakeUpstream(t)
	tc := newTestCache(t, upstream.server.URL, nil)
	dir := filepath.Join(t.TempDir(), "empty.git")
	runGit(t, "", "init", "--quiet", "--bare", "--initial-branch=main", dir)
	st, err := tc.readState(context.Background(), dir)
	if err != nil || st.Branch != "main" || st.Commit != "" || st.Branches != 0 {
		t.Errorf("state %+v, %v; want main with no commit", st, err)
	}
}
