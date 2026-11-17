package gitcache

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// midxName names the serving index maintenance writes in objects/pack,
// with reachability bitmaps beside it.
const midxName = "multi-pack-index"

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

// maintain writes the index a repository is served from: a multi-pack
// index with reachability bitmaps, which turn the object enumeration at
// the head of every clone from a walk of the whole history into bitmap
// lookups. Packs are never rewritten, so a run costs no more than reading
// them. It runs in the background: requests are never held for it, and
// neither are fetches for longer than one run.
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
	// Within the interval the run is done without writing anything, or
	// every request until it ran out would run maintenance again; the
	// bitmaps follow the next fetch that changes the repository.
	if !c.bitmapDue(rp) {
		rp.maintainedAt.Store(rp.refsWritten.Load())
		return
	}
	// A run cut short when the process stopped leaves the index's lock
	// behind, which would fail every later write. With the write slot
	// held, it is stale.
	lock := filepath.Join(rp.dir, "objects", "pack", midxName+".lock")
	if err := os.Remove(lock); err != nil && !errors.Is(err, fs.ErrNotExist) {
		c.logger.Warn("git cache: removing a stale maintenance lock", "path", lock, "error", err)
	}
	if _, err := c.run(ctx, rp.dir, "multi-pack-index", "write", "--bitmap"); err != nil {
		// Not stored, so the next request retries.
		c.logger.Warn("git cache: writing the multi-pack index failed", "repository", rp.key, "error", err)
		return
	}
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
