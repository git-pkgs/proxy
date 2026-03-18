package database

import (
	"database/sql"
	"testing"
	"time"
)

const testEcosystemNPM = "npm"

func setupListCachedPackagesDB(t *testing.T) *DB {
	t.Helper()

	db, err := Create(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}

	seedListCachedPackagesData(t, db)

	return db
}

func seedListCachedPackagesData(t *testing.T, db *DB) {
	t.Helper()

	packages := []*Package{
		{
			PURL:          "pkg:npm/lodash",
			Ecosystem:     testEcosystemNPM,
			Name:          "lodash",
			LatestVersion: sql.NullString{String: "4.17.21", Valid: true},
			License:       sql.NullString{String: "MIT", Valid: true},
		},
		{
			PURL:          "pkg:cargo/serde",
			Ecosystem:     "cargo",
			Name:          "serde",
			LatestVersion: sql.NullString{String: "1.0.0", Valid: true},
			License:       sql.NullString{String: "MIT OR Apache-2.0", Valid: true},
		},
		{
			PURL:          "pkg:npm/react",
			Ecosystem:     testEcosystemNPM,
			Name:          "react",
			LatestVersion: sql.NullString{String: "18.0.0", Valid: true},
			License:       sql.NullString{String: "MIT", Valid: true},
		},
	}

	for _, pkg := range packages {
		if err := db.UpsertPackage(pkg); err != nil {
			t.Fatal(err)
		}
	}

	versions := []*Version{
		{PURL: "pkg:npm/lodash@4.17.21", PackagePURL: packages[0].PURL},
		{PURL: "pkg:cargo/serde@1.0.0", PackagePURL: packages[1].PURL},
		{PURL: "pkg:npm/react@18.0.0", PackagePURL: packages[2].PURL},
	}

	for _, ver := range versions {
		if err := db.UpsertVersion(ver); err != nil {
			t.Fatal(err)
		}
	}

	artifacts := []*Artifact{
		{
			VersionPURL: versions[0].PURL,
			Filename:    "lodash.tgz",
			UpstreamURL: "https://registry.npmjs.org/lodash/-/lodash-4.17.21.tgz",
			StoragePath: sql.NullString{String: "npm/lodash/4.17.21/lodash.tgz", Valid: true},
			Size:        sql.NullInt64{Int64: 1024, Valid: true},
			HitCount:    100,
			FetchedAt:   sql.NullTime{Time: time.Now(), Valid: true},
		},
		{
			VersionPURL: versions[1].PURL,
			Filename:    "serde.crate",
			UpstreamURL: "https://crates.io/api/v1/crates/serde/1.0.0/download",
			StoragePath: sql.NullString{String: "cargo/serde/1.0.0/serde.crate", Valid: true},
			Size:        sql.NullInt64{Int64: 2048, Valid: true},
			HitCount:    50,
			FetchedAt:   sql.NullTime{Time: time.Now().Add(-1 * time.Hour), Valid: true},
		},
		{
			VersionPURL: versions[2].PURL,
			Filename:    "react.tgz",
			UpstreamURL: "https://registry.npmjs.org/react/-/react-18.0.0.tgz",
			StoragePath: sql.NullString{String: "npm/react/18.0.0/react.tgz", Valid: true},
			Size:        sql.NullInt64{Int64: 512, Valid: true},
			HitCount:    200,
			FetchedAt:   sql.NullTime{Time: time.Now().Add(-2 * time.Hour), Valid: true},
		},
	}

	for _, art := range artifacts {
		if err := db.UpsertArtifact(art); err != nil {
			t.Fatal(err)
		}
	}
}

func TestListCachedPackages(t *testing.T) {
	db := setupListCachedPackagesDB(t)
	defer func() { _ = db.Close() }()

	listAll := func(ecosystem, sortBy string) []PackageListItem {
		t.Helper()
		packages, err := db.ListCachedPackages(ecosystem, sortBy, 10, 0)
		if err != nil {
			t.Fatal(err)
		}
		return packages
	}

	t.Run("list all packages", func(t *testing.T) {
		packages := listAll("", "hits")
		if len(packages) != 3 {
			t.Errorf("expected 3 packages, got %d", len(packages))
		}
		if packages[0].Name != "react" {
			t.Errorf("expected first package to be react, got %s", packages[0].Name)
		}
		if packages[0].Hits != 200 {
			t.Errorf("expected 200 hits, got %d", packages[0].Hits)
		}
	})

	t.Run("filter by ecosystem", func(t *testing.T) {
		packages := listAll(testEcosystemNPM, "hits")
		if len(packages) != 2 {
			t.Errorf("expected 2 npm packages, got %d", len(packages))
		}
		for _, pkg := range packages {
			if pkg.Ecosystem != testEcosystemNPM {
				t.Errorf("expected npm ecosystem, got %s", pkg.Ecosystem)
			}
		}
	})

	t.Run("sort by name", func(t *testing.T) {
		packages := listAll("", "name")
		if packages[0].Name != "lodash" {
			t.Errorf("expected first package to be lodash, got %s", packages[0].Name)
		}
	})

	t.Run("sort by size", func(t *testing.T) {
		packages := listAll("", "size")
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

		count, err = db.CountCachedPackages(testEcosystemNPM)
		if err != nil {
			t.Fatal(err)
		}
		if count != 2 {
			t.Errorf("expected count 2 for npm, got %d", count)
		}
	})
}
