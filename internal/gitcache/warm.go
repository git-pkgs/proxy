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
	// one repository. With a StaleMaxAge under twice this, Warm does
	// nothing.
	warmMinInterval = 10 * time.Minute
	// defaultWarmTick is how often Warm looks for repositories due for a
	// check. The first look waits a random part of it, so instances
	// started together do not check their repositories at once.
	defaultWarmTick = time.Minute
	// warmStaleFraction divides StaleMaxAge into the interval between
	// background checks, leaving time for a failed check to be retried
	// before the cached copy goes stale.
	warmStaleFraction = 2
	// warmRetryFraction divides that interval into the wait after a failed
	// check, so a few retries fit before the copy goes stale.
	warmRetryFraction = 4
)

// Warm refreshes, in the background and one at a time, the cached
// repositories that clients used within WarmMaxIdle once half of StaleMaxAge
// has passed since their last successful check, so a cached copy stays
// servable through an upstream outage. A failed check is tried again after a
// quarter of that interval, at least warmMinInterval. Refreshes stop when
// the cache is closed.
//
// A repository is checked at the URL its last successful refresh used, so
// one cached before that URL was kept waits for a client to refresh it.
// serves reports whether the configuration still serves a repository's
// directory in the cache, such as github-1a2b3c4d/acme/widgets.git.
//
// It does nothing with WarmMaxIdle 0, or StaleMaxAge under twice
// warmMinInterval, which includes 0.
func (c *Cache) Warm(serves func(dir string) bool) {
	if !c.warmEnabled() {
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
			c.warmPass(c.ctx, serves)
			wait = tick
		}
	}()
}

// warmEnabled reports whether Warm checks repositories at all: not with
// WarmMaxIdle 0, nor when checks every half StaleMaxAge would come more
// often than warmMinInterval.
func (c *Cache) warmEnabled() bool {
	return c.opts.WarmMaxIdle > 0 && c.warmInterval() >= warmMinInterval
}

// warmInterval is how long after a check a repository is checked again in
// the background: half of StaleMaxAge.
func (c *Cache) warmInterval() time.Duration {
	return c.opts.StaleMaxAge / warmStaleFraction
}

// warmRetryInterval is how long after a failed check a repository is tried
// again in the background: a quarter of warmInterval, at least
// warmMinInterval.
func (c *Cache) warmRetryInterval() time.Duration {
	return max(c.warmInterval()/warmRetryFraction, warmMinInterval)
}

// warmPass refreshes, one after another, the repositories due for a
// background check, and returns how many it refreshed.
func (c *Cache) warmPass(ctx context.Context, serves func(dir string) bool) int {
	if !c.warmEnabled() {
		return 0
	}
	refreshed := 0
	for _, rp := range c.warmDue(time.Now()) {
		dir, err := filepath.Rel(c.dir, rp.dir)
		if err != nil || !serves(filepath.ToSlash(dir)) {
			continue
		}
		call := c.startWarmRefresh(rp)
		if call == nil {
			continue
		}
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

// startWarmRefresh starts a background refresh of rp, unless by now it is
// no longer the cache's entry for its key or no longer due: a pass works
// from a list taken before its first refresh, and evictions and requests
// go on meanwhile.
func (c *Cache) startWarmRefresh(rp *repo) *refreshCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.repos[rp.key] != rp || !c.warmDueLocked(rp, time.Now()) {
		return nil
	}
	return c.startRefreshLocked(rp, rp.repoURL(), true)
}

// warmDue returns the cached repositories due for a background check, the
// longest unchecked first.
func (c *Cache) warmDue(now time.Time) []*repo {
	c.mu.Lock()
	var due []*repo
	for _, rp := range c.repos {
		if c.warmDueLocked(rp, now) {
			due = append(due, rp)
		}
	}
	c.mu.Unlock()
	slices.SortFunc(due, func(a, b *repo) int {
		return cmp.Compare(a.lastChecked.Load(), b.lastChecked.Load())
	})
	return due
}

// warmDueLocked reports whether rp is due for a background check: cached,
// with a URL a refresh succeeded with, used by a client within WarmMaxIdle,
// not checked within warmInterval nor failed within warmRetryInterval, not
// refreshing, and not refused upstream or waiting after passing
// git.max_repo_size. c.mu must be held.
func (c *Cache) warmDueLocked(rp *repo, now time.Time) bool {
	used := rp.lastUsed.Load()
	if rp.refused != nil || rp.refreshing != nil || used == 0 || rp.repoURL() == "" ||
		now.Sub(time.Unix(0, used)) > c.opts.WarmMaxIdle ||
		now.Sub(time.Unix(0, rp.lastChecked.Load())) < c.warmInterval() {
		return false
	}
	if failed := rp.lastError.Load(); failed != nil && now.Sub(failed.at) < c.warmRetryInterval() {
		return false
	}
	if since, ok := c.tooLarge[rp.key]; ok && now.Sub(since) < tooLargeRetryAfter {
		return false
	}
	return rp.exists()
}
