package server

import (
	"context"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/retention"
)

// Container records are pruned without any retention rule: they are written
// on every install, for the web UI.
func TestImageRecordPruningRunsWithoutRetention(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()
	if rules, err := ts.server.cfg.RetentionRules(retention.Default); err == nil && rules.MinPositive() != 0 {
		t.Fatalf("test config has retention rules: %+v", rules)
	}
	const repo = "pkg:oci/team/app"
	db := ts.db
	now := time.Now()
	if err := db.RecordImageManifest(database.ImageManifest{PackagePURL: repo, Digest: "sha256:tagged", Refs: []string{"sha256:layer"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordImageManifest(database.ImageManifest{PackagePURL: repo, Digest: "sha256:untagged", Refs: []string{"sha256:layer"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(db.Rebind(`UPDATE image_manifests SET recorded_at = ?`), now.Add(-2*imageRecordGrace).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordImageTag(repo, "latest", "sha256:tagged", now); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordImageTag(repo, "stale", "sha256:untagged", now.Add(-imageTagMaxAge-time.Hour)); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ts.server.startImageRecordPruning(ctx)
	defer func() {
		cancel()
		ts.server.background.Wait()
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, recorded, _, err := db.ImageManifestState(repo, "sha256:untagged")
		if err != nil {
			t.Fatal(err)
		}
		if !recorded {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the manifest of a tag not confirmed for too long was not pruned")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, recorded, _, err := db.ImageManifestState(repo, "sha256:tagged"); err != nil || !recorded {
		t.Errorf("the manifest latest points at was pruned: %v, %v", recorded, err)
	}
	if tags, err := db.ImageTags(repo, 10); err != nil || len(tags) != 1 || tags[0].Tag != "latest" {
		t.Errorf("tags = %+v, %v; want latest only", tags, err)
	}
}
