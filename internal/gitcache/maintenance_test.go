package gitcache

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func midxPath(t *testing.T, bare string) string {
	t.Helper()
	return filepath.Join(bare, "objects", "pack", midxName)
}

func midxMtime(t *testing.T, bare string) time.Time {
	t.Helper()
	info, err := os.Stat(midxPath(t, bare))
	if err != nil {
		t.Fatalf("multi-pack index: %v", err)
	}
	return info.ModTime()
}

// needGitReady skips the test when git is too old for maintenance.
func needGitReady(t *testing.T) {
	t.Helper()
	if !gitReady(runGit(t, "", "version")) {
		t.Skip("git is older than 2.56")
	}
}

// assertMaintenanceConfig fails the test unless the git processes of tc see
// every setting of maintenanceConfig in the repository at bare when want
// is set, and none of them when it is not.
func assertMaintenanceConfig(t *testing.T, tc *testCache, bare string, want bool) {
	t.Helper()
	for _, kv := range maintenanceConfig {
		out, err := tc.command(context.Background(), nil, "-C", bare, "config", "--get", kv[0]).Output()
		got, set := strings.TrimSpace(string(out)), err == nil
		switch {
		case want && got != kv[1]:
			t.Errorf("%s is %q, want %q", kv[0], got, kv[1])
		case !want && set:
			t.Errorf("%s is set to %q with maintenance off", kv[0], got)
		}
	}
}

func bitmapsOf(t *testing.T, bare string) []string {
	t.Helper()
	found, err := filepath.Glob(midxPath(t, bare) + "-*.bitmap")
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// TestMaintenanceWritesIndexes checks that a cloned repository gets the
// multi-pack index with reachability bitmaps it is served from, kept up to
// date with every fetch that changes it, with the git settings that keep
// it working.
func TestMaintenanceWritesIndexes(t *testing.T) {
	needGitReady(t)
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, func(o *Options) {
		o.Maintenance = true
		o.MaintenanceInterval = 0
	})
	tc.clone(t, "repo")
	waitIdle(t, tc.Cache)
	bare := bareRepo(tc)

	assertMaintenanceConfig(t, tc, bare, true)
	if !fileExists(midxPath(t, bare)) {
		t.Fatal("the clone was left without a multi-pack index")
	}
	if len(bitmapsOf(t, bare)) == 0 {
		t.Error("the multi-pack index was written without reachability bitmaps")
	}
	rp := tc.repo("up/repo.git")
	if got, covered := rp.refsWritten.Load(), rp.maintainedAt.Load(); got != 1 || covered != 1 {
		t.Errorf("after the clone, refsWritten %d, maintainedAt %d", got, covered)
	}

	before := midxMtime(t, bare)
	upstream.commit(work, "second")
	runGit(t, work, "push", "--quiet", "origin", "main")
	tc.clone(t, "repo")
	waitIdle(t, tc.Cache)
	if after := midxMtime(t, bare); !after.After(before) {
		t.Error("a fetch that changed the repository did not rewrite the multi-pack index")
	}
	rp = tc.repo("up/repo.git")
	if got, covered := rp.refsWritten.Load(), rp.maintainedAt.Load(); got != 2 || covered != 2 {
		t.Errorf("after the fetch, refsWritten %d, maintainedAt %d", got, covered)
	}
}

// TestMaintenanceInterval checks that the multi-pack index is rewritten at
// most once per interval, and that a run skipped within it still counts as
// done, so the requests after it do not run maintenance again.
func TestMaintenanceInterval(t *testing.T) {
	needGitReady(t)
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, func(o *Options) {
		o.Maintenance = true
		o.MaintenanceInterval = time.Hour
	})
	tc.clone(t, "repo")
	waitIdle(t, tc.Cache)
	bare := bareRepo(tc)
	before := midxMtime(t, bare)

	upstream.commit(work, "second")
	runGit(t, work, "push", "--quiet", "origin", "main")
	tc.clone(t, "repo")
	waitIdle(t, tc.Cache)

	if after := midxMtime(t, bare); after != before {
		t.Error("the multi-pack index was rewritten within the interval")
	}
	rp := tc.repo("up/repo.git")
	if got, covered := rp.refsWritten.Load(), rp.maintainedAt.Load(); got != 2 || covered != 2 {
		t.Errorf("after the fetch, refsWritten %d, maintainedAt %d; a skipped bitmap still counts as maintained", got, covered)
	}
}

// TestMaintenanceDisabled checks that a cache with maintenance off leaves
// the clone without the serving indexes, and runs git as it did before
// maintenance existed.
func TestMaintenanceDisabled(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, nil)
	tc.clone(t, "repo")
	waitIdle(t, tc.Cache)
	bare := bareRepo(tc)
	if fileExists(midxPath(t, bare)) {
		t.Error("a multi-pack index was written with maintenance off")
	}
	assertMaintenanceConfig(t, tc, bare, false)
	if rp := tc.repo("up/repo.git"); rp.maintainedAt.Load() != 0 {
		t.Error("a repository was maintained with maintenance off")
	}
}

// TestMaintenanceIsNotAUse checks that a maintenance run does not record
// the repository as used, which would keep one no client asks for warm and
// ahead in the eviction order.
func TestMaintenanceIsNotAUse(t *testing.T) {
	needGitReady(t)
	upstream := newFakeUpstream(t)
	upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, func(o *Options) {
		o.Maintenance = true
		o.MaintenanceInterval = 0
	})
	tc.clone(t, "repo")
	waitIdle(t, tc.Cache)

	rp := tc.repo("up/repo.git")
	const used = 1
	rp.lastUsed.Store(used)
	rp.refsWritten.Add(1)
	tc.scheduleMaintain(rp)
	waitIdle(t, tc.Cache)

	if got, covered := rp.refsWritten.Load(), rp.maintainedAt.Load(); got != covered {
		t.Fatalf("maintenance did not run: refsWritten %d, maintainedAt %d", got, covered)
	}
	if got := rp.lastUsed.Load(); got != used {
		t.Errorf("maintenance recorded a use: lastUsed %d", got)
	}
}

// TestMaintenanceRecoversStaleLocks checks that the lock file a maintenance
// run killed mid-write leaves behind, which would block every later one,
// is removed by the next.
func TestMaintenanceRecoversStaleLocks(t *testing.T) {
	needGitReady(t)
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, func(o *Options) {
		o.Maintenance = true
		o.MaintenanceInterval = 0
	})
	tc.clone(t, "repo")
	waitIdle(t, tc.Cache)
	bare := bareRepo(tc)
	before := midxMtime(t, bare)

	lock := filepath.Join(bare, "objects", "pack", midxName+".lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	upstream.commit(work, "second")
	runGit(t, work, "push", "--quiet", "origin", "main")
	tc.clone(t, "repo")
	waitIdle(t, tc.Cache)

	if fileExists(lock) {
		t.Error("the stale lock survived maintenance")
	}
	if after := midxMtime(t, bare); !after.After(before) {
		t.Error("maintenance did not rewrite the multi-pack index past the stale lock")
	}
}

// TestDirSizeSkipsIndexes checks that while maintenance is on, the serving
// indexes, rebuilt from the packs, are not counted as the repository's
// size, and that with it off, every file is, as before maintenance existed.
func TestDirSizeSkipsIndexes(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "objects", "pack")
	for _, path := range []string{packDir, filepath.Join(dir, "refs", "heads")} {
		if err := os.MkdirAll(path, dirPermissions); err != nil {
			t.Fatal(err)
		}
	}
	files := []struct {
		path  string
		size  int64
		index bool
	}{
		{filepath.Join(dir, "HEAD"), 23, false},
		{filepath.Join(dir, "refs", "heads", "main"), 41, false},
		{filepath.Join(packDir, "pack-abc.pack"), 100, false},
		{filepath.Join(packDir, "pack-abc.idx"), 10, false},
		{filepath.Join(packDir, "pack-abc.bitmap"), 5, true},
		{filepath.Join(packDir, midxName), 7, true},
		{filepath.Join(packDir, midxName+"-1.bitmap"), 9, true},
		{filepath.Join(packDir, midxName+".lock"), 0, true},
	}
	var want, all int64
	for _, f := range files {
		if err := os.WriteFile(f.path, make([]byte, f.size), 0o600); err != nil {
			t.Fatal(err)
		}
		all += f.size
		if !f.index {
			want += f.size
		}
	}
	maintained := &Cache{opts: Options{Maintenance: true}}
	if got := maintained.dirSize(dir); got != want {
		t.Errorf("with maintenance, dirSize counted %d, want %d", got, want)
	}
	if got := (&Cache{}).dirSize(dir); got != all {
		t.Errorf("without maintenance, dirSize counted %d, want %d", got, all)
	}
}
