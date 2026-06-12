package scanner

import (
	"context"
	"database/sql"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
)

// Cache wraps the DB-backed artifact_scans / artifact_findings tables to
// short-circuit re-scanning content the pipeline has already seen.
type Cache struct {
	db  *database.DB
	ttl time.Duration
}

// NewCache returns a Cache that considers scan rows fresh for ttl.
// A ttl of 0 disables expiry (any recorded scan counts as fresh).
func NewCache(db *database.DB, ttl time.Duration) *Cache {
	return &Cache{db: db, ttl: ttl}
}

// Lookup returns previously recorded findings for (scanner, contentHash)
// when a scan row exists and is still within ttl. The second return is
// false when no usable cache entry exists.
func (c *Cache) Lookup(scannerName, contentHash string) ([]Finding, bool) {
	if c == nil || c.db == nil || contentHash == "" {
		return nil, false
	}
	scan, err := c.db.GetArtifactScan(contentHash, scannerName)
	if err != nil || scan == nil {
		return nil, false
	}
	if c.ttl > 0 && time.Since(scan.ScannedAt) > c.ttl {
		return nil, false
	}
	if scan.Status != "ok" {
		return nil, false
	}
	rows, err := c.db.GetFindingsByContentHash(contentHash, scannerName)
	if err != nil {
		return nil, false
	}
	findings := make([]Finding, 0, len(rows))
	for _, r := range rows {
		findings = append(findings, dbFindingToFinding(r))
	}
	return findings, true
}

// Record persists the result of a single scanner run against contentHash.
// findings may be nil/empty for a clean scan; scanErr is set when the
// scanner failed under fail-open mode (we still want to remember the
// content was attempted so the next ingest can re-try lazily).
func (c *Cache) Record(scannerName, contentHash string, _ []Finding, scanErr error) {
	if c == nil || c.db == nil || contentHash == "" {
		return
	}
	status := "ok"
	var errStr sql.NullString
	if scanErr != nil {
		status = "error"
		errStr = sql.NullString{String: scanErr.Error(), Valid: true}
	}
	_ = c.db.UpsertArtifactScan(&database.ArtifactScan{
		ContentHash: contentHash,
		Scanner:     scannerName,
		Status:      status,
		Error:       errStr,
		ScannedAt:   time.Now().UTC(),
	})
}

// dbFindingToFinding converts a database row back into a runtime Finding.
func dbFindingToFinding(r database.ArtifactFinding) Finding {
	f := Finding{
		Scanner:  r.Scanner,
		ID:       r.FindingID,
		Severity: ParseSeverity(r.Severity),
	}
	if r.Summary.Valid {
		f.Summary = r.Summary.String
	}
	if r.FixedVersion.Valid {
		f.FixedVersion = r.FixedVersion.String
	}
	if r.References.Valid {
		f.References = splitReferences(r.References.String)
	}
	if r.Raw.Valid {
		f.Raw = r.Raw.String
	}
	return f
}

// LookupCached is a convenience wrapper for callers that want to peek at
// the cache without holding a Pipeline reference.
func LookupCached(ctx context.Context, c *Cache, scannerName, contentHash string) ([]Finding, bool) {
	_ = ctx
	return c.Lookup(scannerName, contentHash)
}
