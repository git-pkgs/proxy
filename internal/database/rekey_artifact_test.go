package database

import (
	"database/sql"
	"testing"
)

func TestRekeyCachedArtifact(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		for _, scenario := range []string{"move", "destination exists", "source changed", "local source"} {
			t.Run(scenario, func(t *testing.T) {
				oldPackage := &Package{PURL: "pkg:golang/example.com/old", Ecosystem: "golang", Name: "example.com/Old"}
				oldVersion := &Version{PURL: oldPackage.PURL + "@v1.0.0", PackagePURL: oldPackage.PURL}
				old := &Artifact{VersionPURL: oldVersion.PURL, Filename: scenario + ".zip",
					UpstreamURL: "https://proxy.example/example.com/!old/@v/v1.0.0.zip",
					StoragePath: sql.NullString{String: scenario + "/old.zip", Valid: true}}
				if err := db.RecordCachedArtifact(oldPackage, oldVersion, old); err != nil {
					t.Fatal(err)
				}
				old, err := db.GetArtifact(oldVersion.PURL, old.Filename)
				if err != nil || old == nil {
					t.Fatalf("source artifact = %+v, %v", old, err)
				}
				pkg := &Package{PURL: "pkg:golang/example.com/Old", Ecosystem: "golang", Name: "example.com/Old"}
				version := &Version{PURL: pkg.PURL + "@v1.0.0", PackagePURL: pkg.PURL}
				prepareRekeyConflict(t, db, scenario, old, pkg, version)
				moved, err := db.RekeyCachedArtifact(old, pkg, version)
				if err != nil || moved != (scenario == "move") {
					t.Fatalf("RekeyCachedArtifact = %v, %v", moved, err)
				}
				checkRekeyResult(t, db, scenario, old, version)
			})
		}
	})
}

func checkRekeyResult(t *testing.T, db *DB, scenario string, old *Artifact, version *Version) {
	t.Helper()
	current, err := db.GetArtifact(version.PURL, old.Filename)
	if err != nil {
		t.Fatal(err)
	}
	if scenario == "move" && (current == nil || current.ID != old.ID || current.StoragePath != old.StoragePath) {
		t.Errorf("moved artifact = %+v, old = %+v", current, old)
	}
	if scenario == "destination exists" && (current == nil || current.StoragePath.String != "destination.zip") {
		t.Errorf("destination replaced: %+v", current)
	}
	if paths := allPendingDeletes(t, db); len(paths) != 0 {
		t.Errorf("rekey queued stored bytes for deletion: %v", paths)
	}
}

func prepareRekeyConflict(t *testing.T, db *DB, scenario string, old *Artifact, pkg *Package, version *Version) {
	t.Helper()
	var err error
	switch scenario {
	case "destination exists":
		err = db.RecordCachedArtifact(pkg, version, &Artifact{VersionPURL: version.PURL, Filename: old.Filename,
			StoragePath: sql.NullString{String: "destination.zip", Valid: true}})
	case "source changed":
		_, err = db.Exec(db.Rebind(`UPDATE artifacts SET storage_path = ? WHERE id = ?`), "replacement.zip", old.ID)
	case "local source":
		_, err = db.Exec(db.Rebind(`UPDATE artifacts SET source = ? WHERE id = ?`), SourceLocal, old.ID)
	}
	if err != nil {
		t.Fatal(err)
	}
}
