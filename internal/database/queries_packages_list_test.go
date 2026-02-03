package database

import (
	"database/sql"
	"testing"
	"time"
)

func TestListCachedPackages(t *testing.T) {
	db, err := Create(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	// Create test packages
	pkg1 := &Package{
		PURL:          "pkg:npm/lodash",
		Ecosystem:     "npm",
		Name:          "lodash",
		LatestVersion: sql.NullString{String: "4.17.21", Valid: true},
		License:       sql.NullString{String: "MIT", Valid: true},
	}
	pkg2 := &Package{
		PURL:          "pkg:cargo/serde",
		Ecosystem:     "cargo",
		Name:          "serde",
		LatestVersion: sql.NullString{String: "1.0.0", Valid: true},
		License:       sql.NullString{String: "MIT OR Apache-2.0", Valid: true},
	}
	pkg3 := &Package{
		PURL:          "pkg:npm/react",
		Ecosystem:     "npm",
		Name:          "react",
		LatestVersion: sql.NullString{String: "18.0.0", Valid: true},
		License:       sql.NullString{String: "MIT", Valid: true},
	}

	if err := db.UpsertPackage(pkg1); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertPackage(pkg2); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertPackage(pkg3); err != nil {
		t.Fatal(err)
	}

	// Create versions
	ver1 := &Version{
		PURL:        "pkg:npm/lodash@4.17.21",
		PackagePURL: pkg1.PURL,
	}
	ver2 := &Version{
		PURL:        "pkg:cargo/serde@1.0.0",
		PackagePURL: pkg2.PURL,
	}
	ver3 := &Version{
		PURL:        "pkg:npm/react@18.0.0",
		PackagePURL: pkg3.PURL,
	}

	if err := db.UpsertVersion(ver1); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertVersion(ver2); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertVersion(ver3); err != nil {
		t.Fatal(err)
	}

	// Create artifacts
	art1 := &Artifact{
		VersionPURL: ver1.PURL,
		Filename:    "lodash.tgz",
		UpstreamURL: "https://registry.npmjs.org/lodash/-/lodash-4.17.21.tgz",
		StoragePath: sql.NullString{String: "npm/lodash/4.17.21/lodash.tgz", Valid: true},
		Size:        sql.NullInt64{Int64: 1024, Valid: true},
		HitCount:    100,
		FetchedAt:   sql.NullTime{Time: time.Now(), Valid: true},
	}
	art2 := &Artifact{
		VersionPURL: ver2.PURL,
		Filename:    "serde.crate",
		UpstreamURL: "https://crates.io/api/v1/crates/serde/1.0.0/download",
		StoragePath: sql.NullString{String: "cargo/serde/1.0.0/serde.crate", Valid: true},
		Size:        sql.NullInt64{Int64: 2048, Valid: true},
		HitCount:    50,
		FetchedAt:   sql.NullTime{Time: time.Now().Add(-1 * time.Hour), Valid: true},
	}
	art3 := &Artifact{
		VersionPURL: ver3.PURL,
		Filename:    "react.tgz",
		UpstreamURL: "https://registry.npmjs.org/react/-/react-18.0.0.tgz",
		StoragePath: sql.NullString{String: "npm/react/18.0.0/react.tgz", Valid: true},
		Size:        sql.NullInt64{Int64: 512, Valid: true},
		HitCount:    200,
		FetchedAt:   sql.NullTime{Time: time.Now().Add(-2 * time.Hour), Valid: true},
	}

	if err := db.UpsertArtifact(art1); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertArtifact(art2); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertArtifact(art3); err != nil {
		t.Fatal(err)
	}

	t.Run("list all packages", func(t *testing.T) {
		packages, err := db.ListCachedPackages("", "hits", 10, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(packages) != 3 {
			t.Errorf("expected 3 packages, got %d", len(packages))
		}
		// Should be sorted by hits DESC
		if packages[0].Name != "react" {
			t.Errorf("expected first package to be react, got %s", packages[0].Name)
		}
		if packages[0].Hits != 200 {
			t.Errorf("expected 200 hits, got %d", packages[0].Hits)
		}
	})

	t.Run("filter by ecosystem", func(t *testing.T) {
		packages, err := db.ListCachedPackages("npm", "hits", 10, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(packages) != 2 {
			t.Errorf("expected 2 npm packages, got %d", len(packages))
		}
		for _, pkg := range packages {
			if pkg.Ecosystem != "npm" {
				t.Errorf("expected npm ecosystem, got %s", pkg.Ecosystem)
			}
		}
	})

	t.Run("sort by name", func(t *testing.T) {
		packages, err := db.ListCachedPackages("", "name", 10, 0)
		if err != nil {
			t.Fatal(err)
		}
		if packages[0].Name != "lodash" {
			t.Errorf("expected first package to be lodash, got %s", packages[0].Name)
		}
	})

	t.Run("sort by size", func(t *testing.T) {
		packages, err := db.ListCachedPackages("", "size", 10, 0)
		if err != nil {
			t.Fatal(err)
		}
		if packages[0].Name != "serde" {
			t.Errorf("expected first package to be serde (largest), got %s", packages[0].Name)
		}
	})

	t.Run("count packages", func(t *testing.T) {
		count, err := db.CountCachedPackages("")
		if err != nil {
			t.Fatal(err)
		}
		if count != 3 {
			t.Errorf("expected count 3, got %d", count)
		}

		count, err = db.CountCachedPackages("npm")
		if err != nil {
			t.Fatal(err)
		}
		if count != 2 {
			t.Errorf("expected count 2 for npm, got %d", count)
		}
	})
}
