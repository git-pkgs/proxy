package database

import (
	"database/sql"
	"slices"
	"time"
)

// Kinds of CacheEntry.
const (
	CacheEntryArtifact = "artifact"
	CacheEntryMetadata = "metadata"
)

// CacheEntry is a cached object size eviction can remove: an artifact fetched
// from upstream or a metadata document. For an artifact Key is its version
// PURL and Name its filename; for metadata Key is the ecosystem and Name the
// cache key. FetchedAt is only read for artifacts, UpdatedAt for metadata
// not backfilled yet.
type CacheEntry struct {
	Kind           string         `db:"kind"`
	Key            string         `db:"cache_key"`
	Name           string         `db:"name"`
	StoragePath    sql.NullString `db:"storage_path"`
	Size           sql.NullInt64  `db:"size"`
	LastAccessedAt sql.NullTime   `db:"last_accessed_at"`
	FetchedAt      sql.NullTime   `db:"fetched_at"`
	UpdatedAt      sql.NullTime   `db:"updated_at"`
}

// GetEvictableCacheSize returns the bytes max_size limits: cached upstream
// artifacts and cached metadata documents.
func (db *DB) GetEvictableCacheSize() (int64, error) {
	var total int64
	err := db.Get(&total, `
		SELECT (SELECT COALESCE(SUM(size), 0) FROM artifacts WHERE storage_path IS NOT NULL AND source <> 'local')
		     + (SELECT COALESCE(SUM(size), 0) FROM metadata_cache)`)
	return total, err
}

// GetLeastRecentlyUsedCacheEntries returns up to limit artifacts and metadata
// documents, least recently used first. An artifact counts as used when it
// was fetched until it is first served, so a fresh download does not go
// before every metadata document. Each kind is read in order on its own and
// the two are merged here, which keeps the queries simple for both dialects.
func (db *DB) GetLeastRecentlyUsedCacheEntries(limit int) ([]CacheEntry, error) {
	var artifacts, metadata []CacheEntry
	if err := db.Select(&artifacts, db.Rebind(`
		SELECT 'artifact' AS kind, version_purl AS cache_key, filename AS name,
		       storage_path, size, last_accessed_at, fetched_at
		FROM artifacts
		WHERE storage_path IS NOT NULL AND source <> 'local'
		ORDER BY COALESCE(last_accessed_at, fetched_at) ASC NULLS FIRST
		LIMIT ?
	`), limit); err != nil {
		return nil, err
	}
	if err := db.Select(&metadata, db.Rebind(`
		SELECT 'metadata' AS kind, ecosystem AS cache_key, name,
		       storage_path, size, last_accessed_at, updated_at
		FROM metadata_cache
		ORDER BY COALESCE(last_accessed_at, updated_at) ASC NULLS FIRST
		LIMIT ?
	`), limit); err != nil {
		return nil, err
	}
	// Rows not backfilled yet fall back to updated_at, which Postgres keeps
	// in local time, so the order is fixed up once it is read as UTC.
	for i := range metadata {
		metadata[i].UpdatedAt = db.writtenAtUTC(metadata[i].UpdatedAt)
	}
	slices.SortStableFunc(metadata, func(a, b CacheEntry) int {
		switch {
		case a.usedAfter(b):
			return 1
		case b.usedAfter(a):
			return -1
		}
		return 0
	})

	entries := make([]CacheEntry, 0, min(limit, len(artifacts)+len(metadata)))
	for len(entries) < limit && (len(artifacts) > 0 || len(metadata) > 0) {
		if len(metadata) == 0 || (len(artifacts) > 0 && !artifacts[0].usedAfter(metadata[0])) {
			entries, artifacts = append(entries, artifacts[0]), artifacts[1:]
		} else {
			entries, metadata = append(entries, metadata[0]), metadata[1:]
		}
	}
	return entries, nil
}

// lastUsed is when e was last served, or for an artifact not served since,
// when it was fetched. It is invalid when neither is known.
func (e CacheEntry) lastUsed() sql.NullTime {
	if e.LastAccessedAt.Valid {
		return e.LastAccessedAt
	}
	if e.FetchedAt.Valid {
		return e.FetchedAt
	}
	return e.UpdatedAt
}

// usedAfter reports whether e was used later than other. Entries with no
// time known count as the oldest, as NULLS FIRST orders them.
func (e CacheEntry) usedAfter(other CacheEntry) bool {
	a, b := e.lastUsed(), other.lastUsed()
	if !a.Valid {
		return false
	}
	return !b.Valid || a.Time.After(b.Time)
}

// EvictMetadataCache deletes a metadata cache row if it still points at
// storagePath and has not been used since lastAccessed, queueing the path in
// the same transaction so a crash before the blob is deleted cannot orphan it.
func (db *DB) EvictMetadataCache(ecosystem, name, storagePath string, lastAccessed sql.NullTime) (bool, error) {
	query := `
		DELETE FROM metadata_cache
		WHERE ecosystem = ? AND name = ? AND storage_path = ? AND last_accessed_at IS NULL`
	args := []any{ecosystem, name, storagePath}
	if lastAccessed.Valid {
		query = `
		DELETE FROM metadata_cache
		WHERE ecosystem = ? AND name = ? AND storage_path = ?
		  AND (last_accessed_at IS NULL OR last_accessed_at <= ?)`
		args = append(args, lastAccessed.Time)
	}
	tx, err := db.Beginx()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(db.Rebind(query), args...)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, err
	}
	if _, err := tx.Exec(db.Rebind(queuePendingDeleteQuery), storagePath, time.Now().UTC()); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// RecordMetadataHit marks a cached metadata document as served now. Like
// RecordArtifactHit it is buffered when BatchHits is on.
func (db *DB) RecordMetadataHit(ecosystem, name string) error {
	now := time.Now()
	if db.hits != nil {
		db.hits.addMetadata(metadataHitKey{ecosystem, name}, now)
		return nil
	}
	_, err := db.Exec(db.Rebind(recordMetadataHitQuery), now.UTC(), now.UTC(), ecosystem, name)
	return err
}

// recordMetadataHitQuery takes the access time twice, then the ecosystem and
// key. The time only moves forward, as for artifacts.
const recordMetadataHitQuery = `
	UPDATE metadata_cache
	SET last_accessed_at = CASE WHEN last_accessed_at IS NULL OR last_accessed_at < ? THEN ? ELSE last_accessed_at END
	WHERE ecosystem = ? AND name = ?`

// writtenAtUTC returns a metadata_cache updated_at or created_at as the
// instant it was written. Both were written in local time, and a Postgres
// TIMESTAMP keeps only the wall clock, which lib/pq reads back as UTC.
func (db *DB) writtenAtUTC(t sql.NullTime) sql.NullTime {
	if t.Valid && db.dialect == DialectPostgres {
		w := t.Time
		t.Time = time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), w.Second(), w.Nanosecond(), time.Local)
	}
	if t.Valid {
		t.Time = t.Time.UTC()
	}
	return t
}

// BackfillMetadataLastAccessed sets last_accessed_at from the write time on
// up to limit rows that have none, in one transaction, and reports how many it
// set. Rows served in the meantime keep the time of that hit.
func (db *DB) BackfillMetadataLastAccessed(limit int) (int, error) {
	var rows []struct {
		ID        int64        `db:"id"`
		UpdatedAt sql.NullTime `db:"updated_at"`
		CreatedAt sql.NullTime `db:"created_at"`
	}
	query := db.Rebind(`SELECT id, updated_at, created_at FROM metadata_cache WHERE last_accessed_at IS NULL LIMIT ?`)
	if err := db.Select(&rows, query, limit); err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	tx, err := db.Beginx()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Preparex(db.Rebind(`UPDATE metadata_cache SET last_accessed_at = ? WHERE id = ? AND last_accessed_at IS NULL`))
	if err != nil {
		return 0, err
	}
	defer func() { _ = stmt.Close() }()
	for _, r := range rows {
		at := db.writtenAtUTC(r.UpdatedAt)
		if !at.Valid {
			at = db.writtenAtUTC(r.CreatedAt)
		}
		if !at.Valid {
			// Nothing to go by; counting it as used now at least ends the loop.
			at = sql.NullTime{Time: time.Now().UTC(), Valid: true}
		}
		if _, err := stmt.Exec(at.Time, r.ID); err != nil {
			return 0, err
		}
	}
	return len(rows), tx.Commit()
}
