package database

import (
	"cmp"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

const (
	// imageWalkDepth is how many levels of references NewestImageDigests
	// follows below an image: nested indexes, a platform manifest, its blobs.
	imageWalkDepth = 4
	// imageWalkBatch is how many digests one query looks up.
	imageWalkBatch = 500
)

// ImageManifest is what one container image manifest of a repository
// references.
type ImageManifest struct {
	// PackagePURL is the repository as its blobs are cached.
	PackagePURL string
	Digest      string
	// Refs are the digests of the manifests or blobs it references.
	Refs []string
	// TaggedAt is when a tag was last found to point at the manifest, or
	// zero for one only asked for by digest.
	TaggedAt time.Time
}

type imageManifestRow struct {
	Digest   string `db:"digest"`
	Refs     string `db:"refs"`
	TaggedAt int64  `db:"tagged_at"`
}

func (r imageManifestRow) refs() []string {
	return strings.Fields(r.Refs)
}

// RecordImageManifest stores a manifest's references. For one recorded
// before it only moves the tag time forward.
func (db *DB) RecordImageManifest(m ImageManifest) error {
	tagged := int64(0)
	if !m.TaggedAt.IsZero() {
		tagged = m.TaggedAt.UnixMilli()
	}
	_, err := db.Exec(db.Rebind(`
		INSERT INTO image_manifests (package_purl, digest, refs, tagged_at, recorded_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (package_purl, digest) DO UPDATE SET tagged_at = excluded.tagged_at
		WHERE image_manifests.tagged_at < excluded.tagged_at
	`), m.PackagePURL, m.Digest, strings.Join(m.Refs, "\n"), tagged, time.Now().UnixMilli())
	return err
}

// ImageManifestTaggedAt returns when a tag was last found to point at a
// manifest, zero if none was, and whether the manifest is recorded at all.
func (db *DB) ImageManifestTaggedAt(packagePURL, digest string) (time.Time, bool, error) {
	var tagged int64
	err := db.Get(&tagged, db.Rebind(`
		SELECT tagged_at FROM image_manifests WHERE package_purl = ? AND digest = ?
	`), packagePURL, digest)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil || tagged == 0 {
		return time.Time{}, err == nil, err
	}
	return time.UnixMilli(tagged), true, nil
}

// NewestImageDigests returns the digests of the n images of a repository
// that a tag was most recently found to point at, and of every manifest and
// blob they reference.
func (db *DB) NewestImageDigests(packagePURL string, n int) (map[string]struct{}, error) {
	digests, _, err := db.walkNewestImages(packagePURL, n)
	return digests, err
}

// ImageManifestRepositories returns the repositories that have recorded
// manifests.
func (db *DB) ImageManifestRepositories() ([]string, error) {
	var repositories []string
	err := db.Select(&repositories, `SELECT DISTINCT package_purl FROM image_manifests`)
	return repositories, err
}

// PruneImageManifests removes the manifests of a repository that its n
// newest images do not reference and that were recorded before the given
// time. The time leaves a manifest recorded shortly before the one that
// references it in place.
func (db *DB) PruneImageManifests(packagePURL string, n int, before time.Time) error {
	_, manifests, err := db.walkNewestImages(packagePURL, n)
	if err != nil {
		return err
	}
	query := `DELETE FROM image_manifests WHERE package_purl = ? AND recorded_at < ?`
	args := []any{packagePURL, before.UnixMilli()}
	if len(manifests) > 0 {
		query, args, err = sqlx.In(query+` AND digest NOT IN (?)`, packagePURL, before.UnixMilli(), manifests)
		if err != nil {
			return fmt.Errorf("building prune query: %w", err)
		}
	}
	_, err = db.Exec(db.Rebind(query), args...)
	return err
}

// walkNewestImages returns every digest the n newest images of a repository
// reference, themselves included, and those among them that are recorded
// manifests.
func (db *DB) walkNewestImages(packagePURL string, n int) (map[string]struct{}, []string, error) {
	digests := map[string]struct{}{}
	if n <= 0 {
		return digests, nil, nil
	}
	rows, err := db.newestImages(packagePURL, n)
	if err != nil {
		return nil, nil, err
	}

	var manifests []string
	for _, row := range rows {
		digests[row.Digest] = struct{}{}
	}
	for depth := 0; len(rows) > 0; depth++ {
		var next []string
		for _, row := range rows {
			manifests = append(manifests, row.Digest)
			for _, ref := range row.refs() {
				if _, seen := digests[ref]; seen {
					continue
				}
				digests[ref] = struct{}{}
				next = append(next, ref)
			}
		}
		if depth == imageWalkDepth {
			break
		}
		if rows, err = db.imageManifestRows(packagePURL, next); err != nil {
			return nil, nil, err
		}
	}
	return digests, manifests, nil
}

// newestImages returns the n tagged manifests of a repository whose tag was
// seen last. A registry serves a tag as an index to one client and as one
// platform's manifest to a client that accepts no index, so a tagged
// manifest that another tagged one references is that image seen another
// way: it is no image of its own and lends the other its tag time.
func (db *DB) newestImages(packagePURL string, n int) ([]imageManifestRow, error) {
	var rows []imageManifestRow
	err := db.Select(&rows, db.Rebind(`
		SELECT digest, refs, tagged_at FROM image_manifests
		WHERE package_purl = ? AND tagged_at > 0
	`), packagePURL)
	if err != nil {
		return nil, err
	}

	tagged := make(map[string]int64, len(rows))
	for _, row := range rows {
		tagged[row.Digest] = row.TaggedAt
	}
	seen := maps.Clone(tagged)
	referenced := map[string]struct{}{}
	for _, row := range rows {
		for _, ref := range row.refs() {
			at, ok := tagged[ref]
			if !ok || ref == row.Digest {
				continue
			}
			referenced[ref] = struct{}{}
			seen[row.Digest] = max(seen[row.Digest], at)
		}
	}

	images := rows[:0:0]
	for _, row := range rows {
		if _, ok := referenced[row.Digest]; !ok {
			images = append(images, row)
		}
	}
	if len(images) == 0 {
		// Manifests that only reference each other; no registry serves these.
		images = rows
	}
	slices.SortFunc(images, func(a, b imageManifestRow) int {
		return cmp.Or(
			cmp.Compare(seen[b.Digest], seen[a.Digest]),
			cmp.Compare(b.TaggedAt, a.TaggedAt),
			cmp.Compare(a.Digest, b.Digest),
		)
	})
	return images[:min(n, len(images))], nil
}

// imageManifestRows returns the recorded manifests among digests.
func (db *DB) imageManifestRows(packagePURL string, digests []string) ([]imageManifestRow, error) {
	var rows []imageManifestRow
	for start := 0; start < len(digests); start += imageWalkBatch {
		batch := digests[start:min(start+imageWalkBatch, len(digests))]
		query, args, err := sqlx.In(`
			SELECT digest, refs FROM image_manifests WHERE package_purl = ? AND digest IN (?)
		`, packagePURL, batch)
		if err != nil {
			return nil, fmt.Errorf("building image manifest query: %w", err)
		}
		var found []imageManifestRow
		if err := db.Select(&found, db.Rebind(query), args...); err != nil {
			return nil, err
		}
		rows = append(rows, found...)
	}
	return rows, nil
}
