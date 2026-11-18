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

// TestMaintenanceClearsLeftovers checks that a run removes what a run or
// fetch cut short leaves behind: the index's lock, which would block every
// later run, and temporary files. It also removes the commit-graph, which
// git does not read while maintenance is on.
func TestMaintenanceClearsLeftovers(t *testing.T) {
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

	packDir := filepath.Join(bare, "objects", "pack")
	graphs := filepath.Join(bare, "objects", "info", "commit-graphs")
	if err := os.MkdirAll(graphs, dirPermissions); err != nil {
		t.Fatal(err)
	}
	leftovers := []string{
		filepath.Join(packDir, midxName+".lock"),
		filepath.Join(packDir, "tmp_bitmap_abc"),
		filepath.Join(packDir, "tmp_rev_abc"),
		filepath.Join(bare, "objects", "info", "commit-graph"),
		filepath.Join(graphs, "commit-graph-chain"),
	}
	for _, path := range leftovers {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	upstream.commit(work, "second")
	runGit(t, work, "push", "--quiet", "origin", "main")
	tc.clone(t, "repo")
	waitIdle(t, tc.Cache)

	for _, path := range append(leftovers, graphs) {
		if fileExists(path) {
			t.Errorf("%s survived maintenance", path)
		}
	}
	if after := midxMtime(t, bare); !after.After(before) {
		t.Error("maintenance did not rewrite the multi-pack index past the stale lock")
	}
}

// TestMaintenanceRewritesBrokenIndex checks that a multi-pack index naming
// packs that are gone, as a gc running alongside a write leaves, which
// fails every write that builds on it, is written again from the packs.
func TestMaintenanceRewritesBrokenIndex(t *testing.T) {
	needGitReady(t)
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, func(o *Options) {
		o.Maintenance = true
		o.MaintenanceInterval = 0
	})
	tc.clone(t, "repo")
	// A second pack, so the repack below writes one under a new name.
	upstream.commit(work, "second")
	runGit(t, work, "push", "--quiet", "origin", "main")
	tc.clone(t, "repo")
	waitIdle(t, tc.Cache)
	bare := bareRepo(tc)
	indexFiles := func() []string {
		found, _ := filepath.Glob(midxPath(t, bare) + "*")
		return found
	}
	saved := map[string][]byte{}
	for _, path := range indexFiles() {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		saved[path] = data
	}
	// A repack deletes the packs the saved index names, and the index is
	// put back.
	runGit(t, bare, "repack", "-adq")
	for _, path := range indexFiles() {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	for path, data := range saved {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tryGit(t, bare, "multi-pack-index", "write", "--bitmap"); err == nil {
		t.Fatal("a write built on the stale index succeeded, so the index is not broken")
	}

	upstream.commit(work, "third")
	runGit(t, work, "push", "--quiet", "origin", "main")
	tc.clone(t, "repo")
	waitIdle(t, tc.Cache)

	rp := tc.repo("up/repo.git")
	if got, covered := rp.refsWritten.Load(), rp.maintainedAt.Load(); got != covered {
		t.Errorf("refsWritten %d, maintainedAt %d: the index was not written", got, covered)
	}
	if out, err := tryGit(t, bare, "multi-pack-index", "verify"); err != nil {
		t.Errorf("multi-pack-index verify: %v\n%s", err, out)
	}
	if len(bitmapsOf(t, bare)) == 0 {
		t.Error("the index was written again without bitmaps")
	}
}

// TestMaintenanceAfterInterval checks that a fetch whose run was skipped
// within the interval is indexed once the interval ends, with no request
// to set it off.
func TestMaintenanceAfterInterval(t *testing.T) {
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
	// The index looks written an hour ago, less two seconds: the next
	// fetch falls within the interval, which ends soon after.
	written := time.Now().Add(-time.Hour + 2*time.Second)
	if err := os.Chtimes(midxPath(t, bare), written, written); err != nil {
		t.Fatal(err)
	}

	upstream.commit(work, "second")
	runGit(t, work, "push", "--quiet", "origin", "main")
	tc.clone(t, "repo")
	waitFor(t, func() bool { return midxMtime(t, bare).After(written) })
	waitIdle(t, tc.Cache)
	rp := tc.repo("up/repo.git")
	if got, covered := rp.refsWritten.Load(), rp.maintainedAt.Load(); got != covered {
		t.Errorf("refsWritten %d, maintainedAt %d", got, covered)
	}
}

// TestFetchSkipsGCWhileIndexing checks that a fetch that starts while a run
// writes the index skips the gc that would otherwise end it, which could
// delete the packs the run reads, and that the fetches after the run do not.
func TestFetchSkipsGCWhileIndexing(t *testing.T) {
	needGitReady(t)
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	tc := newTestCache(t, upstream.server.URL, func(o *Options) {
		o.Maintenance = true
		// No run writes after the clone's, so only the test sets indexing.
		o.MaintenanceInterval = time.Hour
	})
	tc.clone(t, "repo")
	waitIdle(t, tc.Cache)
	bare := bareRepo(tc)
	// gc repacks once the repository has more than two packs.
	runGit(t, bare, "config", "gc.autoPackLimit", "2")
	packs := func() int {
		found, _ := filepath.Glob(filepath.Join(bare, "objects", "pack", "pack-*.pack"))
		return len(found)
	}
	fetch := func(msg string) {
		upstream.commit(work, msg)
		runGit(t, work, "push", "--quiet", "origin", "main")
		tc.clone(t, "repo")
		waitIdle(t, tc.Cache)
	}

	rp := tc.repo("up/repo.git")
	rp.indexing.Store(true)
	for _, msg := range []string{"second", "third", "fourth"} {
		fetch(msg)
	}
	if got := packs(); got < 4 {
		t.Fatalf("%d packs after four fetches while indexing, want one each: gc ran", got)
	}
	rp.indexing.Store(false)
	fetch("fifth")
	if got := packs(); got > 2 {
		t.Errorf("%d packs after a fetch with no run writing, want gc to repack them", got)
	}
}

// TestFailedFetchKeepsRunFiles checks that with maintenance on, a fetch cut
// off by max_repo_size removes the temporary pack it leaves, and not the
// temporary files a run may be writing beside it.
func TestFailedFetchKeepsRunFiles(t *testing.T) {
	needGitReady(t)
	upstream := newFakeUpstream(t)
	work := upstream.newRepo("repo")
	runGit(t, upstream.bare(), "config", "uploadpack.allowAnySHA1InWant", "true")
	large := upstream.pushPullRequest(work, 1, randomBytes(200<<10))
	base := repoSize(t, upstream, "repo")
	tc := newTestCache(t, upstream.server.URL, func(o *Options) {
		o.MaxRepoSize = base + 100<<10
		o.Maintenance = true
		// No run writes after the clone's, so none clears the file below.
		o.MaintenanceInterval = time.Hour
	})
	tc.clone(t, "repo")
	waitIdle(t, tc.Cache)
	packDir := filepath.Join(bareRepo(tc), "objects", "pack")
	running := filepath.Join(packDir, "tmp_bitmap_abc")
	if err := os.WriteFile(running, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := fetchByID(t, tc.url("repo"), large); err == nil {
		t.Fatal("fetched a commit larger than max_repo_size leaves")
	}
	waitIdle(t, tc.Cache)
	for _, pattern := range []string{"tmp_pack_*", "tmp_idx_*"} {
		if leftovers, _ := filepath.Glob(filepath.Join(packDir, pattern)); len(leftovers) != 0 {
			t.Errorf("the cut-off transfer left %v", leftovers)
		}
	}
	if !fileExists(running) {
		t.Error("the failed fetch removed a run's temporary file")
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
