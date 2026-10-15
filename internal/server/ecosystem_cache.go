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

// statsCache memoizes one of the grouped statistics queries behind the
// analytics page and the metrics gauges.
//
// Each query is a grouped aggregation over a whole table, which is fine once a
// minute for the metrics gauges but not once per page load: the UI is
// unauthenticated, so without this a client reloading /ui/analytics would keep
// the database busy for as long as it cared to. The page and the gauges read
// the same snapshot, so they never disagree.
//
// L selects the query. It is a zero-size type rather than a stored function so
// the zero value stays usable: a Server assembled as a struct literal -- as
// tests do -- needs no constructor and cannot end up with a nil cache.
type statsCache[E any, L statsLoader[E]] struct {
	ttl time.Duration

	mu     sync.Mutex
	stats  []E
	err    error
	lastAt time.Time
	// goodAt is when stats was last read successfully, which is not lastAt
	// once reads start failing and the retained snapshot is served on.
	goodAt time.Time
}

// statsLoader runs the query a statsCache memoizes, returning one row per
// ecosystem.
type statsLoader[E any] interface {
	load(db *database.DB) ([]E, error)
}

type ecosystemStatsLoader struct{}

func (ecosystemStatsLoader) load(db *database.DB) ([]database.EcosystemStats, error) {
	return db.GetEcosystemStats()
}

type metadataStatsLoader struct{}

func (metadataStatsLoader) load(db *database.DB) ([]database.MetadataCacheStats, error) {
	return db.GetMetadataCacheStats()
}

// ecosystemStatsCache memoizes GetEcosystemStats.
type ecosystemStatsCache = statsCache[database.EcosystemStats, ecosystemStatsLoader]

// metadataStatsCache memoizes GetMetadataCacheStats.
type metadataStatsCache = statsCache[database.MetadataCacheStats, metadataStatsLoader]

// SnapshotAt reports when the rows currently held were last read successfully.
// The zero time means no read has ever succeeded.
func (c *statsCache[E, L]) SnapshotAt() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.goodAt
}

func (c *statsCache[E, L]) interval() time.Duration {
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
func (c *statsCache[E, L]) Get(db *database.DB) ([]E, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.lastAt.IsZero() && time.Since(c.lastAt) < c.interval() {
		return c.stats, c.err
	}
	return c.refreshLocked(db)
}

// Refresh forces a read, bypassing the TTL. The metrics loop uses it so its own
// tick is never served a snapshot that is about to expire.
func (c *statsCache[E, L]) Refresh(db *database.DB) ([]E, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refreshLocked(db)
}

func (c *statsCache[E, L]) refreshLocked(db *database.DB) ([]E, error) {
	var loader L
	stats, err := loader.load(db)
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
