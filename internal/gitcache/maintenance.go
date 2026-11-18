package gitcache

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
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
	if rp.gcSkipped.Swap(false) {
		// The fetches that ran while an index was written skipped the gc
		// that ends a fetch. It runs here instead, with the write slot held
		// as in a fetch, and before an index is written over what it leaves.
		if _, err := c.run(ctx, rp.dir, "maintenance", "run", "--auto", "--quiet"); err != nil {
			c.logger.Warn("git cache: the gc held back while an index was written failed",
				"repository", rp.key, "error", err)
		}
	}
	due, wait := c.indexDue(rp)
	if !due {
		unlock()
		// Done without writing anything, or every request until the
		// interval ran out would run maintenance again. Packs left to
		// index are indexed when it does.
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
	start := time.Now()
	if err := c.writeIndex(ctx, rp); err != nil {
		// Not stored, so the next fetch retries.
		c.logger.Warn("git cache: writing the multi-pack index failed", "repository", rp.key, "error", err)
		return
	}
	c.logger.Debug("git cache: wrote the serving index", "repository", rp.key, "duration", time.Since(start))
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

// indexDue reports whether the repository's serving index is due to be
// written, and, when it is not, how long until packs it does not cover
// make it so. It is due when it is missing, has no bitmaps beside it,
// cannot be read or names a pack that is gone, and, once
// MaintenanceInterval has passed since it was written, when there are
// packs it does not cover. An index that covers every pack is left as it
// is, and a repository with no packs has nothing to index.
func (c *Cache) indexDue(rp *repo) (due bool, wait time.Duration) {
	packDir := filepath.Join(rp.dir, "objects", "pack")
	packs, _ := filepath.Glob(filepath.Join(packDir, "pack-*.idx"))
	if len(packs) == 0 {
		return false, 0
	}
	midx := filepath.Join(packDir, midxName)
	info, err := os.Stat(midx)
	if err != nil {
		return true, 0
	}
	if bitmaps, _ := filepath.Glob(midx + "-*.bitmap"); len(bitmaps) == 0 {
		return true, 0
	}
	indexed, err := indexedPacks(midx)
	if err != nil {
		return true, 0
	}
	uncovered := false
	for _, pack := range packs {
		name := filepath.Base(pack)
		uncovered = uncovered || !indexed[name]
		delete(indexed, name)
	}
	if len(indexed) > 0 {
		return true, 0
	}
	if !uncovered {
		return false, 0
	}
	if wait := c.opts.MaintenanceInterval - time.Since(info.ModTime()); wait > 0 {
		return false, wait
	}
	return true, 0
}

// maxPackNames bounds the PNAM chunk indexedPacks reads, far above what
// the names of any repository's packs take, so a corrupt index cannot
// make it allocate much.
const maxPackNames = 64 << 20

// The layout of a multi-pack index, from gitformat-pack(5): a 12-byte
// header, then a table of chunks, each a 4-byte ID and an 8-byte offset.
const (
	midxHeaderSize = 12
	midxChunkIDLen = 4
	midxChunkEntry = midxChunkIDLen + 8
)

// indexedPacks returns the packs the multi-pack index at path covers, by
// the names of their index files ("pack-<hash>.idx"), read from its PNAM
// chunk, the index's list of its packs. See gitformat-pack(5).
func indexedPacks(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	// The header: "MIDX", the version, the object ID version, the number
	// of chunks, the number of base indexes and the number of packs.
	var header [midxHeaderSize]byte
	if _, err := io.ReadFull(f, header[:]); err != nil {
		return nil, fmt.Errorf("reading the multi-pack index header: %w", err)
	}
	if string(header[:4]) != "MIDX" {
		return nil, errors.New("not a multi-pack index")
	}
	chunks, packs := int(header[6]), binary.BigEndian.Uint32(header[8:])
	// The chunk table: an entry per chunk, in file order, then a
	// terminating one whose offset is where the last chunk ends.
	table := make([]byte, (chunks+1)*midxChunkEntry)
	if _, err := io.ReadFull(f, table); err != nil {
		return nil, fmt.Errorf("reading the multi-pack index chunk table: %w", err)
	}
	for i := range chunks {
		entry := table[i*midxChunkEntry:]
		if string(entry[:midxChunkIDLen]) != "PNAM" {
			continue
		}
		start := binary.BigEndian.Uint64(entry[midxChunkIDLen:])
		end := binary.BigEndian.Uint64(entry[midxChunkEntry+midxChunkIDLen:])
		if end < start || end-start > maxPackNames {
			return nil, errors.New("the multi-pack index's pack names are out of bounds")
		}
		names := make([]byte, end-start)
		if _, err := f.ReadAt(names, int64(start)); err != nil {
			return nil, fmt.Errorf("reading the multi-pack index's pack names: %w", err)
		}
		// NUL-terminated names, padded with NULs to a multiple of four.
		indexed := map[string]bool{}
		for _, name := range bytes.Split(bytes.TrimRight(names, "\x00"), []byte{0}) {
			indexed[string(name)] = true
		}
		if uint32(len(indexed)) != packs {
			return nil, fmt.Errorf("the multi-pack index names %d packs, not the %d its header counts", len(indexed), packs)
		}
		return indexed, nil
	}
	return nil, errors.New("the multi-pack index has no pack names")
}

// fetchFlags returns flags for a fetch into the repository, which must
// hold its write slot. While a maintenance run writes the repository's
// index, the fetch skips the gc that may end it, which could delete the
// packs the run is reading; the next run runs it.
func fetchFlags(rp *repo, flags []string) []string {
	if !rp.indexing.Load() {
		return flags
	}
	rp.gcSkipped.Store(true)
	return append(slices.Clip(flags), "--no-auto-maintenance")
}
