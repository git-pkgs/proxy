package database

import (
	"database/sql"
	"errors"
	"time"
)

// ImageTag is a tag of a container image repository as the proxy last saw
// it upstream.
type ImageTag struct {
	Tag    string `db:"tag"`
	Digest string `db:"digest"`
	// SeenAt is when the upstream last said the tag points at Digest.
	SeenAt time.Time `db:"-"`
}

type imageTagRow struct {
	Tag    string `db:"tag"`
	Digest string `db:"digest"`
	SeenAt int64  `db:"seen_at"`
}

// RecordImageTag stores that a tag of a repository pointed at digest at the
// given time. An older sighting than the stored one changes nothing.
func (db *DB) RecordImageTag(packagePURL, tag, digest string, seenAt time.Time) error {
	_, err := db.Exec(db.Rebind(`
		INSERT INTO image_tags (package_purl, tag, digest, seen_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (package_purl, tag) DO UPDATE SET digest = excluded.digest, seen_at = excluded.seen_at
		WHERE image_tags.seen_at < excluded.seen_at
	`), packagePURL, tag, digest, seenAt.UnixMilli())
	return err
}

// ImageTags returns up to limit tags of a repository, the most recently seen
// first.
func (db *DB) ImageTags(packagePURL string, limit int) ([]ImageTag, error) {
	var rows []imageTagRow
	err := db.Select(&rows, db.Rebind(`
		SELECT tag, digest, seen_at FROM image_tags WHERE package_purl = ?
		ORDER BY seen_at DESC, tag LIMIT ?
	`), packagePURL, limit)
	if err != nil {
		return nil, err
	}
	tags := make([]ImageTag, len(rows))
	for i, row := range rows {
		tags[i] = ImageTag{Tag: row.Tag, Digest: row.Digest, SeenAt: time.UnixMilli(row.SeenAt)}
	}
	return tags, nil
}

// RecordImageManifestReferences stores the digests a manifest of a
// repository references: the platform manifests of an index, or the config
// and layers of an image. A digest fixes the manifest's content, so a
// manifest already recorded is left as it is.
func (db *DB) RecordImageManifestReferences(packagePURL, manifestDigest string, references []string) error {
	var known int
	err := db.Get(&known, db.Rebind(`
		SELECT 1 FROM image_manifest_references WHERE package_purl = ? AND manifest_digest = ? LIMIT 1
	`), packagePURL, manifestDigest)
	if err == nil || !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	insert := db.Rebind(`
		INSERT INTO image_manifest_references (package_purl, manifest_digest, reference_digest) VALUES (?, ?, ?)
		ON CONFLICT DO NOTHING
	`)
	for _, reference := range references {
		if _, err := tx.Exec(insert, packagePURL, manifestDigest, reference); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ImageTagsContaining returns up to limit tags of a repository whose image
// contains digest, the most recently seen first: tags pointing at digest,
// at a manifest that references it, or at an index listing such a manifest.
func (db *DB) ImageTagsContaining(packagePURL, digest string, limit int) ([]string, error) {
	var tags []string
	err := db.Select(&tags, db.Rebind(`
		SELECT tag FROM image_tags WHERE package_purl = ? AND (digest = ? OR digest IN (
			SELECT manifest_digest FROM image_manifest_references
			WHERE package_purl = ? AND reference_digest = ?
			UNION
			SELECT parent.manifest_digest FROM image_manifest_references child
			JOIN image_manifest_references parent
				ON parent.package_purl = child.package_purl AND parent.reference_digest = child.manifest_digest
			WHERE child.package_purl = ? AND child.reference_digest = ?
		))
		ORDER BY seen_at DESC, tag LIMIT ?
	`), packagePURL, digest, packagePURL, digest, packagePURL, digest, limit)
	return tags, err
}
