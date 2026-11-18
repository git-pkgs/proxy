package gitcache

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
// delete the packs the run reads, and that the next run runs that gc
// before writing an index over what it leaves.
func TestFetchSkipsGCWhileIndexing(t *testing.T) {
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
	// gc repacks once the repository has more than two packs.
	runGit(t, bare, "config", "gc.autoPackLimit", "2")
	packs := func() int {
		found, _ := filepath.Glob(filepath.Join(bare, "objects", "pack", "pack-*.pack"))
		return len(found)
	}

	// A run writing the index, as far as the fetches can tell. The runs
	// they ask for wait, as every slot for a run is taken.
	rp := tc.repo("up/repo.git")
	rp.indexing.Store(true)
	for range maxConcurrentMaintains {
		tc.maintains <- struct{}{}
	}
	for _, msg := range []string{"second", "third", "fourth"} {
		upstream.commit(work, msg)
		runGit(t, work, "push", "--quiet", "origin", "main")
		// Served once the fetch, and any gc ending it, is done.
		tc.clone(t, "repo")
	}
	if got := packs(); got < 4 {
		t.Fatalf("%d packs after four fetches while indexing, want one each: gc ran", got)
	}

	// The write ends, and the runs the fetches asked for go ahead.
	rp.indexing.Store(false)
	for range maxConcurrentMaintains {
		<-tc.maintains
	}
	waitIdle(t, tc.Cache)
	if got := packs(); got > 2 {
		t.Errorf("%d packs after the run, want the gc the fetches skipped to have repacked them", got)
	}
	if due, _ := tc.indexDue(rp); due {
		t.Error("the run left the index short of the packs the gc left")
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
		// A run's files while it writes.
		{filepath.Join(packDir, "tmp_bitmap_abc"), 11, true},
		{filepath.Join(packDir, "tmp_rev_abc"), 3, true},
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

// packedRepo returns a bare repository holding n packs, one per commit,
// fetched as the cache fetches, with no index.
func packedRepo(t *testing.T, n int) string {
	t.Helper()
	work := t.TempDir()
	runGit(t, work, "init", "--quiet", "--initial-branch", "main")
	bare := t.TempDir()
	runGit(t, bare, "init", "--quiet", "--bare")
	for i := range n {
		if err := os.WriteFile(filepath.Join(work, "f"), []byte(strconv.Itoa(i)), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, work, "add", "f")
		runGit(t, work, "commit", "--quiet", "-m", strconv.Itoa(i))
		runGit(t, bare, "-c", "transfer.unpackLimit=1", "fetch", "--quiet", "--no-auto-maintenance",
			work, "main:refs/heads/main")
	}
	return bare
}

// packIndexes returns the names of the index files of the packs in bare.
func packIndexes(t *testing.T, bare string) []string {
	t.Helper()
	found, _ := filepath.Glob(filepath.Join(bare, "objects", "pack", "pack-*.idx"))
	for i, path := range found {
		found[i] = filepath.Base(path)
	}
	return found
}

// TestIndexedPacks checks that the packs read from a multi-pack index git
// wrote are the ones it covers, and that a file that is not one, or is cut
// short, is refused rather than read as covering nothing.
func TestIndexedPacks(t *testing.T) {
	needGitReady(t)
	bare := packedRepo(t, 3)
	runGit(t, bare, "multi-pack-index", "write", "--bitmap")
	midx := midxPath(t, bare)
	got, err := indexedPacks(midx)
	if err != nil {
		t.Fatal(err)
	}
	want := packIndexes(t, bare)
	if len(got) != len(want) {
		t.Fatalf("read %v, want %v", got, want)
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("read %v, without %s", got, name)
		}
	}

	data, err := os.ReadFile(midx)
	if err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(t.TempDir(), midxName)
	for name, content := range map[string][]byte{
		"empty":                   nil,
		"another file":            []byte("PACK\x00\x00\x00\x02\x00\x00\x00\x00"),
		"cut in the header":       data[:8],
		"cut in the chunk table":  data[:20],
		"cut in the pack names":   data[:100],
		"a pack count off by one": append(append(slices.Clone(data[:8]), 0, 0, 0, 4), data[12:]...),
	} {
		if err := os.WriteFile(broken, content, 0o600); err != nil {
			t.Fatal(err)
		}
		if packs, err := indexedPacks(broken); err == nil {
			t.Errorf("%s: read %v", name, packs)
		}
	}
}

// TestIndexDue checks when the serving index is due to be written: when
// it is missing, has no bitmaps, cannot be read or names a pack that is
// gone, and when it leaves packs out once the interval has passed, but
// never while it covers every pack.
func TestIndexDue(t *testing.T) {
	needGitReady(t)
	c := &Cache{opts: Options{MaintenanceInterval: time.Hour}}
	check := func(name string, rp *repo, wantDue, wantWait bool) {
		t.Helper()
		due, wait := c.indexDue(rp)
		if due != wantDue || (wait > 0) != wantWait {
			t.Errorf("%s: due %v, wait %v; want due %v, a wait %v", name, due, wait, wantDue, wantWait)
		}
	}
	setUp := func(index ...string) (*repo, string) {
		bare := packedRepo(t, 2)
		if len(index) > 0 {
			runGit(t, bare, append([]string{"multi-pack-index", "write"}, index...)...)
		}
		return &repo{dir: bare}, bare
	}

	check("no packs", &repo{dir: t.TempDir()}, false, false)
	rp, _ := setUp()
	check("no index", rp, true, false)
	rp, _ = setUp("--no-bitmap")
	check("no bitmaps", rp, true, false)
	rp, bare := setUp("--bitmap")
	check("covers every pack", rp, false, false)

	// A pack it leaves out: due once the interval has passed.
	work := t.TempDir()
	runGit(t, work, "init", "--quiet", "--initial-branch", "main")
	runGit(t, work, "commit", "--quiet", "--allow-empty", "-m", "unrelated")
	runGit(t, bare, "-c", "transfer.unpackLimit=1", "fetch", "--quiet", "--no-auto-maintenance",
		work, "main:refs/heads/other")
	check("leaves a pack out, within the interval", rp, false, true)
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(midxPath(t, bare), old, old); err != nil {
		t.Fatal(err)
	}
	check("leaves a pack out, after the interval", rp, true, false)

	// A pack it names is gone.
	rp, bare = setUp("--bitmap")
	gone := strings.TrimSuffix(packIndexes(t, bare)[0], ".idx")
	matches, _ := filepath.Glob(filepath.Join(bare, "objects", "pack", gone+".*"))
	for _, path := range matches {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	check("names a pack that is gone", rp, true, false)

	rp, bare = setUp("--bitmap")
	if err := os.WriteFile(midxPath(t, bare), []byte("not an index"), 0o600); err != nil {
		t.Fatal(err)
	}
	check("cannot be read", rp, true, false)
}

// TestMaintenanceLeavesCoveringIndex checks that a fetch that changes the
// refs but brings no pack does not rewrite an index that covers every
// pack, with no interval to hold it back.
func TestMaintenanceLeavesCoveringIndex(t *testing.T) {
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
	packs := packIndexes(t, bare)

	// A new branch at a commit the cache holds.
	runGit(t, work, "push", "--quiet", "origin", "main:refs/heads/other")
	tc.clone(t, "repo")
	waitIdle(t, tc.Cache)

	if got := packIndexes(t, bare); len(got) != len(packs) {
		t.Fatalf("the fetch brought a pack: %v, before %v", got, packs)
	}
	// git would leave such an index as it is too; no run gets that far.
	if writes := strings.Count(tc.logs.String(), "wrote the serving index"); writes != 1 {
		t.Errorf("%d index writes, want the clone's alone", writes)
	}
	if after := midxMtime(t, bare); !after.Equal(before) {
		t.Error("an index that covers every pack was rewritten")
	}
	rp := tc.repo("up/repo.git")
	if got, covered := rp.refsWritten.Load(), rp.maintainedAt.Load(); got != covered {
		t.Errorf("refsWritten %d, maintainedAt %d", got, covered)
	}
}

// TestMaintenanceChecksIndexAfterRestart checks that a repository found on
// disk gets its index on its first request, though its upstream has not
// changed: one cached before maintenance was on, here.
func TestMaintenanceChecksIndexAfterRestart(t *testing.T) {
	needGitReady(t)
	upstream := newFakeUpstream(t)
	upstream.newRepo("repo")
	dir := t.TempDir()
	first := newTestCache(t, upstream.server.URL, func(o *Options) { o.CacheDir = dir })
	first.clone(t, "repo")
	waitIdle(t, first.Cache)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	bare := bareRepo(first)
	if fileExists(midxPath(t, bare)) {
		t.Fatal("an index was written with maintenance off")
	}

	second := newTestCache(t, upstream.server.URL, func(o *Options) {
		o.CacheDir = dir
		o.Maintenance = true
	})
	second.clone(t, "repo")
	waitIdle(t, second.Cache)
	if !fileExists(midxPath(t, bare)) || len(bitmapsOf(t, bare)) == 0 {
		t.Error("the repository found on disk was left without an index")
	}
}
