package server

import (
	"sync"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
)

// ecosystemStatsTTL bounds how stale a served snapshot can be. The metrics
// refresh loop runs on the same cadence, so a scrape and a page load a moment
// apart report the same figures rather than two slightly different ones.
const ecosystemStatsTTL = time.Minute

// ecosystemStatsErrorTTL is the retry interval after a failed read. It is short
// so a recovered database is picked up quickly, but not zero, so a database
// that is down is not queried once per request.
const ecosystemStatsErrorTTL = 5 * time.Second

// ecosystemStatsCache memoizes GetEcosystemStats.
//
// The query is three grouped aggregations over the whole artifact table, which
// is fine once a minute for the metrics gauges but not once per request:
// /stats is unauthenticated and linked from the UI, so without this a polling
// client would keep the database busy for as long as it cared to.
//
// The zero value is usable, so a Server assembled as a struct literal — as
// tests do — needs no constructor and cannot end up with a nil cache.
type ecosystemStatsCache struct {
	ttl time.Duration

	mu     sync.Mutex
	stats  []database.EcosystemStats
	err    error
	lastAt time.Time
	// goodAt is when stats was last read successfully, which is not lastAt
	// once reads start failing and the retained snapshot is served on.
	goodAt time.Time
}

// SnapshotAt reports when the rows currently held were last read successfully.
// The zero time means no read has ever succeeded.
func (c *ecosystemStatsCache) SnapshotAt() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.goodAt
}

func (c *ecosystemStatsCache) interval() time.Duration {
	if c.err != nil {
		return ecosystemStatsErrorTTL
	}
	if c.ttl <= 0 {
		return ecosystemStatsTTL
	}
	return c.ttl
}

// Get returns the cached rows, refreshing them from db when they have aged out.
//
// A failed read returns the last good snapshot alongside the error, so a
// caller can choose to serve stale figures rather than fail outright.
func (c *ecosystemStatsCache) Get(db *database.DB) ([]database.EcosystemStats, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.lastAt.IsZero() && time.Since(c.lastAt) < c.interval() {
		return c.stats, c.err
	}
	return c.refreshLocked(db)
}

// Refresh forces a read, bypassing the TTL. The metrics loop uses it so its own
// tick is never served a snapshot that is about to expire.
func (c *ecosystemStatsCache) Refresh(db *database.DB) ([]database.EcosystemStats, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refreshLocked(db)
}

func (c *ecosystemStatsCache) refreshLocked(db *database.DB) ([]database.EcosystemStats, error) {
	stats, err := db.GetEcosystemStats()
	c.lastAt = time.Now()
	c.err = err
	if err != nil {
		// Keep the last good snapshot: a transient failure should not erase
		// figures the caller could still usefully show.
		return c.stats, err
	}

	c.stats = stats
	c.goodAt = c.lastAt
	return c.stats, nil
}
