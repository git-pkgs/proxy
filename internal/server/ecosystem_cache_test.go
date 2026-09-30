package server

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
)

func cacheTestDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.Create(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// The zero value must work, because Server is assembled as a struct literal in
// places that never call a constructor.
func TestEcosystemStatsCacheZeroValueIsUsable(t *testing.T) {
	var c ecosystemStatsCache

	if _, err := c.Get(cacheTestDB(t)); err != nil {
		t.Fatalf("Get on a zero-value cache: %v", err)
	}
	if c.interval() != ecosystemStatsTTL {
		t.Errorf("interval = %v, want the default %v", c.interval(), ecosystemStatsTTL)
	}
}

// A second call inside the TTL must not touch the database again — that is the
// whole point, since /stats is unauthenticated and pollable.
func TestEcosystemStatsCacheServesWithinTTL(t *testing.T) {
	db := cacheTestDB(t)
	c := ecosystemStatsCache{ttl: time.Hour}

	if _, err := c.Get(db); err != nil {
		t.Fatalf("Get: %v", err)
	}
	first := c.lastAt

	if _, err := c.Get(db); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !c.lastAt.Equal(first) {
		t.Error("a second Get inside the TTL re-queried the database")
	}
}

func TestEcosystemStatsCacheRefreshesAfterTTL(t *testing.T) {
	db := cacheTestDB(t)
	// A TTL already elapsed by the time the second call lands.
	c := ecosystemStatsCache{ttl: time.Nanosecond}

	if _, err := c.Get(db); err != nil {
		t.Fatalf("Get: %v", err)
	}
	first := c.lastAt

	time.Sleep(time.Millisecond)
	if _, err := c.Get(db); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if c.lastAt.Equal(first) {
		t.Error("the cache did not refresh after its TTL elapsed")
	}
}

// Refresh ignores the TTL, so the metrics loop always publishes a fresh read.
func TestEcosystemStatsCacheRefreshBypassesTTL(t *testing.T) {
	db := cacheTestDB(t)
	c := ecosystemStatsCache{ttl: time.Hour}

	if _, err := c.Get(db); err != nil {
		t.Fatalf("Get: %v", err)
	}
	first := c.lastAt

	time.Sleep(time.Millisecond)
	if _, err := c.Refresh(db); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if c.lastAt.Equal(first) {
		t.Error("Refresh honoured the TTL; it must always re-read")
	}
}

// A transient failure must not erase a snapshot the caller could still show.
func TestEcosystemStatsCacheKeepsLastGoodSnapshotOnError(t *testing.T) {
	good := cacheTestDB(t)
	seedCachePackage(t, good, "npm", "lodash")

	c := ecosystemStatsCache{ttl: time.Nanosecond}
	stats, err := c.Get(good)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("expected 1 ecosystem from the seeded database, got %+v", stats)
	}

	// A closed handle stands in for the database going away.
	broken := cacheTestDB(t)
	if err := broken.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	time.Sleep(time.Millisecond)
	stats, err = c.Get(broken)
	if err == nil {
		t.Fatal("expected an error from the closed database")
	}
	if len(stats) != 1 || stats[0].Ecosystem != "npm" {
		t.Errorf("last good snapshot was discarded: got %+v", stats)
	}
}

// An error must not be pinned for the full TTL, or a recovered database stays
// invisible for a minute.
func TestEcosystemStatsCacheRetriesSoonerAfterError(t *testing.T) {
	broken := cacheTestDB(t)
	if err := broken.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	c := ecosystemStatsCache{ttl: time.Hour}
	if _, err := c.Get(broken); err == nil {
		t.Fatal("expected an error")
	}
	if got := c.interval(); got != ecosystemStatsErrorTTL {
		t.Errorf("retry interval after an error = %v, want %v", got, ecosystemStatsErrorTTL)
	}
}

// A closed database makes the query fail; the error must reach the caller
// rather than being swallowed into an empty-looking result.
func TestEcosystemStatsCacheSurfacesErrors(t *testing.T) {
	db, err := database.Create(filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var c ecosystemStatsCache
	if _, err := c.Get(db); err == nil {
		t.Error("expected an error from a closed database")
	}
}

func seedCachePackage(t *testing.T, db *database.DB, ecosystem, name string) {
	t.Helper()
	if err := db.UpsertPackage(&database.Package{
		PURL:      "pkg:" + ecosystem + "/" + name,
		Ecosystem: ecosystem,
		Name:      name,
	}); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}
}

// The snapshot timestamp must track the last successful read, not the last
// attempt. The analytics page dates its "figures may be out of date" banner
// from it, and a timestamp that advanced on every failed retry would report an
// hour-old snapshot as seconds old.
func TestEcosystemStatsCacheSnapshotAtTracksSuccessOnly(t *testing.T) {
	good := cacheTestDB(t)
	seedCachePackage(t, good, "npm", "lodash")

	c := ecosystemStatsCache{ttl: time.Nanosecond}
	if c.SnapshotAt(); !c.SnapshotAt().IsZero() {
		t.Fatalf("SnapshotAt on a fresh cache = %v, want the zero time", c.SnapshotAt())
	}
	if _, err := c.Get(good); err != nil {
		t.Fatalf("Get: %v", err)
	}

	at := c.SnapshotAt()
	if at.IsZero() {
		t.Fatal("SnapshotAt is zero after a successful read")
	}

	broken := cacheTestDB(t)
	if err := broken.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	time.Sleep(2 * time.Millisecond)
	if _, err := c.Get(broken); err == nil {
		t.Fatal("expected an error from the closed database")
	}
	if got := c.SnapshotAt(); !got.Equal(at) {
		t.Errorf("SnapshotAt moved on a failed read: %v, want %v", got, at)
	}
}
