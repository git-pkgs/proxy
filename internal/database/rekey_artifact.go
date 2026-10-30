package database

import (
	"fmt"
	"time"
)

// RekeyCachedArtifact moves one unchanged upstream record without replacing
// an existing destination. Access history and stored bytes are preserved.
func (db *DB) RekeyCachedArtifact(old *Artifact, pkg *Package, version *Version) (bool, error) {
	tx, err := db.Beginx()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now()
	_, err = tx.Exec(db.Rebind(`INSERT INTO packages (purl, ecosystem, name, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(purl) DO NOTHING`), pkg.PURL, pkg.Ecosystem, pkg.Name, now, now)
	if err != nil {
		return false, fmt.Errorf("creating artifact package: %w", err)
	}
	_, err = tx.Exec(db.Rebind(`INSERT INTO versions (purl, package_purl, created_at, updated_at)
		VALUES (?, ?, ?, ?) ON CONFLICT(purl) DO NOTHING`), version.PURL, version.PackagePURL, now, now)
	if err != nil {
		return false, fmt.Errorf("creating artifact version: %w", err)
	}
	result, err := tx.Exec(db.Rebind(`UPDATE artifacts SET version_purl = ?, updated_at = ?
		WHERE id = ? AND version_purl = ? AND storage_path = ? AND upstream_url = ? AND source = ?
		AND NOT EXISTS (SELECT 1 FROM artifacts WHERE version_purl = ? AND filename = ?)`),
		version.PURL, now, old.ID, old.VersionPURL, old.StoragePath, old.UpstreamURL, SourceUpstream,
		version.PURL, old.Filename)
	if err != nil {
		return false, fmt.Errorf("moving cached artifact: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil || count == 0 {
		return false, err
	}
	return true, tx.Commit()
}
