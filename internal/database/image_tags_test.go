package database

import (
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
