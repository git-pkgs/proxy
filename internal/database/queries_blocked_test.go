package database

import (
	"database/sql"
	"testing"
	"time"
)

func TestGetRecentlyBlockedPackages(t *testing.T) {
	db, err := Create(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Seed: package + version, no surviving artifact row, one critical finding.
	if err := db.UpsertPackage(&Package{
		PURL: "pkg:npm/form-data", Ecosystem: "npm", Name: "form-data",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertVersion(&Version{
		PURL: "pkg:npm/form-data@2.3.3", PackagePURL: "pkg:npm/form-data",
	}); err != nil {
		t.Fatal(err)
	}
	// Insert finding directly via UpsertArtifactFinding. artifact_id may
	// reference a no-longer-existing row — the blocked-packages query
	// joins via version_purl, not artifact_id.
	scannedAt := time.Now().Add(-1 * time.Minute)
	if err := db.UpsertArtifactFinding(&ArtifactFinding{
		ArtifactID:  999,
		VersionPURL: "pkg:npm/form-data@2.3.3",
		ContentHash: "abc123",
		Scanner:     "osv",
		FindingID:   "GHSA-fjxv-7rqg-78g4",
		Severity:    "critical",
		ScannedAt:   scannedAt,
	}); err != nil {
		t.Fatal(err)
	}

	// Also seed a package WITH a surviving cached artifact; it must NOT
	// appear in the blocked list.
	if err := db.UpsertPackage(&Package{
		PURL: "pkg:npm/lodash", Ecosystem: "npm", Name: "lodash",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertVersion(&Version{
		PURL: "pkg:npm/lodash@4.17.21", PackagePURL: "pkg:npm/lodash",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertArtifact(&Artifact{
		VersionPURL: "pkg:npm/lodash@4.17.21",
		Filename:    "lodash.tgz",
		UpstreamURL: "https://example/lodash.tgz",
		StoragePath: sql.NullString{String: "npm/lodash/4.17.21/lodash.tgz", Valid: true},
		FetchedAt:   sql.NullTime{Time: time.Now(), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	art, err := db.GetArtifact("pkg:npm/lodash@4.17.21", "lodash.tgz")
	if err != nil || art == nil {
		t.Fatalf("get artifact: %v", err)
	}
	if err := db.UpsertArtifactFinding(&ArtifactFinding{
		ArtifactID:  art.ID,
		VersionPURL: "pkg:npm/lodash@4.17.21",
		ContentHash: "xyz",
		Scanner:     "osv",
		FindingID:   "CVE-test",
		Severity:    "high",
		ScannedAt:   time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	blocked, err := db.GetRecentlyBlockedPackages(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocked) != 1 {
		t.Fatalf("expected 1 blocked package, got %d: %+v", len(blocked), blocked)
	}
	got := blocked[0]
	if got.Ecosystem != "npm" || got.Name != "form-data" || got.Version != "2.3.3" {
		t.Errorf("unexpected blocked entry: %+v", got)
	}
	if got.HighestSeverity != "critical" {
		t.Errorf("expected critical severity, got %q", got.HighestSeverity)
	}
	if got.FindingCount != 1 {
		t.Errorf("expected 1 finding, got %d", got.FindingCount)
	}
}

func TestGetRecentlyBlockedPackages_HighestSeverityWins(t *testing.T) {
	db, err := Create(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.UpsertPackage(&Package{
		PURL: "pkg:npm/evil", Ecosystem: "npm", Name: "evil",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertVersion(&Version{
		PURL: "pkg:npm/evil@1.0.0", PackagePURL: "pkg:npm/evil",
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i, sev := range []string{"low", "high", "medium", "critical"} {
		if err := db.UpsertArtifactFinding(&ArtifactFinding{
			ArtifactID:  int64(100 + i),
			VersionPURL: "pkg:npm/evil@1.0.0",
			ContentHash: "h",
			Scanner:     "osv",
			FindingID:   sev + "-id",
			Severity:    sev,
			ScannedAt:   now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	blocked, err := db.GetRecentlyBlockedPackages(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocked) != 1 {
		t.Fatalf("expected 1 row, got %d", len(blocked))
	}
	if blocked[0].HighestSeverity != "critical" {
		t.Errorf("expected critical, got %q", blocked[0].HighestSeverity)
	}
	if blocked[0].FindingCount != 4 {
		t.Errorf("expected 4 findings, got %d", blocked[0].FindingCount)
	}
}
