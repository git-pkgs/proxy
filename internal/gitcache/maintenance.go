package gitcache

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// maintainTimeout bounds one maintenance run, so a repository whose indexes
// cannot be written, on a full disk say, holds neither its write slot nor
// the fetches waiting for it for long.
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

// maintain writes the indexes a repository is served from: an incremental
// commit-graph, which speeds up the commit walks of the web UI and of the
// negotiation of every fetch, and a multi-pack index with reachability
// bitmaps, which turn the object enumeration at the head of every clone
// from a walk of the whole history into bitmap lookups. Packs are never
// rewritten, so a run costs no more than reading them. It runs in the
// background: requests are never held for it, and neither are fetches for
// longer than one run.
func (c *Cache) maintain(rp *repo) {
	// The repository may have been evicted since it asked for maintenance;
	// the one to maintain is whatever is cached under its key now.
	rp = c.acquire(rp.key)
	defer c.release(rp)
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
	// With the write slot held, no fetch can land a pack the multi-pack
	// index would miss, and no other writer of the index files runs.
	unlock, err := rp.lockWrites(ctx)
	if err != nil {
		return
	}
	defer unlock()
	c.clearMaintainLocks(rp.dir)
	if _, err := c.run(ctx, rp.dir, "commit-graph", "write", "--reachable", "--split"); err != nil {
		c.logger.Warn("git cache: writing the commit-graph failed", "repository", rp.key, "error", err)
	}
	if !c.bitmapDue(rp) {
		return
	}
	if _, err := c.run(ctx, rp.dir, "multi-pack-index", "write", "--bitmap"); err != nil {
		c.logger.Warn("git cache: writing the multi-pack index failed", "repository", rp.key, "error", err)
		return
	}
	// Not stored on a skip or a failure, so the next run tries again.
	rp.maintainedAt.Store(rp.refsWritten.Load())
}

// bitmapDue reports whether the repository's multi-pack index and its
// bitmaps should be rewritten: when it has neither, when its bitmaps are
// missing, or when the ones it has are older than MaintenanceInterval. A
// repository with no packs has nothing to index and no bitmaps to make.
func (c *Cache) bitmapDue(rp *repo) bool {
	packs, _ := filepath.Glob(filepath.Join(rp.dir, "objects", "pack", "pack-*.idx"))
	if len(packs) == 0 {
		return false
	}
	midx := filepath.Join(rp.dir, "objects", "pack", midxName)
	info, err := os.Stat(midx)
	if err != nil {
		return true
	}
	if bitmaps, _ := filepath.Glob(midx + "-*.bitmap"); len(bitmaps) == 0 {
		return true
	}
	return c.opts.MaintenanceInterval <= 0 || time.Since(info.ModTime()) >= c.opts.MaintenanceInterval
}

// clearMaintainLocks removes the lock files of the index writers, left by a
// maintenance run cut short when the process stopped, and the graph layers
// such a run wrote halfway; the index files themselves are renamed into
// place, so a run interrupted after them leaves them complete. With the
// repository's write slot held, any of these files that exist are stale.
func (c *Cache) clearMaintainLocks(dir string) {
	stale := []string{
		filepath.Join(dir, "objects", "pack", midxName+".lock"),
		filepath.Join(dir, "objects", "info", commitGraphName+".lock"),
		filepath.Join(dir, "objects", "info", commitGraphsName, "commit-graph-chain.lock"),
	}
	tmps, _ := filepath.Glob(filepath.Join(dir, "objects", "info", commitGraphsName, "tmp_graph_*"))
	for _, path := range append(stale, tmps...) {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			c.logger.Warn("git cache: removing a stale maintenance lock", "path", path, "error", err)
		}
	}
}
