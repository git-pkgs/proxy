package gitcache

import (
	"context"
	"errors"
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

// TestRepositoryStateKeepsAFailedRead checks a repository whose state can't
// be read isn't read again until its next upstream check.
func TestRepositoryStateKeepsAFailedRead(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, nil)
	tc.clone(t, "repo")
	waitIdle(t, tc.Cache)
	rp := tc.repo("up/repo.git")
	rp.state.Store(&cachedState{checked: rp.lastChecked.Load(), err: errors.New("unreadable")})

	if _, err := tc.RepositoryState(context.Background(), "up/repo.git"); err == nil || err.Error() != "unreadable" {
		t.Errorf("state = %v, want the kept error", err)
	}
	rp.lastChecked.Add(1)
	if _, err := tc.RepositoryState(context.Background(), "up/repo.git"); err != nil {
		t.Errorf("state after a new check = %v, want it read again", err)
	}
}
