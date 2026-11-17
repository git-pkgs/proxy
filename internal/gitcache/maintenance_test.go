package gitcache

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
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

// assertNoCommitGraph fails the test if the repository at bare has a
// commit-graph, in either layout: with the bitmaps it slows fetches down.
func assertNoCommitGraph(t *testing.T, bare string) {
	t.Helper()
	for _, name := range []string{"commit-graph", "commit-graphs"} {
		if fileExists(filepath.Join(bare, "objects", "info", name)) {
			t.Errorf("the repository has a %s", name)
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
// date with every fetch that changes it, and no commit-graph.
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

	assertNoCommitGraph(t, bare)
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
// most once per interval, and that a run skipped within it is not repeated
// by every request after.
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

	// A request that changes nothing runs no maintenance, the skipped
	// bitmap included.
	tc.logs.Reset()
	tc.clone(t, "repo")
	waitIdle(t, tc.Cache)
	if strings.Contains(tc.logs.String(), "serving indexes") {
		t.Error("an unchanged request ran maintenance")
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
	assertNoCommitGraph(t, bare)
	if rp := tc.repo("up/repo.git"); rp.maintainedAt.Load() != 0 {
		t.Error("a repository was maintained with maintenance off")
	}
}

// TestMaintenanceRecoversStaleLocks checks that the lock file a maintenance
// run killed mid-write leaves behind, which would block every later one,
// is removed by the next.
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

// TestDirSizeSkipsIndexes checks that the serving indexes, rebuilt from the
// packs, are not counted as the repository's size.
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

// TestServingIndexes checks that the files a repository is served from are
// listed, and nothing else.
func TestServingIndexes(t *testing.T) {
	dir := t.TempDir()
	packDir := filepath.Join(dir, "objects", "pack")
	if err := os.MkdirAll(packDir, dirPermissions); err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(packDir, midxName),
		filepath.Join(packDir, midxName+"-1.bitmap"),
		filepath.Join(packDir, midxName+"-1.rev"),
	}
	for _, path := range append(want, filepath.Join(packDir, midxName+".lock"), filepath.Join(packDir, "pack-1.idx")) {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := servingIndexes(dir); !slices.Equal(got, want) {
		t.Errorf("serving indexes listed %v, want %v", got, want)
	}
	if got := servingIndexes(t.TempDir()); got != nil {
		t.Errorf("an empty repository listed %v", got)
	}
}

// TestMaintenancePreloadsIndexes checks that the indexes written by the
// maintenance run are read back, so that a filesystem that does not keep
// them in the page cache does not have the next fetch pay for reading
// them.
func TestMaintenancePreloadsIndexes(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, func(o *Options) {
		o.Maintenance = true
		o.MaintenanceInterval = 0
	})
	// The clone maintenance writes the indexes and reads them back.
	tc.clone(t, "repo")
	waitIdle(t, tc.Cache)
	assertReadBack(t, tc, "up/repo.git")
}

// TestNewPreloadsCachedRepositories checks that the serving indexes of
// the repositories loaded at startup are read back, and that Close waits
// for that.
func TestNewPreloadsCachedRepositories(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, func(o *Options) { o.Maintenance = true })
	tc.clone(t, "repo")
	waitIdle(t, tc.Cache)
	if err := tc.Close(); err != nil {
		t.Fatal(err)
	}

	logs := &lockedBuffer{}
	reloaded, err := New(Options{
		Upstreams:   map[string]string{"up": "up"},
		CacheDir:    tc.dir,
		StaleMaxAge: time.Hour,
		Client:      &http.Client{},
		Logger:      slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Maintenance: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Close does not return before the reading has, so nothing races the
	// checks below.
	if err := reloaded.Close(); err != nil {
		t.Fatal(err)
	}

	want := "reading the serving indexes of the cached repositories"
	if !strings.Contains(logs.String(), want) {
		t.Errorf("the serving indexes were not read at startup: %s", logs.String())
	}

	// With maintenance turned off, no reading happens at startup.
	plain := newTestCache(t, upstream.server.URL, nil)
	plain.clone(t, "repo")
	waitIdle(t, plain.Cache)
	if err := plain.Close(); err != nil {
		t.Fatal(err)
	}
	logs.Reset()
	reloaded, err = New(Options{
		Upstreams:   map[string]string{"up": "up"},
		CacheDir:    plain.dir,
		StaleMaxAge: time.Hour,
		Client:      &http.Client{},
		Logger:      slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := reloaded.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), want) {
		t.Error("the serving indexes were read at startup with maintenance off")
	}
}

// assertReadBack checks that the serving indexes of the repository cached
// under key have just been read into the page cache.
func assertReadBack(t *testing.T, tc *testCache, key string) {
	t.Helper()
	want := "read the serving indexes into the page cache"
	waitFor(t, func() bool {
		return strings.Contains(tc.logs.String(), want) && strings.Contains(tc.logs.String(), "repository="+key)
	})
}

// TestMaintenancePreloadsPageCache checks that the read-back genuinely
// brings the serving indexes into the page cache. It drops this process's
// copy of them first, and counts what comes back through the kernel; the
// pages it lost are the proof that the reading ran, and the ones that
// outstayed the dropping do not spoil it. Elsewhere there is no way to
// look at the page cache, so it does not run.
func TestMaintenancePreloadsPageCache(t *testing.T) {
	upstream := newFakeUpstream(t)
	upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, func(o *Options) {
		o.Maintenance = true
		// The bitmaps are written on the clone and fresh, so the run below
		// skips them: it still reads the indexes, and in place, which is
		// what the counts look at.
		o.MaintenanceInterval = time.Hour
	})
	// The clone leaves the indexes written; opening them does not read them.
	tc.clone(t, "repo")
	waitIdle(t, tc.Cache)
	indexes := repoIndexFiles(t, bareRepo(tc))
	if len(indexes) == 0 {
		t.Fatal("the clone left no serving indexes")
	}
	total := pageCount(indexes)
	if err := evictPageCache(indexes); err != nil {
		t.Skipf("dropping the page cache is not supported here: %v", err)
	}
	before := residentCount(indexes)
	if before >= total {
		t.Skipf("the kernel would not give up any page: %d of %d left", before, total)
	}

	// A fetch that changes nothing runs no maintenance, so the reading has
	// to run by hand, just as maintenance runs it.
	rp := tc.repo("up/repo.git")
	tc.maintain(rp)
	if after := residentCount(indexes); after != total {
		t.Errorf("the serving indexes were read back with %d of %d pages resident", after, total)
	}
}

// repoIndexFiles opens a connection to every file the bare repository at
// dir is served from, and to nothing else, so a test can see whether the
// cache has read them through the kernel.
func repoIndexFiles(t *testing.T, dir string) []*os.File {
	t.Helper()
	var conns []*os.File
	for _, path := range servingIndexes(dir) {
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		conns = append(conns, file)
		t.Cleanup(func() { _ = file.Close() })
	}
	return conns
}
