package gitcache

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestRemoveDeletesARepository(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, nil)
	tc.clone(t, "repo")
	waitIdle(t, tc.Cache)
	dir := filepath.Join(tc.dir, "up", "repo.git")
	want := tc.repo("up/repo.git").size.Load()

	freed, err := tc.Remove("up/repo.git")
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if freed != want || freed == 0 {
		t.Errorf("freed %d bytes, want %d", freed, want)
	}
	if fileExists(dir) || tc.repo("up/repo.git") != nil {
		t.Error("the repository is still cached")
	}
	if len(tc.Repositories()) != 0 {
		t.Errorf("Repositories() = %+v, want none", tc.Repositories())
	}
	if _, err := tc.Remove("up/repo.git"); !errors.Is(err, ErrNotCached) {
		t.Errorf("second Remove: %v, want ErrNotCached", err)
	}

	// The next clone fetches it again.
	requests := upstream.gitRequests.Load()
	tc.clone(t, "repo")
	if upstream.gitRequests.Load() == requests || !fileExists(dir) {
		t.Error("the clone after Remove did not fetch the repository again")
	}
}

// A repository that a clone, a fetch or a client is using is not removed,
// so nothing reads it while it is deleted.
func TestRemoveRefusesARepositoryInUse(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, nil)
	tc.clone(t, "repo")
	waitIdle(t, tc.Cache)

	rp := tc.acquire("up/repo.git")
	if _, err := tc.Remove("up/repo.git"); !errors.Is(err, ErrInUse) {
		t.Errorf("Remove while in use: %v, want ErrInUse", err)
	}
	tc.release(rp)
	if !fileExists(filepath.Join(tc.dir, "up", "repo.git")) {
		t.Fatal("the repository was removed while in use")
	}
	if _, err := tc.Remove("up/repo.git"); err != nil {
		t.Errorf("Remove once idle: %v", err)
	}
}

func TestRemoveUnknownRepository(t *testing.T) {
	upstream := newFakeUpstream(t)
	tc := newTestCache(t, upstream.server.URL, nil)
	for _, key := range []string{"up/missing.git", "up/../up.git", ""} {
		if _, err := tc.Remove(key); !errors.Is(err, ErrNotCached) {
			t.Errorf("Remove(%q): %v, want ErrNotCached", key, err)
		}
	}
}
