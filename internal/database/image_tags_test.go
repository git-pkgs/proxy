package database

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

const testTagRepo = "pkg:oci/pihole/pihole"

func recordTag(t *testing.T, db *DB, repo, tag, digest string, seen time.Time) {
	t.Helper()
	if err := db.RecordImageTag(repo, tag, digest, seen); err != nil {
		t.Fatalf("RecordImageTag(%s): %v", tag, err)
	}
}

// The tags of a repository are listed most recently seen first, each with
// the digest it pointed at when last seen.
func TestImageTagsListsTagsMostRecentlySeenFirst(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		now := time.Now().Truncate(time.Millisecond)
		recordTag(t, db, testTagRepo, "2026.10.0", "sha256:old", now.Add(-48*time.Hour))
		recordTag(t, db, testTagRepo, "latest", "sha256:old", now.Add(-time.Hour))
		recordTag(t, db, testTagRepo, "2026.11.0", "sha256:new", now.Add(-time.Minute))
		recordTag(t, db, "pkg:oci/library/alpine", "latest", "sha256:alpine", now)

		// latest moves to the new image.
		recordTag(t, db, testTagRepo, "latest", "sha256:new", now)
		// A sighting older than the stored one changes nothing.
		recordTag(t, db, testTagRepo, "2026.11.0", "sha256:other", now.Add(-time.Hour))

		tags, err := db.ImageTags(testTagRepo, 10)
		if err != nil {
			t.Fatalf("ImageTags: %v", err)
		}
		want := []ImageTag{
			{Tag: "latest", Digest: "sha256:new", SeenAt: now},
			{Tag: "2026.11.0", Digest: "sha256:new", SeenAt: now.Add(-time.Minute)},
			{Tag: "2026.10.0", Digest: "sha256:old", SeenAt: now.Add(-48 * time.Hour)},
		}
		if len(tags) != len(want) {
			t.Fatalf("ImageTags = %+v, want %+v", tags, want)
		}
		for i := range want {
			if tags[i].Tag != want[i].Tag || tags[i].Digest != want[i].Digest || !tags[i].SeenAt.Equal(want[i].SeenAt) {
				t.Errorf("tag %d = %+v, want %+v", i, tags[i], want[i])
			}
		}

		if tags, err := db.ImageTags(testTagRepo, 1); err != nil || len(tags) != 1 || tags[0].Tag != "latest" {
			t.Errorf("ImageTags(limit 1) = %+v, %v; want latest only", tags, err)
		}
	})
}

func TestMigrateAddsImageTags(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		if _, err := db.Exec("DROP TABLE image_tags"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("DELETE FROM migrations WHERE name = '024_add_image_tags'"); err != nil {
			t.Fatal(err)
		}
		if err := db.MigrateSchema(); err != nil {
			t.Fatalf("MigrateSchema: %v", err)
		}
		recordTag(t, db, testTagRepo, "latest", "sha256:a", time.Now())
		if tags, err := db.ImageTags(testTagRepo, 10); err != nil || len(tags) != 1 {
			t.Errorf("ImageTags after migration = %+v, %v; want one tag", tags, err)
		}
	})
}

func recordReferences(t *testing.T, db *DB, repo, manifest string, references ...string) {
	t.Helper()
	if err := db.RecordImageManifestReferences(repo, manifest, references); err != nil {
		t.Fatalf("RecordImageManifestReferences(%s): %v", manifest, err)
	}
}

// The tags whose image contains a digest: the manifest a tag points at, the
// platform manifests of an index, and their configs and layers.
func TestImageTagsContaining(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		now := time.Now()
		recordReferences(t, db, testTagRepo, "sha256:index", "sha256:amd64", "sha256:arm64")
		recordReferences(t, db, testTagRepo, "sha256:amd64", "sha256:amd64-config", "sha256:shared", "sha256:amd64-layer")
		recordReferences(t, db, testTagRepo, "sha256:single", "sha256:single-config", "sha256:shared")
		// The same digests in another repository say nothing about this one.
		recordReferences(t, db, "pkg:oci/library/alpine", "sha256:alpine", "sha256:single-config")
		recordReferences(t, db, "pkg:oci/library/alpine", "sha256:index", "sha256:alpine-layer")
		recordReferences(t, db, "pkg:oci/library/alpine", "sha256:single", "sha256:amd64")
		recordTag(t, db, "pkg:oci/library/alpine", "latest", "sha256:alpine", now)

		recordTag(t, db, testTagRepo, "multi", "sha256:index", now)
		recordTag(t, db, testTagRepo, "single", "sha256:single", now.Add(-time.Minute))
		recordTag(t, db, testTagRepo, "old", "sha256:index", now.Add(-time.Hour))

		for _, tt := range []struct {
			digest string
			want   []string
		}{
			{"sha256:index", []string{"multi", "old"}},
			{"sha256:amd64", []string{"multi", "old"}},
			{"sha256:arm64", []string{"multi", "old"}},
			{"sha256:amd64-layer", []string{"multi", "old"}},
			{"sha256:shared", []string{"multi", "single", "old"}},
			{"sha256:single-config", []string{"single"}},
			{"sha256:alpine-layer", nil},
			{"sha256:unknown", nil},
		} {
			got, err := db.ImageTagsContaining(testTagRepo, tt.digest, 10)
			if err != nil {
				t.Fatalf("ImageTagsContaining(%s): %v", tt.digest, err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("ImageTagsContaining(%s) = %v, want %v", tt.digest, got, tt.want)
			}
		}

		if got, _ := db.ImageTagsContaining(testTagRepo, "sha256:shared", 2); !slices.Equal(got, []string{"multi", "single"}) {
			t.Errorf("ImageTagsContaining(limit 2) = %v, want multi and single", got)
		}
	})
}

// The limit applies to the tags that match, so a match among older tags is
// found however many newer tags there are.
func TestImageTagsContainingLimitsTheMatches(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		now := time.Now()
		recordReferences(t, db, testTagRepo, "sha256:old", "sha256:layer")
		recordTag(t, db, testTagRepo, "oldest", "sha256:old", now.Add(-time.Hour))
		for i := range 5 {
			recordTag(t, db, testTagRepo, fmt.Sprintf("v%d", i), "sha256:new", now)
		}
		if got, _ := db.ImageTagsContaining(testTagRepo, "sha256:layer", 2); !slices.Equal(got, []string{"oldest"}) {
			t.Errorf("ImageTagsContaining = %v, want oldest", got)
		}
	})
}

// A manifest's digest fixes its content, so the references recorded first
// stand, and recording them again adds nothing.
func TestRecordImageManifestReferencesKeepsTheFirstRecord(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		recordReferences(t, db, testTagRepo, "sha256:m", "sha256:a", "sha256:b", "sha256:a")
		recordReferences(t, db, testTagRepo, "sha256:m", "sha256:c")
		recordTag(t, db, testTagRepo, "latest", "sha256:m", time.Now())
		if got, _ := db.ImageTagsContaining(testTagRepo, "sha256:c", 10); len(got) != 0 {
			t.Errorf("a second record added a reference: %v", got)
		}
		var rows int
		if err := db.Get(&rows, "SELECT COUNT(*) FROM image_manifest_references"); err != nil {
			t.Fatal(err)
		}
		if rows != 2 {
			t.Errorf("%d rows, want 2", rows)
		}
	})
}

func TestMigrateAddsImageManifestReferences(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		if _, err := db.Exec("DROP TABLE image_manifest_references"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("DELETE FROM migrations WHERE name = '025_add_image_manifest_references'"); err != nil {
			t.Fatal(err)
		}
		if err := db.MigrateSchema(); err != nil {
			t.Fatalf("MigrateSchema: %v", err)
		}
		recordReferences(t, db, testTagRepo, "sha256:m", "sha256:a")
		recordTag(t, db, testTagRepo, "latest", "sha256:m", time.Now())
		if got, err := db.ImageTagsContaining(testTagRepo, "sha256:a", 10); err != nil || len(got) != 1 {
			t.Errorf("ImageTagsContaining after migration = %v, %v; want latest", got, err)
		}
	})
}
