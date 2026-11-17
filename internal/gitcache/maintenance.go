package gitcache

import (
	"context"
	"errors"
	"io"
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

// maintain writes the indexes a repository is served from: a multi-pack
// index with reachability bitmaps, which turn the object enumeration at
// the head of every clone from a walk of the whole history into bitmap
// lookups. Packs are never rewritten, so a run costs no more than reading
// them. The indexes are read back afterward, so that a filesystem that does not keep them in
// the page cache, a network mount or a freshly booted machine say, does
// not have the next fetch pay for that reading. It runs in the background:
// requests are never held for it, and neither are fetches for longer than
// one run.
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
	c.preloadIndexes(ctx, rp.dir)
	c.logger.Debug("git cache: read the serving indexes into the page cache", "repository", rp.key)
}

// preloadIndexes reads the serving indexes of the repository at dir
// sequentially, which brings them into the page cache, so that the next
// fetch is not the one to pay for reading them.
func (c *Cache) preloadIndexes(ctx context.Context, dir string) {
	for _, path := range servingIndexes(dir) {
		if ctx.Err() != nil {
			return
		}
		// The file can vanish, evicted say, between it being listed and
		// opened, or something else take its place, which would not read
		// like a file; nothing is served from it anymore, so skip it.
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
			_ = file.Close()
			continue
		}
		_, err = io.Copy(io.Discard, file)
		_ = file.Close()
		if err != nil {
			c.logger.Warn("git cache: reading a serving index", "path", path, "error", err)
		}
	}
}

// preloadCached starts reading the serving indexes of the cached
// repositories into the page cache, in the background, once at startup.
// Close stops it and waits for it.
func (c *Cache) preloadCached() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	var rps []*repo
	for _, rp := range c.repos {
		rps = append(rps, rp)
	}
	if len(rps) == 0 {
		c.mu.Unlock()
		return
	}
	// Matches a maintenance run: Close waits for the reading, and it
	// cannot start once Close has begun.
	c.refreshes.Add(1)
	c.mu.Unlock()
	c.logger.Info("git cache: reading the serving indexes of the cached repositories into the page cache",
		"repositories", len(rps))
	go func() {
		defer c.refreshes.Done()
		for _, rp := range rps {
			if c.ctx.Err() != nil {
				return
			}
			c.preloadIndexes(c.ctx, rp.dir)
		}
	}()
}

// servingIndexes returns the index files a repository at dir is served
// from, that exist: the multi-pack index, with its reachability bitmaps
// and reverse indexes beside it.
func servingIndexes(dir string) []string {
	packDir := filepath.Join(dir, "objects", "pack")
	var paths []string
	for _, pattern := range []string{midxName, midxName + "-*.bitmap", midxName + "-*.rev"} {
		matches, _ := filepath.Glob(filepath.Join(packDir, pattern))
		paths = append(paths, matches...)
	}
	return paths
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
