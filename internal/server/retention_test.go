package server

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/metrics"
	"github.com/git-pkgs/proxy/internal/retention"
	"github.com/git-pkgs/proxy/internal/storage"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

const retentionDay = 24 * time.Hour

// retentionRegistry registers npm and gem the way their handlers would, in a
// registry of the test's own so the handlers' registrations stay untouched.
func retentionRegistry() *retention.Registry {
	reg := retention.NewRegistry()
	reg.Register(retention.Spec{Key: "npm", CanonicalPackage: retention.DefaultCanonical("npm")})
	reg.Register(retention.Spec{Key: "gem", CanonicalPackage: retention.DefaultCanonical("gem")})
	return reg
}

// seedAgedArtifact stores a cached artifact of pkg:{ecosystem}/{name}, with
// the package record's ecosystem set to dbEcosystem, fetched and last
// accessed at the given times. A zero accessed leaves it never served.
func seedAgedArtifact(t *testing.T, db *database.DB, store storage.Storage, dbEcosystem, purlType, name string,
	fetched, accessed time.Time) string {
	t.Helper()
	pkgPURL := "pkg:" + purlType + "/" + name
	versionPURL := pkgPURL + "@1.0.0"
	filename := name + "-1.0.0.tgz"
	if err := db.UpsertPackage(&database.Package{PURL: pkgPURL, Ecosystem: dbEcosystem, Name: name}); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}
	if err := db.UpsertVersion(&database.Version{PURL: versionPURL, PackagePURL: pkgPURL}); err != nil {
		t.Fatalf("UpsertVersion: %v", err)
	}
	path := storage.ArtifactPath(dbEcosystem, "", name, "1.0.0", filename)
	size, hash, err := store.Store(context.Background(), path, strings.NewReader("artifact"))
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	art := &database.Artifact{
		VersionPURL: versionPURL,
		Filename:    filename,
		UpstreamURL: "https://example.com/" + filename,
		StoragePath: sql.NullString{String: path, Valid: true},
		ContentHash: sql.NullString{String: hash, Valid: true},
		Size:        sql.NullInt64{Int64: size, Valid: true},
		FetchedAt:   sql.NullTime{Time: fetched, Valid: true},
	}
	if !accessed.IsZero() {
		art.LastAccessedAt = sql.NullTime{Time: accessed, Valid: true}
	}
	if err := db.UpsertArtifact(art); err != nil {
		t.Fatalf("UpsertArtifact: %v", err)
	}
	return path
}

func isCached(t *testing.T, db *database.DB, purlType, name string) bool {
	t.Helper()
	art, err := db.GetArtifact("pkg:"+purlType+"/"+name+"@1.0.0", name+"-1.0.0.tgz")
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	return art.StoragePath.Valid
}

func TestSweepRetentionAppliesEachRule(t *testing.T) {
	db, store := setupEvictionTest(t)
	now := time.Now()
	ago := func(days int) time.Time { return now.Add(-time.Duration(days) * retentionDay) }

	rules := retention.Rules{
		Default:    30 * retentionDay,
		Ecosystems: map[string]time.Duration{"npm": 14 * retentionDay},
		Packages: map[string]time.Duration{
			"pkg:npm/pinned":             0,
			"pkg:npm/long-lived":         90 * retentionDay,
			"pkg:npm/long-lived-expired": 90 * retentionDay,
			"pkg:gem/short":              2 * retentionDay,
		},
	}

	cases := []struct {
		dbEco, purlType, name string
		fetched, accessed     time.Time
		wantCached            bool
	}{
		{"npm", "npm", "stale", ago(60), ago(20), false},
		{"npm", "npm", "fresh", ago(60), ago(5), true},
		{"npm", "npm", "never-served", ago(20), time.Time{}, false},
		{"npm", "npm", "pinned", ago(400), ago(400), true},
		{"npm", "npm", "long-lived", ago(60), ago(60), true},
		{"npm", "npm", "long-lived-expired", ago(100), ago(100), false},
		{"gem", "gem", "default-applies", ago(40), ago(40), false},
		{"gem", "gem", "within-default", ago(40), ago(20), true},
		{"rubygems", "gem", "mirrored", ago(40), time.Time{}, false},
		{"gem", "gem", "short", ago(3), ago(3), false},
		{"pypi", "pypi", "unregistered", ago(400), ago(400), true},
	}
	for _, c := range cases {
		seedAgedArtifact(t, db, store, c.dbEco, c.purlType, c.name, c.fetched, c.accessed)
	}
	sweepRetention(context.Background(), db, discardLogger(), retentionRegistry(), rules, now)

	for _, c := range cases {
		if got := isCached(t, db, c.purlType, c.name); got != c.wantCached {
			t.Errorf("%s cached = %v, want %v", c.name, got, c.wantCached)
		}
	}
}

func TestSweepRetentionQueuesInsteadOfDeleting(t *testing.T) {
	db, store := setupEvictionTest(t)
	now := time.Now()
	old := now.Add(-30 * retentionDay)
	path := seedAgedArtifact(t, db, store, "npm", "npm", "stale", old, old)

	rules := retention.Rules{Ecosystems: map[string]time.Duration{"npm": retentionDay}}
	sweepRetention(context.Background(), db, discardLogger(), retentionRegistry(), rules, now)

	if isCached(t, db, "npm", "stale") {
		t.Fatal("stale artifact still cached")
	}
	if !objectExists(t, store, path) {
		t.Error("object deleted directly instead of waiting in the queue")
	}
	if got := queuedPaths(t, db); !slices.Equal(got, []string{path}) {
		t.Errorf("queue = %v, want [%s]", got, path)
	}
}

// An artifact fetched again after an eviction keeps the access time of its
// earlier copy and must not be cleared again by the next sweep.
func TestSweepRetentionKeepsRefetchedArtifact(t *testing.T) {
	db, store := setupEvictionTest(t)
	now := time.Now()
	old := now.Add(-30 * retentionDay)
	seedAgedArtifact(t, db, store, "npm", "npm", "popular", old, old)
	rules := retention.Rules{Ecosystems: map[string]time.Duration{"npm": retentionDay}}

	sweepRetention(context.Background(), db, discardLogger(), retentionRegistry(), rules, now)
	if isCached(t, db, "npm", "popular") {
		t.Fatal("first sweep kept the expired artifact")
	}

	// Refetch: the upsert sets a new fetch time but leaves last_accessed_at.
	seedAgedArtifact(t, db, store, "npm", "npm", "popular", now, time.Time{})
	art, err := db.GetArtifact("pkg:npm/popular@1.0.0", "popular-1.0.0.tgz")
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	if !art.LastAccessedAt.Valid || art.LastAccessedAt.Time.After(old.Add(time.Minute)) {
		t.Fatalf("test setup: last_accessed_at = %v, want the old access kept", art.LastAccessedAt)
	}

	sweepRetention(context.Background(), db, discardLogger(), retentionRegistry(), rules, now.Add(time.Hour))
	if !isCached(t, db, "npm", "popular") {
		t.Error("refetched artifact cleared again by the next sweep")
	}
}

func TestSweepRetentionFlushesBufferedHits(t *testing.T) {
	db, store := setupEvictionTest(t)
	now := time.Now()
	old := now.Add(-30 * retentionDay)
	seedAgedArtifact(t, db, store, "npm", "npm", "busy", old, old)
	db.BatchHits(time.Hour, nil)
	if err := db.RecordArtifactHit("pkg:npm/busy@1.0.0", "busy-1.0.0.tgz"); err != nil {
		t.Fatalf("RecordArtifactHit: %v", err)
	}

	rules := retention.Rules{Ecosystems: map[string]time.Duration{"npm": retentionDay}}
	sweepRetention(context.Background(), db, discardLogger(), retentionRegistry(), rules, time.Now())

	if !isCached(t, db, "npm", "busy") {
		t.Error("artifact with a buffered hit was cleared")
	}
}

func TestSweepRetentionCapsClearsPerSweep(t *testing.T) {
	db, store := setupEvictionTest(t)
	prev := retentionMaxClears
	retentionMaxClears = 2
	t.Cleanup(func() { retentionMaxClears = prev })

	now := time.Now()
	old := now.Add(-30 * retentionDay)
	names := []string{"a", "b", "c"}
	for _, name := range names {
		seedAgedArtifact(t, db, store, "npm", "npm", name, old, old)
	}
	rules := retention.Rules{Ecosystems: map[string]time.Duration{"npm": retentionDay}}

	countCached := func() int {
		n := 0
		for _, name := range names {
			if isCached(t, db, "npm", name) {
				n++
			}
		}
		return n
	}

	sweepRetention(context.Background(), db, discardLogger(), retentionRegistry(), rules, now)
	if got := countCached(); got != 1 {
		t.Fatalf("cached after the first sweep = %d, want 1 (cap of 2)", got)
	}
	sweepRetention(context.Background(), db, discardLogger(), retentionRegistry(), rules, now)
	if got := countCached(); got != 0 {
		t.Errorf("cached after the second sweep = %d, want 0", got)
	}
}

func TestSweepRetentionStopsOnCancel(t *testing.T) {
	db, store := setupEvictionTest(t)
	now := time.Now()
	old := now.Add(-30 * retentionDay)
	seedAgedArtifact(t, db, store, "npm", "npm", "stale", old, old)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rules := retention.Rules{Ecosystems: map[string]time.Duration{"npm": retentionDay}}
	sweepRetention(ctx, db, discardLogger(), retentionRegistry(), rules, now)

	if !isCached(t, db, "npm", "stale") {
		t.Error("cancelled sweep cleared an artifact")
	}
}

func TestSweepRetentionWithoutRulesDoesNothing(t *testing.T) {
	db, store := setupEvictionTest(t)
	old := time.Now().Add(-400 * retentionDay)
	seedAgedArtifact(t, db, store, "npm", "npm", "ancient", old, old)

	sweepRetention(context.Background(), db, discardLogger(), retentionRegistry(), retention.Rules{}, time.Now())

	if !isCached(t, db, "npm", "ancient") {
		t.Error("sweep without rules cleared an artifact")
	}
}

func TestNotCovered(t *testing.T) {
	got := notCovered([]string{"npm", "pypi"})
	if slices.Contains(got, "npm") || slices.Contains(got, "pypi") {
		t.Errorf("notCovered = %v, includes a covered key", got)
	}
	if len(got) != len(retention.KnownKeys)-2 {
		t.Errorf("notCovered returned %d keys, want %d", len(got), len(retention.KnownKeys)-2)
	}
}

func TestReclaimDueDrainsSeveralBatches(t *testing.T) {
	db, store := setupEvictionTest(t)
	const queued = reclaimBatch*2 + 10
	for i := range queued {
		storeQueued(t, db, store, fmt.Sprintf("npm/old/1.0.0/%d/old.tgz", i))
	}

	reclaimDue(context.Background(), db, store, discardLogger(), time.Now().Add(time.Hour), time.Minute)

	remaining, err := db.GetDuePendingDeletes(time.Now().Add(time.Hour), queued)
	if err != nil {
		t.Fatalf("GetDuePendingDeletes: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("%d objects still queued, want the whole queue reclaimed in one tick", len(remaining))
	}
}

func TestReclaimDueRespectsBudget(t *testing.T) {
	db, store := setupEvictionTest(t)
	storeQueued(t, db, store, "npm/old/1.0.0/a/old.tgz")

	reclaimDue(context.Background(), db, store, discardLogger(), time.Now().Add(time.Hour), 0)

	if got := queuedPaths(t, db); len(got) != 1 {
		t.Errorf("queue = %v, want the entry kept with no budget", got)
	}
}

// A hit that lands after the scan but before the clear keeps the artifact:
// the clear re-checks the artifact against its own rule's cutoff.
func TestClearIfExpiredRechecksAgainstItsRule(t *testing.T) {
	db, store := setupEvictionTest(t)
	now := time.Now()
	old := now.Add(-30 * retentionDay)
	seedAgedArtifact(t, db, store, "npm", "npm", "racing", old, old)
	rules := retention.Rules{Ecosystems: map[string]time.Duration{"npm": retentionDay}}

	candidates, err := db.GetRetentionCandidates(0, 10, now.Add(-retentionDay))
	if err != nil || len(candidates) != 1 {
		t.Fatalf("GetRetentionCandidates = %v, %v, want one candidate", candidates, err)
	}
	if err := db.RecordArtifactHit("pkg:npm/racing@1.0.0", "racing-1.0.0.tgz"); err != nil {
		t.Fatalf("RecordArtifactHit: %v", err)
	}

	if clearIfExpired(db, discardLogger(), retentionRegistry(), rules, now.Add(time.Minute), candidates[0]) {
		t.Error("cleared an artifact served after the scan")
	}
	if !isCached(t, db, "npm", "racing") {
		t.Error("artifact served after the scan is no longer cached")
	}
}

// When buffered hits cannot be written, the sweep must not run on the stale
// access times they would have replaced.
func TestSweepRetentionSkipsWhenHitsCannotBeWritten(t *testing.T) {
	db, store := setupEvictionTest(t)
	now := time.Now()
	old := now.Add(-30 * retentionDay)
	seedAgedArtifact(t, db, store, "npm", "npm", "busy", old, old)
	db.BatchHits(time.Hour, nil)
	if err := db.RecordArtifactHit("pkg:npm/busy@1.0.0", "busy-1.0.0.tgz"); err != nil {
		t.Fatalf("RecordArtifactHit: %v", err)
	}
	// Refuse hit writes only; clearing a record leaves hit_count alone.
	if _, err := db.Exec(`CREATE TRIGGER refuse_hits BEFORE UPDATE ON artifacts
		WHEN NEW.hit_count > OLD.hit_count BEGIN SELECT RAISE(ABORT, 'hits refused'); END`); err != nil {
		t.Fatalf("creating trigger: %v", err)
	}

	rules := retention.Rules{Ecosystems: map[string]time.Duration{"npm": retentionDay}}
	sweepRetention(context.Background(), db, discardLogger(), retentionRegistry(), rules, now)

	if !isCached(t, db, "npm", "busy") {
		t.Error("sweep cleared an artifact whose buffered hit could not be written")
	}
	if _, err := db.Exec(`DROP TRIGGER refuse_hits`); err != nil {
		t.Fatalf("dropping trigger: %v", err)
	}
}

func TestEvictionsAreCounted(t *testing.T) {
	db, store := setupEvictionTest(t)
	ctx := context.Background()
	now := time.Now()
	old := now.Add(-30 * retentionDay)

	retained := testutil.ToFloat64(metrics.ArtifactsEvicted.WithLabelValues("retention", "rubygems"))
	seedAgedArtifact(t, db, store, "gem", "gem", "expired", old, old)
	rules := retention.Rules{Ecosystems: map[string]time.Duration{"gem": retentionDay}}
	sweepRetention(ctx, db, discardLogger(), retentionRegistry(), rules, now)
	if got := testutil.ToFloat64(metrics.ArtifactsEvicted.WithLabelValues("retention", "rubygems")) - retained; got != 1 {
		t.Errorf("retention evictions counted = %v, want 1", got)
	}

	lru := testutil.ToFloat64(metrics.ArtifactsEvicted.WithLabelValues("lru", "npm"))
	seedArtifact(t, ctx, db, store, "big", 500, now)
	evictLRU(ctx, db, store, discardLogger(), 100)
	if got := testutil.ToFloat64(metrics.ArtifactsEvicted.WithLabelValues("lru", "npm")) - lru; got != 1 {
		t.Errorf("lru evictions counted = %v, want 1", got)
	}
}

func TestReclaimDueStopsAfterAFailedDelete(t *testing.T) {
	db, store := setupEvictionTest(t)
	queued := reclaimBatch * 2
	for i := range queued {
		storeQueued(t, db, store, fmt.Sprintf("npm/old/1.0.0/%d/old.tgz", i))
	}

	undeletable := &undeletableStorage{Storage: store}
	reclaimDue(context.Background(), db, undeletable, discardLogger(), time.Now().Add(time.Hour), time.Minute)

	if got := undeletable.deletes.Load(); got != reclaimBatch {
		t.Errorf("delete attempts = %d, want one batch of %d during an outage", got, reclaimBatch)
	}
}
