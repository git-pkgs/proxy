package database

import (
	"database/sql"
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
// cache key. FetchedAt is only read for artifacts.
type CacheEntry struct {
	Kind           string         `db:"kind"`
	Key            string         `db:"cache_key"`
	Name           string         `db:"name"`
	StoragePath    sql.NullString `db:"storage_path"`
	Size           sql.NullInt64  `db:"size"`
	LastAccessedAt sql.NullTime   `db:"last_accessed_at"`
	FetchedAt      sql.NullTime   `db:"fetched_at"`
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
		       storage_path, size, last_accessed_at
		FROM metadata_cache
		ORDER BY last_accessed_at ASC NULLS FIRST
		LIMIT ?
	`), limit); err != nil {
		return nil, err
	}

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
	return e.FetchedAt
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
// storagePath and has not been used since lastAccessed, and reports whether
// it did. A row a request refreshed or served in the meantime is kept.
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
	res, err := db.Exec(db.Rebind(query), args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
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
