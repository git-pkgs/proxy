package database

import (
	"database/sql"
	"errors"
	"time"
)

// RetentionCandidate is a cached artifact that has not been accessed since a
// cutoff, together with the package record it belongs to.
type RetentionCandidate struct {
	ID             int64         `db:"id"`
	VersionPURL    string        `db:"version_purl"`
	Filename       string        `db:"filename"`
	StoragePath    string        `db:"storage_path"`
	Size           sql.NullInt64 `db:"size"`
	FetchedAt      sql.NullTime  `db:"fetched_at"`
	LastAccessedAt sql.NullTime  `db:"last_accessed_at"`
	Ecosystem      string        `db:"ecosystem"`
	PackagePURL    string        `db:"package_purl"`
}

// ExpiredBefore reports whether the artifact was neither fetched nor
// accessed at or after cutoff. Both are checked: clearing a record keeps its
// last access time, so a record fetched again after an eviction has a fresh
// fetch time next to an old access time.
func (c RetentionCandidate) ExpiredBefore(cutoff time.Time) bool {
	if !c.FetchedAt.Valid || !c.FetchedAt.Time.Before(cutoff) {
		return false
	}
	return !c.LastAccessedAt.Valid || c.LastAccessedAt.Time.Before(cutoff)
}

// expiredCondition selects cached artifacts fetched before the cutoff and not
// accessed since. It takes the cutoff twice.
const expiredCondition = `
	storage_path IS NOT NULL
	AND fetched_at < ?
	AND (last_accessed_at IS NULL OR last_accessed_at < ?)`

// MaxArtifactID returns the highest artifact id, or zero for an empty table.
func (db *DB) MaxArtifactID() (int64, error) {
	var maxID sql.NullInt64
	if err := db.Get(&maxID, `SELECT MAX(id) FROM artifacts`); err != nil {
		return 0, err
	}
	return maxID.Int64, nil
}

// GetRetentionCandidates returns the cached artifacts with fromID < id <= toID
// that expired before cutoff. Bounding the scan by id keeps each query short,
// which matters on SQLite, where every request shares one connection.
func (db *DB) GetRetentionCandidates(fromID, toID int64, cutoff time.Time) ([]RetentionCandidate, error) {
	var candidates []RetentionCandidate
	query := db.Rebind(`
		SELECT a.id, a.version_purl, a.filename, a.storage_path, a.size,
		       a.fetched_at, a.last_accessed_at, p.ecosystem, p.purl AS package_purl
		FROM (
			SELECT * FROM artifacts
			WHERE id > ? AND id <= ? AND` + expiredCondition + `
		) a
		JOIN versions v ON v.purl = a.version_purl
		JOIN packages p ON p.purl = v.package_purl
		ORDER BY a.id
	`)
	if err := db.Select(&candidates, query, fromID, toID, cutoff, cutoff); err != nil {
		return nil, err
	}
	if db.dialect == DialectPostgres {
		for i := range candidates {
			candidates[i].FetchedAt = asLocalWallClock(candidates[i].FetchedAt)
			candidates[i].LastAccessedAt = asLocalWallClock(candidates[i].LastAccessedAt)
		}
	}
	return candidates, nil
}

// asLocalWallClock reinterprets a time read from a Postgres TIMESTAMP
// column. The column has no zone and holds the local wall clock time the
// proxy wrote, but lib/pq hands it back labelled UTC, which east of UTC
// makes it look hours newer than it is.
func asLocalWallClock(t sql.NullTime) sql.NullTime {
	if !t.Valid {
		return t
	}
	v := t.Time
	t.Time = time.Date(v.Year(), v.Month(), v.Day(), v.Hour(), v.Minute(), v.Second(), v.Nanosecond(), time.Local)
	return t
}

// ClearExpiredArtifact marks an artifact uncached like ClearArtifactCache,
// but only while it is still expired before cutoff, so an artifact served
// between the scan and the clear stays cached.
func (db *DB) ClearExpiredArtifact(versionPURL, filename, storagePath string, cutoff time.Time) (bool, error) {
	query := db.Rebind(`
		UPDATE artifacts
		SET storage_path = NULL, content_hash = NULL, size = NULL,
		    content_type = NULL, fetched_at = NULL, updated_at = ?
		WHERE version_purl = ? AND filename = ? AND storage_path = ? AND` + expiredCondition)
	res, err := db.Exec(query, time.Now(), versionPURL, filename, storagePath, cutoff, cutoff)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// GetArtifactEcosystem returns the packages.ecosystem value of the package an
// artifact version belongs to, or "" when it has no package record.
func (db *DB) GetArtifactEcosystem(versionPURL string) (string, error) {
	var ecosystem string
	query := db.Rebind(`
		SELECT p.ecosystem FROM versions v
		JOIN packages p ON p.purl = v.package_purl
		WHERE v.purl = ?
	`)
	err := db.Get(&ecosystem, query, versionPURL)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return ecosystem, err
}

// FlushHits writes hits buffered by BatchHits now, so a caller about to read
// last_accessed_at sees them.
func (db *DB) FlushHits() error {
	if db.hits == nil {
		return nil
	}
	return db.flushHits()
}
