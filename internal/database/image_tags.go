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

// ImageTagsContaining returns up to limit tags of a repository whose image
// contains digest, the most recently seen first: tags pointing at digest,
// at a manifest that references it, or at an index listing such a manifest.
// It reads the references recorded in image_manifests.
func (db *DB) ImageTagsContaining(packagePURL, digest string, limit int) ([]string, error) {
	var manifests []imageManifestRow
	err := db.Select(&manifests, db.Rebind(`
		SELECT digest, refs FROM image_manifests WHERE package_purl = ?
	`), packagePURL)
	if err != nil {
		return nil, err
	}
	referencedBy := make(map[string][]string)
	for _, manifest := range manifests {
		for _, ref := range manifest.refs() {
			referencedBy[ref] = append(referencedBy[ref], manifest.Digest)
		}
	}
	containing := map[string]struct{}{digest: {}}
	for next := []string{digest}; len(next) > 0; {
		var parents []string
		for _, child := range next {
			for _, parent := range referencedBy[child] {
				if _, seen := containing[parent]; !seen {
					containing[parent] = struct{}{}
					parents = append(parents, parent)
				}
			}
		}
		next = parents
	}

	var rows []imageTagRow
	err = db.Select(&rows, db.Rebind(`
		SELECT tag, digest, seen_at FROM image_tags WHERE package_purl = ? ORDER BY seen_at DESC, tag
	`), packagePURL)
	if err != nil {
		return nil, err
	}
	var tags []string
	for _, row := range rows {
		if len(tags) == limit {
			break
		}
		if _, ok := containing[row.Digest]; ok {
			tags = append(tags, row.Tag)
		}
	}
	return tags, nil
}
