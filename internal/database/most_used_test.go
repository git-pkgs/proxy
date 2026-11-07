package database

import (
	"reflect"
	"testing"
	"time"
)

// seedUsedArtifact stores a cached artifact with hits, last downloaded at
// accessed.
func seedUsedArtifact(t *testing.T, db *DB, ecosystem, name, version, filename string, hits int64, accessed time.Time) {
	t.Helper()
	packagePURL := "pkg:" + ecosystem + "/" + name
	versionPURL := packagePURL + "@" + version
	if err := db.UpsertPackage(&Package{PURL: packagePURL, Ecosystem: ecosystem, Name: name}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertVersion(&Version{PURL: versionPURL, PackagePURL: packagePURL}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertArtifact(&Artifact{VersionPURL: versionPURL, Filename: filename, UpstreamURL: "https://example.test/" + filename}); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkArtifactCached(versionPURL, filename, ecosystem+"/"+filename, "sha256:00", 1, "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(db.Rebind("UPDATE artifacts SET hit_count = ?, last_accessed_at = ? WHERE version_purl = ? AND filename = ?"),
		hits, accessed.UTC(), versionPURL, filename); err != nil {
		t.Fatal(err)
	}
}

func TestMostUsedPackages(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		now := time.Now()
		seedUsedArtifact(t, db, "npm", "lodash", "4.17.20", "lodash-4.17.20.tgz", 5, now.Add(-time.Hour))
		seedUsedArtifact(t, db, "npm", "lodash", "4.17.21", "lodash-4.17.21.tgz", 4, now.Add(-20*24*time.Hour))
		seedUsedArtifact(t, db, "npm", "react", "18.3.1", "react-18.3.1.tgz", 10, now.Add(-time.Hour))
		seedUsedArtifact(t, db, "npm", "left-pad", "1.3.0", "left-pad-1.3.0.tgz", 100, now.Add(-30*24*time.Hour))
		seedUsedArtifact(t, db, "cargo", "serde", "1.0.200", "serde-1.0.200.crate", 1, now.Add(-time.Hour))

		// Not cached: never counts.
		if err := db.UpsertArtifact(&Artifact{VersionPURL: "pkg:npm/react@19.0.0", Filename: "react-19.0.0.tgz", UpstreamURL: "https://example.test/x"}); err != nil {
			t.Fatal(err)
		}

		got, err := db.MostUsedPackages(now.Add(-14*24*time.Hour), 2)
		if err != nil {
			t.Fatal(err)
		}
		want := []UsedPackage{
			{Ecosystem: "cargo", Name: "serde", PURL: "pkg:cargo/serde", Hits: 1, Files: []UsedFile{
				{Version: "1.0.200", Filename: "serde-1.0.200.crate", UpstreamURL: "https://example.test/serde-1.0.200.crate"},
			}},
			// react has more hits than lodash. Every cached file of lodash is
			// listed, but only recent downloads count.
			{Ecosystem: "npm", Name: "react", PURL: "pkg:npm/react", Hits: 10, Files: []UsedFile{
				{Version: "18.3.1", Filename: "react-18.3.1.tgz", UpstreamURL: "https://example.test/react-18.3.1.tgz"},
			}},
			{Ecosystem: "npm", Name: "lodash", PURL: "pkg:npm/lodash", Hits: 5, Files: []UsedFile{
				{Version: "4.17.20", Filename: "lodash-4.17.20.tgz", UpstreamURL: "https://example.test/lodash-4.17.20.tgz"},
				{Version: "4.17.21", Filename: "lodash-4.17.21.tgz", UpstreamURL: "https://example.test/lodash-4.17.21.tgz"},
			}},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("MostUsedPackages =\n%+v\nwant\n%+v", got, want)
		}

		top1, err := db.MostUsedPackages(now.Add(-14*24*time.Hour), 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(top1) != 2 || top1[0].Name != "serde" || top1[1].Name != "react" {
			t.Errorf("top 1 per ecosystem = %+v, want serde and react", top1)
		}
	})
}

func TestMostUsedPackagesSkipsLocalArtifacts(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		now := time.Now()
		seedUsedArtifact(t, db, "npm", "internal-lib", "1.0.0", "internal-lib-1.0.0.tgz", 50, now)
		if _, err := db.Exec(db.Rebind("UPDATE artifacts SET source = ? WHERE filename = ?"), SourceLocal, "internal-lib-1.0.0.tgz"); err != nil {
			t.Fatal(err)
		}
		got, err := db.MostUsedPackages(now.Add(-time.Hour), 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("MostUsedPackages = %+v, want none: local packages have no upstream to refresh from", got)
		}
	})
}
