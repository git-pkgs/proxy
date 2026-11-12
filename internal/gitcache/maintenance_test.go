package gitcache

import (
	"os"
	"path/filepath"
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

func bitmapsOf(t *testing.T, bare string) []string {
	t.Helper()
	found, err := filepath.Glob(midxPath(t, bare) + "-*.bitmap")
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// TestMaintenanceWritesIndexes checks that a cloned repository gets the
// commit-graph and the multi-pack index with reachability bitmaps it is
// served from, kept up to date with every fetch that changes it.
func TestMaintenanceWritesIndexes(t *testing.T) {
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, func(o *Options) {
		o.Maintenance = true
		o.MaintenanceInterval = 0
	})
	tc.clone(t, "repo")
	waitIdle(t, tc.Cache)
	bare := bareRepo(tc)

	if !fileExists(filepath.Join(bare, "objects", "info", commitGraphsName, "commit-graph-chain")) {
		t.Error("the clone was left without a commit-graph")
	}
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
// most once per interval, while the commit-graph, which is incremental,
// still follows every fetch.
func TestMaintenanceInterval(t *testing.T) {
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
	graphs := filepath.Join(bare, "objects", "info", commitGraphsName)
	graphMtime := osStatMtime(t, graphs)

	upstream.commit(work, "second")
	runGit(t, work, "push", "--quiet", "origin", "main")
	tc.clone(t, "repo")
	waitIdle(t, tc.Cache)

	if after := midxMtime(t, bare); after != before {
		t.Error("the multi-pack index was rewritten within the interval")
	}
	rp := tc.repo("up/repo.git")
	if got, covered := rp.refsWritten.Load(), rp.maintainedAt.Load(); got != 2 || covered != 1 {
		t.Errorf("after the fetch, refsWritten %d, maintainedAt %d; the bitmap should have been skipped", got, covered)
	}
	if after := osStatMtime(t, graphs); !after.After(graphMtime) {
		t.Error("the commit-graph did not follow the fetch")
	}
}

// TestMaintenanceDisabled checks that a cache with maintenance off leaves
// the clone without the serving indexes.
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
	if fileExists(filepath.Join(bare, "objects", "info", commitGraphName)) ||
		fileExists(filepath.Join(bare, "objects", "info", commitGraphsName)) {
		t.Error("a commit-graph was written with maintenance off")
	}
	if rp := tc.repo("up/repo.git"); rp.maintainedAt.Load() != 0 {
		t.Error("a repository was maintained with maintenance off")
	}
}

// TestMaintenanceRecoversStaleLocks checks that the lock files a maintenance
// run killed mid-write leaves behind, which would block every later one,
// are removed by the next.
func TestMaintenanceRecoversStaleLocks(t *testing.T) {
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

	stale := []string{
		filepath.Join(bare, "objects", "pack", midxName+".lock"),
		filepath.Join(bare, "objects", "info", commitGraphName+".lock"),
		filepath.Join(bare, "objects", "info", commitGraphsName, "commit-graph-chain.lock"),
		filepath.Join(bare, "objects", "info", commitGraphsName, "tmp_graph_partial"),
	}
	for _, path := range stale {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	upstream.commit(work, "second")
	runGit(t, work, "push", "--quiet", "origin", "main")
	tc.clone(t, "repo")
	waitIdle(t, tc.Cache)

	for _, path := range stale {
		if fileExists(path) {
			t.Errorf("%s survived maintenance", filepath.Base(path))
		}
	}
	if after := midxMtime(t, bare); !after.After(before) {
		t.Error("maintenance did not rewrite the multi-pack index past the stale lock")
	}
	runGit(t, bare, "commit-graph", "verify")
}

// TestDirSizeSkipsIndexes checks that the serving indexes, rebuilt from the
// packs, are not counted as the repository's size.
func TestDirSizeSkipsIndexes(t *testing.T) {
	dir := t.TempDir()
	infoDir := filepath.Join(dir, "objects", "info")
	packDir := filepath.Join(dir, "objects", "pack")
	graphDir := filepath.Join(infoDir, commitGraphsName)
	for _, path := range []string{packDir, graphDir, filepath.Join(dir, "refs", "heads")} {
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
		{filepath.Join(infoDir, commitGraphName), 11, true},
		{filepath.Join(infoDir, commitGraphName+".lock"), 0, true},
		{filepath.Join(graphDir, "commit-graph-chain"), 3, true},
		{filepath.Join(graphDir, "graph-0.graph"), 13, true},
		{filepath.Join(graphDir, "tmp_graph_partial"), 4, true},
	}
	var want int64
	for _, f := range files {
		if err := os.WriteFile(f.path, make([]byte, f.size), 0o600); err != nil {
			t.Fatal(err)
		}
		if !f.index {
			want += f.size
		}
	}
	if got := dirSize(dir); got != want {
		t.Errorf("dirSize counted %d, want %d", got, want)
	}
}

func osStatMtime(t *testing.T, path string) time.Time {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime()
}
