package gitcache

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// midxName names the serving index maintenance writes in objects/pack,
// with reachability bitmaps beside it.
const midxName = "multi-pack-index"

// maintainTimeout bounds one maintenance run, so a repository whose index
// cannot be written, on a full disk say, does not keep the fetches into it
// from running gc for long.
const maintainTimeout = 30 * time.Minute

// maxConcurrentMaintains caps the repositories being maintained at once.
// Each run is a git process walking a repository's objects; with more,
// together they could crowd the upload-packs serving from it.
const maxConcurrentMaintains = 2

// scheduleMaintain brings the serving indexes of the repository up to date
// with what its last fetch left, in the background. It is called after
// every fetch that changed the repository; a run is skipped when the
// indexes already cover the last fetch, and while one runs the next waits
// for it. Close stops and waits for it.
func (c *Cache) scheduleMaintain(rp *repo) {
	if !c.opts.Maintenance || rp.refsWritten.Load() == rp.maintainedAt.Load() {
		return
	}
	c.startMaintain(rp)
}

// startMaintain runs maintenance on the repository in the background, or,
// while a run is in progress, runs it again once that one ends.
func (c *Cache) startMaintain(rp *repo) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	if rp.maintaining {
		rp.maintainPending = true
		c.mu.Unlock()
		return
	}
	rp.maintaining = true
	// Matches a refresh: Close waits for the run, and it cannot be started
	// by a request after Close has begun.
	c.refreshes.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.refreshes.Done()
		for {
			c.maintain(rp)
			c.mu.Lock()
			pending := rp.maintainPending
			rp.maintainPending = false
			if !pending {
				rp.maintaining = false
			}
			c.mu.Unlock()
			if !pending {
				return
			}
		}
	}()
}

// maintainLater runs maintenance on the repository once wait has passed,
// to index the fetches a run skipped within MaintenanceInterval, which no
// later fetch may come to trigger. A repository has one such timer at a
// time, as the run it starts covers every fetch before it. After Close,
// the timer does nothing.
func (c *Cache) maintainLater(rp *repo, wait time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || rp.maintainTimer != nil {
		return
	}
	rp.maintainTimer = time.AfterFunc(wait, func() {
		c.mu.Lock()
		rp.maintainTimer = nil
		c.mu.Unlock()
		c.startMaintain(rp)
	})
}

// maintain writes the index a repository is served from: a multi-pack
// index with reachability bitmaps, which turn the object enumeration at
// the head of every clone from a walk of the whole history into bitmap
// lookups. Packs are never rewritten, so a run costs no more than reading
// them. It runs in the background, and neither requests nor fetches wait
// for it: the fetches that start while it writes skip the gc that would
// otherwise end them, the only thing that deletes packs.
func (c *Cache) maintain(rp *repo) {
	// The repository may have been evicted since it asked for maintenance;
	// the one to maintain is whatever is cached under its key now.
	// Like a background refresh, a run is not a use of the repository:
	// recording it as one would keep a repository no client asks for warm
	// and ahead in the eviction order for as long as its upstream changes.
	rp = c.acquire(rp.key)
	defer func() { c.drop(rp, rp.exists()) }()
	if !rp.exists() {
		return
	}
	ctx, cancel := context.WithTimeout(c.ctx, maintainTimeout)
	defer cancel()
	// The slot keeps the runs queued from piling onto the CPU.
	select {
	case c.maintains <- struct{}{}:
		defer func() { <-c.maintains }()
	case <-ctx.Done():
		return
	}
	// The write slot is held only to start: while it is, no fetch runs,
	// so none is ending in a gc that could delete the packs the run is
	// about to read, and every fetch after it sees indexing set.
	unlock, err := rp.lockWrites(ctx)
	if err != nil {
		return
	}
	// The fetches up to here are the ones this run covers.
	written := rp.refsWritten.Load()
	wait, ok := c.indexDue(rp)
	if !ok || wait > 0 {
		unlock()
		// Done without writing anything, or every request until the
		// interval ran out would run maintenance again.
		rp.maintainedAt.Store(written)
		if wait > 0 {
			c.maintainLater(rp, wait)
		}
		return
	}
	// A run through an entry of the repository from before an eviction
	// may still be writing in its directory.
	if !rp.indexing.CompareAndSwap(false, true) {
		unlock()
		return
	}
	defer rp.indexing.Store(false)
	c.clearLeftovers(rp)
	unlock()
	if err := c.writeIndex(ctx, rp); err != nil {
		// Not stored, so the next fetch retries.
		c.logger.Warn("git cache: writing the multi-pack index failed", "repository", rp.key, "error", err)
		return
	}
	rp.maintainedAt.Store(written)
}

// writeIndex writes the repository's multi-pack index with its bitmaps.
// git builds on the index already there, so one that names a pack no
// longer there, as a gc running alongside a write leaves, fails every
// write after it. When a write fails, the index is removed and written
// once more from the packs alone.
func (c *Cache) writeIndex(ctx context.Context, rp *repo) error {
	write := func() error {
		_, err := c.run(ctx, rp.dir, "multi-pack-index", "write", "--bitmap")
		return err
	}
	err := write()
	if err == nil || ctx.Err() != nil {
		return err
	}
	c.logger.Warn("git cache: writing the multi-pack index failed; writing it again from the packs alone",
		"repository", rp.key, "error", err)
	files, _ := filepath.Glob(filepath.Join(rp.dir, "objects", "pack", midxName+"*"))
	for _, path := range files {
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return write()
}

// clearLeftovers removes, before a run writes the index, what earlier runs
// and fetches left behind that would get in its way or only take space:
// the index's lock and the temporary files of a run or fetch cut short,
// and the commit-graph, which git does not read while maintenance is on.
// It runs with the write slot held and no other run writing, so nothing
// it removes is in use.
func (c *Cache) clearLeftovers(rp *repo) {
	packDir := filepath.Join(rp.dir, "objects", "pack")
	paths, _ := filepath.Glob(filepath.Join(packDir, "tmp_*"))
	paths = append(paths,
		filepath.Join(packDir, midxName+".lock"),
		filepath.Join(rp.dir, "objects", "info", "commit-graph"),
		filepath.Join(rp.dir, "objects", "info", "commit-graphs"))
	for _, path := range paths {
		if err := os.RemoveAll(path); err != nil {
			c.logger.Warn("git cache: removing a leftover before maintenance", "path", path, "error", err)
		}
	}
}

// indexDue reports how long until the repository's serving index is due
// to be rewritten: no time when it has no multi-pack index or no bitmaps
// beside it, else what remains of MaintenanceInterval since it was
// written. ok is false for a repository with no packs, which has nothing
// to index.
func (c *Cache) indexDue(rp *repo) (wait time.Duration, ok bool) {
	packs, _ := filepath.Glob(filepath.Join(rp.dir, "objects", "pack", "pack-*.idx"))
	if len(packs) == 0 {
		return 0, false
	}
	midx := filepath.Join(rp.dir, "objects", "pack", midxName)
	info, err := os.Stat(midx)
	if err != nil {
		return 0, true
	}
	if bitmaps, _ := filepath.Glob(midx + "-*.bitmap"); len(bitmaps) == 0 {
		return 0, true
	}
	return max(0, c.opts.MaintenanceInterval-time.Since(info.ModTime())), true
}

// fetchFlags returns flags for a fetch into the repository, which must
// hold its write slot. While a maintenance run writes the repository's
// index, the fetch skips the gc that may end it, which could delete the
// packs the run is reading; a later fetch runs it.
func fetchFlags(rp *repo, flags []string) []string {
	if rp.indexing.Load() {
		return append(slices.Clip(flags), "--no-auto-maintenance")
	}
	return flags
}
