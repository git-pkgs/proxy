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

// The points of a prune imagePruneHook runs at: after reading the manifests
// old enough to delete, and before deleting those not kept.
const (
	imagePruneRead   = "read"
	imagePruneDelete = "delete"
)

// imagePruneHook, when set, runs at those points of a prune. For tests.
var imagePruneHook func(point string)

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

// imageReader is the database or a transaction on it.
type imageReader interface {
	Select(dest any, query string, args ...any) error
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
//
// A pull can tag one of the manifests while the prune runs. On SQLite the
// transaction holds the only writer, so the pull records its manifest after
// the prune. On PostgreSQL each delete checks that its manifests are as they
// were read and that no tag points at them, and the prune deletes nothing
// when one is not: what it references was not read as kept either.
func (db *DB) PruneImageManifests(packagePURL string, n int, before time.Time) error {
	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Read before what is kept: a manifest tagged after this is either read
	// as kept or no longer as it is here.
	var old []imageManifestRow
	err = tx.Select(&old, db.Rebind(`
		SELECT digest, tagged_at FROM image_manifests WHERE package_purl = ? AND recorded_at < ?
	`), packagePURL, before.UnixMilli())
	if err != nil {
		return err
	}
	if imagePruneHook != nil {
		imagePruneHook(imagePruneRead)
	}

	roots, err := db.newestImages(tx, packagePURL, n)
	if err != nil {
		return err
	}
	var tagged []imageManifestRow
	err = tx.Select(&tagged, db.Rebind(`
		SELECT digest, refs FROM image_manifests WHERE package_purl = ?
			AND digest IN (SELECT digest FROM image_tags WHERE package_purl = ?)
	`), packagePURL, packagePURL)
	if err != nil {
		return err
	}
	_, manifests, err := db.walkImages(tx, packagePURL, append(roots, tagged...))
	if err != nil {
		return err
	}
	keep := make(map[string]struct{}, len(manifests))
	for _, digest := range manifests {
		keep[digest] = struct{}{}
	}

	drop := old[:0]
	for _, row := range old {
		if _, ok := keep[row.Digest]; !ok {
			drop = append(drop, row)
		}
	}
	if imagePruneHook != nil {
		imagePruneHook(imagePruneDelete)
	}
	// Deleted by name in batches: a list of the kept ones can outgrow what
	// one query may bind.
	for start := 0; start < len(drop); start += imagePruneBatch {
		batch := drop[start:min(start+imagePruneBatch, len(drop))]
		args := make([]any, 0, 2+2*len(batch)) //nolint:mnd // two per manifest, two for the repository
		args = append(args, packagePURL)
		for _, row := range batch {
			args = append(args, row.Digest, row.TaggedAt)
		}
		args = append(args, packagePURL)
		res, err := tx.Exec(db.Rebind(`
			DELETE FROM image_manifests WHERE package_purl = ?
				AND (`+strings.TrimSuffix(strings.Repeat("(digest = ? AND tagged_at = ?) OR ", len(batch)), " OR ")+`)
				AND digest NOT IN (SELECT digest FROM image_tags WHERE package_purl = ?)
		`), args...)
		if err != nil {
			return err
		}
		deleted, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if deleted != int64(len(batch)) {
			// A pull changed one: roll back and leave it to the next prune.
			return nil
		}
	}
	return tx.Commit()
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
	rows, err := db.newestImages(db, packagePURL, n)
	if err != nil {
		return nil, nil, err
	}
	return db.walkImages(db, packagePURL, rows)
}

// walkImages returns every digest the given manifests of a repository
// reference, themselves included, and those among them that are recorded
// manifests.
func (db *DB) walkImages(q imageReader, packagePURL string, roots []imageManifestRow) (map[string]struct{}, []string, error) {
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
		if rows, err = db.imageManifestRows(q, packagePURL, next); err != nil {
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
func (db *DB) newestImages(q imageReader, packagePURL string, n int) ([]imageManifestRow, error) {
	if n <= 0 {
		return nil, nil
	}
	var rows []imageManifestRow
	err := q.Select(&rows, db.Rebind(`
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
func (db *DB) imageManifestRows(q imageReader, packagePURL string, digests []string) ([]imageManifestRow, error) {
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
		if err := q.Select(&found, db.Rebind(query), args...); err != nil {
			return nil, err
		}
		rows = append(rows, found...)
	}
	return rows, nil
}
