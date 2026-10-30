package database

import (
	"database/sql"
	"fmt"
	"math"
	"time"
)

// MetadataSnapshot is one kept version of a metadata document, so a request
// for a past date can be answered with the document that was current then.
// It is valid from ValidFromMS (Unix milliseconds, the time the proxy first
// stored it) until the next snapshot of the same ecosystem and name.
type MetadataSnapshot struct {
	ID              int64          `db:"id"`
	Ecosystem       string         `db:"ecosystem"`
	Name            string         `db:"name"`
	StoragePath     string         `db:"storage_path"`
	ContentType     sql.NullString `db:"content_type"`
	ContentEncoding sql.NullString `db:"content_encoding"`
	ContentDigest   string         `db:"content_digest"`
	Size            int64          `db:"size"`
	ValidFromMS     int64          `db:"valid_from_ms"`
}

// ValidFrom returns the time the snapshot became current.
func (s *MetadataSnapshot) ValidFrom() time.Time {
	return time.UnixMilli(s.ValidFromMS)
}

const metadataSnapshotColumns = `id, ecosystem, name, storage_path, content_type, content_encoding,
	content_digest, size, valid_from_ms`

// MetadataSnapshotAt returns the snapshot of ecosystem and name that was
// current at at: the newest one that became current before it. It returns nil
// when there is none.
func (db *DB) MetadataSnapshotAt(ecosystem, name string, at time.Time) (*MetadataSnapshot, error) {
	return db.metadataSnapshotBefore(ecosystem, name, at.UnixMilli())
}

// LatestMetadataSnapshot returns the newest snapshot of ecosystem and name,
// or nil when there is none.
func (db *DB) LatestMetadataSnapshot(ecosystem, name string) (*MetadataSnapshot, error) {
	return db.metadataSnapshotBefore(ecosystem, name, math.MaxInt64)
}

func (db *DB) metadataSnapshotBefore(ecosystem, name string, beforeMS int64) (*MetadataSnapshot, error) {
	var snap MetadataSnapshot
	err := db.Get(&snap, db.Rebind(`SELECT `+metadataSnapshotColumns+`
		FROM metadata_snapshots
		WHERE ecosystem = ? AND name = ? AND valid_from_ms < ?
		ORDER BY valid_from_ms DESC, id DESC
		LIMIT 1`), ecosystem, name, beforeMS)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading metadata snapshot: %w", err)
	}
	return &snap, nil
}

// AddMetadataSnapshot records snap as the current version of its document,
// unless the newest snapshot already has its digest, and reports whether it
// did. On Postgres, replicas recording the same document take an advisory
// lock on it first, so their check and insert don't interleave: without it,
// two transactions can both read the previous digest and both insert. SQLite
// runs one write at a time, so the single statement is enough there. A
// document going back to an earlier digest is recorded again, since only the
// newest snapshot is compared. The casts let Postgres type the parameters of
// the INSERT ... SELECT.
func (db *DB) AddMetadataSnapshot(snap *MetadataSnapshot) (bool, error) {
	tx, err := db.Beginx()
	if err != nil {
		return false, fmt.Errorf("recording metadata snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if db.dialect == DialectPostgres {
		if _, err := tx.Exec("SELECT pg_advisory_xact_lock($1)", metadataSnapshotLockKey(snap.Ecosystem, snap.Name)); err != nil {
			return false, fmt.Errorf("locking metadata snapshots: %w", err)
		}
	}
	res, err := tx.Exec(db.Rebind(`
		INSERT INTO metadata_snapshots (ecosystem, name, storage_path, content_type, content_encoding,
		                                content_digest, size, valid_from_ms)
		SELECT CAST(? AS TEXT), CAST(? AS TEXT), CAST(? AS TEXT), CAST(? AS TEXT), CAST(? AS TEXT),
		       CAST(? AS TEXT), CAST(? AS BIGINT), CAST(? AS BIGINT)
		WHERE COALESCE((
			SELECT content_digest FROM metadata_snapshots
			WHERE ecosystem = ? AND name = ?
			ORDER BY valid_from_ms DESC, id DESC
			LIMIT 1
		), '') <> CAST(? AS TEXT)
	`), snap.Ecosystem, snap.Name, snap.StoragePath, snap.ContentType, snap.ContentEncoding,
		snap.ContentDigest, snap.Size, snap.ValidFromMS,
		snap.Ecosystem, snap.Name, snap.ContentDigest)
	if err != nil {
		return false, fmt.Errorf("recording metadata snapshot: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("recording metadata snapshot: %w", err)
	}
	return n > 0, nil
}

// metadataSnapshotLockKey is the advisory lock AddMetadataSnapshot takes for
// a document.
func metadataSnapshotLockKey(ecosystem, name string) int64 {
	return lockKey("metadata-snapshot:" + ecosystem + "\x00" + name)
}

// PruneMetadataSnapshots deletes the snapshots of ecosystem that stopped
// being current before cutoff, that is those replaced by a newer snapshot
// that became current before cutoff, and queues their blobs for deletion. The
// newest snapshot of each document is always kept. It returns the number of
// snapshots deleted.
func (db *DB) PruneMetadataSnapshots(ecosystem string, cutoff time.Time) (int, error) {
	tx, err := db.Beginx()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var expired []struct {
		ID          int64  `db:"id"`
		StoragePath string `db:"storage_path"`
	}
	err = tx.Select(&expired, db.Rebind(`
		SELECT s.id, s.storage_path FROM metadata_snapshots s
		WHERE s.ecosystem = ?
		  AND EXISTS (
			SELECT 1 FROM metadata_snapshots n
			WHERE n.ecosystem = s.ecosystem AND n.name = s.name
			  AND (n.valid_from_ms > s.valid_from_ms OR (n.valid_from_ms = s.valid_from_ms AND n.id > s.id))
			  AND n.valid_from_ms < ?
		  )
	`), ecosystem, cutoff.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("listing expired metadata snapshots: %w", err)
	}

	queuedAt := time.Now().UTC()
	for _, row := range expired {
		if _, err := tx.Exec(db.Rebind(`DELETE FROM metadata_snapshots WHERE id = ?`), row.ID); err != nil {
			return 0, fmt.Errorf("deleting metadata snapshot: %w", err)
		}
		if _, err := tx.Exec(db.Rebind(queuePendingDeleteQuery), row.StoragePath, queuedAt); err != nil {
			return 0, fmt.Errorf("queueing metadata snapshot blob: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(expired), nil
}
