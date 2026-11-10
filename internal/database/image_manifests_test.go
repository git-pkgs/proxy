package database

import (
	"slices"
	"testing"
	"time"
)

const testImageRepo = "pkg:oci/library%2Fnginx"

func recordImage(t *testing.T, db *DB, repo, digest string, taggedAt time.Time, refs ...string) {
	t.Helper()
	err := db.RecordImageManifest(ImageManifest{PackagePURL: repo, Digest: digest, Refs: refs, TaggedAt: taggedAt})
	if err != nil {
		t.Fatalf("RecordImageManifest(%s): %v", digest, err)
	}
}

func newestImageDigests(t *testing.T, db *DB, repo string, n int) []string {
	t.Helper()
	set, err := db.NewestImageDigests(repo, n)
	if err != nil {
		t.Fatalf("NewestImageDigests: %v", err)
	}
	digests := make([]string, 0, len(set))
	for d := range set {
		digests = append(digests, d)
	}
	slices.Sort(digests)
	return digests
}

// The newest images are the manifests a tag was last found to point at. What
// they reference is followed through the manifests recorded for the same
// repository, so an index leads to its platform manifests and their blobs.
func TestNewestImageDigestsFollowsReferences(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		now := time.Now()
		recordImage(t, db, testImageRepo, "sha256:index-old", now.Add(-2*time.Hour), "sha256:arm-old", "sha256:amd-old")
		recordImage(t, db, testImageRepo, "sha256:arm-old", time.Time{}, "sha256:config-old", "sha256:layer-shared", "sha256:layer-old")
		recordImage(t, db, testImageRepo, "sha256:index-new", now.Add(-time.Hour), "sha256:arm-new", "sha256:amd-new")
		recordImage(t, db, testImageRepo, "sha256:arm-new", time.Time{}, "sha256:config-new", "sha256:layer-shared", "sha256:layer-new")
		// Another repository's image is newer still and must not count here.
		recordImage(t, db, "pkg:oci/library%2Fredis", "sha256:redis", now, "sha256:layer-redis")

		want := []string{"sha256:amd-new", "sha256:arm-new", "sha256:config-new", "sha256:index-new", "sha256:layer-new", "sha256:layer-shared"}
		if got := newestImageDigests(t, db, testImageRepo, 1); !slices.Equal(got, want) {
			t.Errorf("newest 1 = %v, want %v", got, want)
		}
		if got := newestImageDigests(t, db, testImageRepo, 2); len(got) != 11 {
			t.Errorf("newest 2 = %v, want all 11 digests of both images", got)
		}
		if got := newestImageDigests(t, db, testImageRepo, 0); len(got) != 0 {
			t.Errorf("newest 0 = %v, want none", got)
		}
	})
}

// A manifest only asked for by digest is not an image of its own.
func TestNewestImageDigestsIgnoresUntaggedManifests(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		recordImage(t, db, testImageRepo, "sha256:pinned", time.Time{}, "sha256:layer-pinned")
		if got := newestImageDigests(t, db, testImageRepo, 5); len(got) != 0 {
			t.Errorf("newest = %v, want none", got)
		}
	})
}

// Recording a manifest again moves its tag time forward only, and a request
// by digest does not make it lose the time a tag gave it.
func TestRecordImageManifestKeepsLatestTagTime(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		now := time.Now().Truncate(time.Millisecond)
		recordImage(t, db, testImageRepo, "sha256:a", now.Add(-time.Hour), "sha256:layer-a")
		recordImage(t, db, testImageRepo, "sha256:b", now.Add(-30*time.Minute), "sha256:layer-b")

		if at, ok, err := db.ImageManifestTaggedAt(testImageRepo, "sha256:missing"); err != nil || ok || !at.IsZero() {
			t.Fatalf("ImageManifestTaggedAt(missing) = %v, %v, %v; want zero, false, nil", at, ok, err)
		}

		recordImage(t, db, testImageRepo, "sha256:a", time.Time{}, "sha256:layer-a")
		recordImage(t, db, testImageRepo, "sha256:a", now.Add(-2*time.Hour), "sha256:layer-a")
		at, ok, err := db.ImageManifestTaggedAt(testImageRepo, "sha256:a")
		if err != nil || !ok || !at.Equal(now.Add(-time.Hour)) {
			t.Fatalf("ImageManifestTaggedAt(a) = %v, %v, %v; want %v, true, nil", at, ok, err, now.Add(-time.Hour))
		}
		if got, want := newestImageDigests(t, db, testImageRepo, 1), []string{"sha256:b", "sha256:layer-b"}; !slices.Equal(got, want) {
			t.Errorf("newest 1 = %v, want %v", got, want)
		}

		// A tag found on a again makes it the newest image.
		recordImage(t, db, testImageRepo, "sha256:a", now, "sha256:layer-a")
		if got, want := newestImageDigests(t, db, testImageRepo, 1), []string{"sha256:a", "sha256:layer-a"}; !slices.Equal(got, want) {
			t.Errorf("newest 1 after retag = %v, want %v", got, want)
		}
	})
}

// A manifest that references itself, or a chain longer than any real index,
// ends the walk instead of looping.
func TestNewestImageDigestsStopsOnCycles(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		now := time.Now()
		recordImage(t, db, testImageRepo, "sha256:a", now, "sha256:b")
		recordImage(t, db, testImageRepo, "sha256:b", time.Time{}, "sha256:a", "sha256:layer")
		if got, want := newestImageDigests(t, db, testImageRepo, 1), []string{"sha256:a", "sha256:b", "sha256:layer"}; !slices.Equal(got, want) {
			t.Errorf("newest 1 = %v, want %v", got, want)
		}
	})
}

// Pruning drops what the newest images no longer reference, once it is older
// than the grace time, so a platform manifest recorded just after its index
// is not lost.
func TestPruneImageManifests(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		now := time.Now()
		recordImage(t, db, testImageRepo, "sha256:index-old", now.Add(-2*time.Hour), "sha256:arm-old")
		recordImage(t, db, testImageRepo, "sha256:arm-old", time.Time{}, "sha256:layer-old")
		recordImage(t, db, testImageRepo, "sha256:index-new", now.Add(-time.Hour), "sha256:arm-new")
		recordImage(t, db, testImageRepo, "sha256:arm-new", time.Time{}, "sha256:layer-new")
		recordImage(t, db, testImageRepo, "sha256:pinned", time.Time{}, "sha256:layer-pinned")
		recordImage(t, db, "pkg:oci/library%2Fredis", "sha256:redis-old", now.Add(-3*time.Hour), "sha256:layer-redis")

		// Everything was recorded just now, so nothing is old enough yet.
		if err := db.PruneImageManifests(testImageRepo, 1, now.Add(-time.Minute)); err != nil {
			t.Fatalf("PruneImageManifests: %v", err)
		}
		if got := newestImageDigests(t, db, testImageRepo, 2); len(got) != 6 {
			t.Fatalf("after early prune, newest 2 = %v, want both images", got)
		}

		if err := db.PruneImageManifests(testImageRepo, 1, now.Add(time.Minute)); err != nil {
			t.Fatalf("PruneImageManifests: %v", err)
		}
		want := []string{"sha256:arm-new", "sha256:index-new", "sha256:layer-new"}
		if got := newestImageDigests(t, db, testImageRepo, 2); !slices.Equal(got, want) {
			t.Errorf("after prune, newest 2 = %v, want %v", got, want)
		}
		for _, digest := range []string{"sha256:index-old", "sha256:arm-old", "sha256:pinned"} {
			if _, ok, err := db.ImageManifestTaggedAt(testImageRepo, digest); err != nil || ok {
				t.Errorf("%s still recorded after prune (ok=%v, err=%v)", digest, ok, err)
			}
		}
		if got := newestImageDigests(t, db, "pkg:oci/library%2Fredis", 1); len(got) != 2 {
			t.Errorf("other repository after prune = %v, want its image kept", got)
		}
	})
}

// A database from before the table existed gets it from the migration.
func TestMigrateAddsImageManifests(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		if _, err := db.Exec("DROP TABLE image_manifests"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("DELETE FROM migrations WHERE name = '024_add_image_manifests'"); err != nil {
			t.Fatal(err)
		}
		if err := db.MigrateSchema(); err != nil {
			t.Fatalf("MigrateSchema: %v", err)
		}
		recordImage(t, db, testImageRepo, "sha256:a", time.Now(), "sha256:layer-a")
		if got := newestImageDigests(t, db, testImageRepo, 1); len(got) != 2 {
			t.Errorf("newest 1 after migration = %v, want the image", got)
		}
	})
}

// The repositories with records are listed once each, for pruning.
func TestImageManifestRepositories(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		if got, err := db.ImageManifestRepositories(); err != nil || len(got) != 0 {
			t.Fatalf("ImageManifestRepositories() = %v, %v; want none", got, err)
		}
		const redis = "pkg:oci/library%2Fredis"
		recordImage(t, db, testImageRepo, "sha256:a", time.Now(), "sha256:layer-a")
		recordImage(t, db, testImageRepo, "sha256:b", time.Time{}, "sha256:layer-b")
		recordImage(t, db, redis, "sha256:c", time.Time{}, "sha256:layer-c")

		got, err := db.ImageManifestRepositories()
		if err != nil {
			t.Fatalf("ImageManifestRepositories: %v", err)
		}
		slices.Sort(got)
		if want := []string{testImageRepo, redis}; !slices.Equal(got, want) {
			t.Errorf("ImageManifestRepositories() = %v, want %v", got, want)
		}
	})
}
