package gitcache

import (
	"context"
	"sync/atomic"
	"time"
)

const (
	// maxReserveChunk caps the space a fetch reserves ahead of what it has
	// received, so the cache is not locked for every read. Each fetch takes
	// at most a sixteenth of MaxSize ahead, so fetches share a small cache.
	maxReserveChunk = 4 << 20
	reserveChunks   = 16
	// cutWait bounds how long a fetch waits for one that was cut off to
	// give back its space.
	cutWait = time.Minute
)

// reservation holds space within MaxSize for the bytes a fetch receives,
// until they are counted in the repository's size. Fetches still running
// therefore count toward MaxSize, so the cache stays within it however many
// run at once.
type reservation struct {
	c *Cache
	// reserved is written under c.mu; reads that find it large enough skip
	// the lock.
	reserved atomic.Int64
	// cut, guarded by c.mu, marks a fetch stopped because the cache could
	// not make room for it.
	cut bool
}

func (c *Cache) newReservation() *reservation {
	return &reservation{c: c}
}

// cover reserves space for a fetch that has received relayed bytes,
// evicting idle repositories as needed. It returns ErrCacheFull when evicting
// every idle repository would not make room, unless another fetch has just
// been cut off, whose space it waits for: one cut off waits for nothing, so
// two fetches that do not fit together do not both fail.
func (r *reservation) cover(ctx context.Context, relayed int64) error {
	c := r.c
	if c.opts.MaxSize <= 0 || relayed <= r.reserved.Load() {
		return nil
	}
	var timeout <-chan time.Time
	for {
		c.mu.Lock()
		if r.cut {
			c.mu.Unlock()
			return ErrCacheFull
		}
		need := relayed - r.reserved.Load()
		removed, total := c.evictLocked(need)
		fits := total+need <= c.opts.MaxSize
		var freed chan struct{}
		switch {
		case fits:
			chunk := min(maxReserveChunk, c.opts.MaxSize/reserveChunks)
			n := min(need+chunk, c.opts.MaxSize-total)
			r.reserved.Add(n)
			c.reserved += n
		case c.cutPending > 0:
			freed = c.freed
		default:
			r.cut = true
			c.cutPending++
		}
		c.mu.Unlock()
		c.removeEvicted(removed)
		if fits {
			return nil
		}
		if freed == nil {
			return ErrCacheFull
		}
		if timeout == nil {
			timeout = time.After(cutWait)
		}
		select {
		case <-freed:
		case <-timeout:
			c.mu.Lock()
			r.cut = true
			c.cutPending++
			c.mu.Unlock()
			return ErrCacheFull
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// settle counts what the fetch left in the repository's size and gives back
// its reservation together, so the bytes are never counted twice or not at
// all.
func (r *reservation) settle(rp *repo, size int64) {
	r.c.mu.Lock()
	rp.size.Store(size)
	r.releaseLocked()
	r.c.mu.Unlock()
}

// release gives back the reservation of a fetch that left nothing behind.
// It may follow settle.
func (r *reservation) release() {
	r.c.mu.Lock()
	r.releaseLocked()
	r.c.mu.Unlock()
}

func (r *reservation) releaseLocked() {
	c := r.c
	n := r.reserved.Swap(0)
	if r.cut {
		r.cut = false
		c.cutPending--
	} else if n == 0 {
		return
	}
	c.reserved -= n
	close(c.freed)
	c.freed = make(chan struct{})
}
