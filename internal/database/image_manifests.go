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

// imagePruneBatch is how many manifests one prune query deletes, well below
// the bind limits of SQLite and PostgreSQL. A variable so tests can lower it.
var imagePruneBatch = 500

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
// before it moves the tag time forward, and fills in the references if none
// were known: a tag seen through a HEAD is recorded before its manifest is
// read.
func (db *DB) RecordImageManifest(m ImageManifest) error {
	tagged := int64(0)
	if !m.TaggedAt.IsZero() {
		tagged = m.TaggedAt.UnixMilli()
	}
	_, err := db.Exec(db.Rebind(`
		INSERT INTO image_manifests (package_purl, digest, refs, tagged_at, recorded_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (package_purl, digest) DO UPDATE SET
			tagged_at = CASE WHEN excluded.tagged_at > image_manifests.tagged_at THEN excluded.tagged_at ELSE image_manifests.tagged_at END,
			refs = CASE WHEN image_manifests.refs = '' THEN excluded.refs ELSE image_manifests.refs END
		WHERE image_manifests.tagged_at < excluded.tagged_at OR (image_manifests.refs = '' AND excluded.refs <> '')
	`), m.PackagePURL, m.Digest, strings.Join(m.Refs, "\n"), tagged, time.Now().UnixMilli())
	return err
}

// ImageManifestState returns when a tag was last found to point at a
// manifest, zero if none was, whether the manifest is recorded at all, and
// whether its references are known.
func (db *DB) ImageManifestState(packagePURL, digest string) (tagged time.Time, recorded, hasRefs bool, err error) {
	var row imageManifestRow
	err = db.Get(&row, db.Rebind(`
		SELECT digest, refs, tagged_at FROM image_manifests WHERE package_purl = ? AND digest = ?
	`), packagePURL, digest)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, false, nil
	}
	if err != nil {
		return time.Time{}, false, false, err
	}
	if row.TaggedAt != 0 {
		tagged = time.UnixMilli(row.TaggedAt)
	}
	return tagged, true, row.Refs != "", nil
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

// PruneImageManifests removes the manifests of a repository that neither
// its n newest images nor the images its tags point at reference, and that
// were recorded before the given time. The time leaves a manifest recorded
// shortly before the one that references it in place. What the tags point
// at stays for ImageTagsContaining.
func (db *DB) PruneImageManifests(packagePURL string, n int, before time.Time) error {
	roots, err := db.newestImages(packagePURL, n)
	if err != nil {
		return err
	}
	var tagged []imageManifestRow
	err = db.Select(&tagged, db.Rebind(`
		SELECT digest, refs FROM image_manifests WHERE package_purl = ?
			AND digest IN (SELECT digest FROM image_tags WHERE package_purl = ?)
	`), packagePURL, packagePURL)
	if err != nil {
		return err
	}
	_, manifests, err := db.walkImages(packagePURL, append(roots, tagged...))
	if err != nil {
		return err
	}
	keep := make(map[string]struct{}, len(manifests))
	for _, digest := range manifests {
		keep[digest] = struct{}{}
	}

	var old []string
	err = db.Select(&old, db.Rebind(`
		SELECT digest FROM image_manifests WHERE package_purl = ? AND recorded_at < ?
	`), packagePURL, before.UnixMilli())
	if err != nil {
		return err
	}
	drop := old[:0]
	for _, digest := range old {
		if _, ok := keep[digest]; !ok {
			drop = append(drop, digest)
		}
	}
	// Deleted by name in batches: a list of the kept ones can outgrow what
	// one query may bind.
	for start := 0; start < len(drop); start += imagePruneBatch {
		batch := drop[start:min(start+imagePruneBatch, len(drop))]
		query, args, err := sqlx.In(`DELETE FROM image_manifests WHERE package_purl = ? AND digest IN (?)`, packagePURL, batch)
		if err != nil {
			return fmt.Errorf("building prune query: %w", err)
		}
		if _, err := db.Exec(db.Rebind(query), args...); err != nil {
			return err
		}
	}
	return nil
}

// ImageManifestRefs returns the digests a recorded manifest of a repository
// references, or none for one that is not recorded.
func (db *DB) ImageManifestRefs(packagePURL, digest string) ([]string, error) {
	var row imageManifestRow
	err := db.Get(&row, db.Rebind(`
		SELECT digest, refs FROM image_manifests WHERE package_purl = ? AND digest = ?
	`), packagePURL, digest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return row.refs(), nil
}

// walkNewestImages returns every digest the n newest images of a repository
// reference, themselves included, and those among them that are recorded
// manifests.
func (db *DB) walkNewestImages(packagePURL string, n int) (map[string]struct{}, []string, error) {
	rows, err := db.newestImages(packagePURL, n)
	if err != nil {
		return nil, nil, err
	}
	return db.walkImages(packagePURL, rows)
}

// walkImages returns every digest the given manifests of a repository
// reference, themselves included, and those among them that are recorded
// manifests.
func (db *DB) walkImages(packagePURL string, roots []imageManifestRow) (map[string]struct{}, []string, error) {
	digests := map[string]struct{}{}
	for _, row := range roots {
		digests[row.Digest] = struct{}{}
	}

	var manifests []string
	var err error
	for depth, rows := 0, roots; len(rows) > 0; depth++ {
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
	if n <= 0 {
		return nil, nil
	}
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
