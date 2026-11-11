package database

import (
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
