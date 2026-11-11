package gitcache

import (
	"cmp"
	"context"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"time"
)

const (
	// warmMinInterval is the least time between two background checks of
	// one repository, however short StaleMaxAge is.
	warmMinInterval = 10 * time.Minute
	// defaultWarmTick is how often Warm looks for repositories due for a
	// check. The first look waits a random part of it, so instances
	// started together do not check their repositories at once.
	defaultWarmTick = time.Minute
	// warmStaleFraction divides StaleMaxAge into the interval between
	// background checks, leaving time for a failed check to be retried
	// before the cached copy goes stale.
	warmStaleFraction = 2
)

// Warm refreshes, in the background and one at a time, the cached
// repositories that clients used within WarmMaxIdle once half of StaleMaxAge
// has passed since their last successful check, so a cached copy stays
// servable through an upstream outage. A failed check is tried again after
// the same interval. Refreshes stop when the cache is closed.
//
// resolve maps a repository's directory in the cache, such as
// github-1a2b3c4d/acme/widgets.git, to its upstream URL, or reports that the
// configuration no longer serves it.
//
// It does nothing with WarmMaxIdle or StaleMaxAge 0.
func (c *Cache) Warm(resolve func(dir string) (string, bool)) {
	if c.opts.WarmMaxIdle <= 0 || c.opts.StaleMaxAge <= 0 {
		return
	}
	tick := c.opts.warmTick
	if tick <= 0 {
		tick = defaultWarmTick
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.refreshes.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.refreshes.Done()
		wait := rand.N(tick)
		for {
			select {
			case <-c.ctx.Done():
				return
			case <-time.After(wait):
			}
			c.warmPass(c.ctx, resolve)
			wait = tick
		}
	}()
}

// warmInterval is how long after a check a repository is checked again in
// the background: half of StaleMaxAge, at least warmMinInterval.
func (c *Cache) warmInterval() time.Duration {
	return max(c.opts.StaleMaxAge/warmStaleFraction, warmMinInterval)
}

// warmPass refreshes, one after another, the repositories due for a
// background check, and returns how many it refreshed.
func (c *Cache) warmPass(ctx context.Context, resolve func(dir string) (string, bool)) int {
	if c.opts.WarmMaxIdle <= 0 || c.opts.StaleMaxAge <= 0 {
		return 0
	}
	refreshed := 0
	for _, rp := range c.warmDue(time.Now()) {
		dir, err := filepath.Rel(c.dir, rp.dir)
		if err != nil {
			continue
		}
		repoURL, ok := resolve(filepath.ToSlash(dir))
		if !ok {
			continue
		}
		call := c.startRefreshAs(rp, repoURL, true)
		select {
		case <-call.done:
		case <-ctx.Done():
			return refreshed
		}
		if call.err == nil {
			refreshed++
		}
	}
	return refreshed
}

// warmDue returns the cached repositories due for a background check, the
// longest unchecked first: used by a client within WarmMaxIdle, neither
// checked nor failed within warmInterval, and not refused upstream or
// waiting after passing git.max_repo_size.
func (c *Cache) warmDue(now time.Time) []*repo {
	interval := c.warmInterval()
	c.mu.Lock()
	var due []*repo
	for _, rp := range c.repos {
		used := rp.lastUsed.Load()
		last := rp.lastChecked.Load()
		if failed := rp.lastError.Load(); failed != nil {
			last = max(last, failed.at.UnixNano())
		}
		if rp.refused != nil || rp.refreshing != nil || used == 0 ||
			now.Sub(time.Unix(0, used)) > c.opts.WarmMaxIdle ||
			now.Sub(time.Unix(0, last)) < interval {
			continue
		}
		due = append(due, rp)
	}
	c.mu.Unlock()

	due = slices.DeleteFunc(due, func(rp *repo) bool { return !rp.exists() || c.isTooLarge(rp.key) })
	slices.SortFunc(due, func(a, b *repo) int {
		return cmp.Compare(a.lastChecked.Load(), b.lastChecked.Load())
	})
	return due
}
