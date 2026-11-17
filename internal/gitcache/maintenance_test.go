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
	if got, covered := rp.refsWritten.Load(), rp.maintainedAt.Load(); got != 2 || covered != 2 {
		t.Errorf("after the fetch, refsWritten %d, maintainedAt %d; a skipped bitmap still counts as maintained", got, covered)
	}
	if after := osStatMtime(t, graphs); !after.After(graphMtime) {
		t.Error("the commit-graph did not follow the fetch")
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

// TestServingIndexes checks that the serving indexes a repository is
// served from are found in either commit-graph layout, while stale
// leftovers and files that are not regular, say a directory, are left out.
func TestServingIndexes(t *testing.T) {
	dir := t.TempDir()
	infoDir := filepath.Join(dir, "objects", "info")
	packDir := filepath.Join(dir, "objects", "pack")
	graphDir := filepath.Join(infoDir, commitGraphsName)
	for _, path := range []string{packDir, graphDir} {
		if err := os.MkdirAll(path, dirPermissions); err != nil {
			t.Fatal(err)
		}
	}
	touched := []string{
		filepath.Join(infoDir, commitGraphName),
		filepath.Join(graphDir, "commit-graph-chain"),
		filepath.Join(packDir, midxName),
		filepath.Join(graphDir, "graph-0.graph"),
		filepath.Join(graphDir, "graph-1.graph"),
		filepath.Join(packDir, midxName+"-1.bitmap"),
		filepath.Join(packDir, midxName+"-1.rev"),
	}
	for _, path := range touched {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(graphDir, "tmp_graph_partial"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	want := make([]string, len(touched))
	copy(want, touched)
	if got := servingIndexes(dir); !slices.Equal(got, want) {
		t.Errorf("serving indexes listed %v, want %v", got, want)
	}

	if got := servingIndexes(t.TempDir()); got != nil {
		t.Errorf("an empty repository listed %v", got)
	}
	if err := os.Remove(touched[0]); err != nil {
		t.Fatal(err)
	}
	if got := servingIndexes(dir); slices.Contains(got, touched[0]) {
		t.Error("a missing commit-graph was listed")
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

// TestMaintenancePreloadsSkippedBitmaps checks that the read-back covers a
// run that skips the bitmaps, within their maintenance interval: the
// commit-graph it still writes pays off on the negotiation of the next
// fetch just the same.
func TestMaintenancePreloadsSkippedBitmaps(t *testing.T) {
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
	tc.logs.Reset()

	upstream.commit(work, "second")
	runGit(t, work, "push", "--quiet", "origin", "main")
	tc.clone(t, "repo")
	waitIdle(t, tc.Cache)

	if after := midxMtime(t, bare); after != before {
		t.Error("the multi-pack index was rewritten within the interval")
	}
	rp := tc.repo("up/repo.git")
	if got, covered := rp.refsWritten.Load(), rp.maintainedAt.Load(); got != 2 || covered != 2 {
		t.Fatalf("after the fetch, refsWritten %d, maintainedAt %d; a skipped bitmap still counts as maintained", got, covered)
	}
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

func osStatMtime(t *testing.T, path string) time.Time {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime()
}
