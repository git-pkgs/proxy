package database

import (
	"slices"
	"testing"
	"time"
)

const (
	raceIndex    = "sha256:index"
	racePlatform = "sha256:platform"
	raceGone     = "sha256:gone"
)

func wantRecorded(t *testing.T, db *DB, when string, want bool, digests ...string) {
	t.Helper()
	for _, digest := range digests {
		if _, ok, _, err := db.ImageManifestState(testTagRepo, digest); err != nil || ok != want {
			t.Errorf("%s recorded %s = %v, %v; want %v", digest, when, ok, err, want)
		}
	}
}

// pruneDuringPull prunes a repository of three old, untagged manifests while
// pull runs at the given point of the prune.
func pruneDuringPull(t *testing.T, db *DB, now time.Time, point string, pull func(db *DB, now time.Time) error) {
	t.Helper()
	// Recorded first, so their batches are deleted before the one that finds
	// the change.
	recordReferences(t, db, testTagRepo, raceGone, "sha256:gone-layer")
	recordReferences(t, db, testTagRepo, racePlatform, "sha256:layer")
	recordReferences(t, db, testTagRepo, raceIndex, racePlatform)

	pulled := make(chan error, 1)
	imagePruneHook = func(at string) {
		if at != point {
			return
		}
		go func() { pulled <- pull(db, now) }()
		if db.dialect == DialectPostgres {
			pulled <- <-pulled
			return
		}
		// SQLite has one writer, so there the pull waits for the prune to
		// finish.
		select {
		case err := <-pulled:
			pulled <- err
		case <-time.After(200 * time.Millisecond):
		}
	}
	if err := db.PruneImageManifests(testTagRepo, 0, now.Add(time.Minute)); err != nil {
		t.Fatalf("PruneImageManifests: %v", err)
	}
	if err := <-pulled; err != nil {
		t.Fatalf("concurrent pull: %v", err)
	}
}

// wantKeptAfterRacedPrune checks what a prune that a pull ran into left.
// everywhere says the pull records the manifest itself, which keeps it on
// SQLite, where the pull runs after the prune.
func wantKeptAfterRacedPrune(t *testing.T, db *DB, point string, everywhere bool) {
	t.Helper()
	switch {
	case db.dialect != DialectPostgres:
		if everywhere {
			wantRecorded(t, db, "after the prune", true, raceIndex)
		}
	case point == imagePruneDelete:
		// The prune saw the change and deleted nothing, so what the tagged
		// manifest references is kept too.
		wantRecorded(t, db, "after the prune", true, raceIndex, racePlatform, raceGone)
	default:
		// Read as kept, or found changed.
		wantRecorded(t, db, "after the prune", true, raceIndex, racePlatform)
	}
}

// A pull that tags an old manifest while a prune is between reading what to
// keep and deleting the rest must not lose the manifest: the tag would point
// at nothing recorded and its layers would lose their protection.
func TestPruneImageManifestsKeepsAManifestTaggedMeanwhile(t *testing.T) {
	recordTagged := func(db *DB, now time.Time) error {
		return db.RecordImageManifest(ImageManifest{PackagePURL: testTagRepo, Digest: raceIndex, Refs: []string{racePlatform}, TaggedAt: now})
	}
	tagIt := func(db *DB, now time.Time) error {
		return db.RecordImageTag(testTagRepo, "latest", raceIndex, now)
	}
	cases := []struct {
		name string
		// pull is what the concurrent request writes.
		pull func(db *DB, now time.Time) error
		// everywhere says the manifest is kept on SQLite too, where the pull
		// can only run after the prune and has to record the manifest itself.
		everywhere bool
		// tagged says the pull leaves a tag pointing at a recorded manifest.
		tagged bool
	}{
		{"tag and manifest", func(db *DB, now time.Time) error {
			if err := tagIt(db, now); err != nil {
				return err
			}
			return recordTagged(db, now)
		}, true, true},
		{"manifest before its tag", recordTagged, true, false},
		{"tag before its manifest", tagIt, false, false},
	}
	for _, tc := range cases {
		for _, point := range []string{imagePruneRead, imagePruneDelete} {
			t.Run(tc.name+" at "+point, func(t *testing.T) {
				defer func(n int) { imagePruneBatch = n }(imagePruneBatch)
				imagePruneBatch = 1
				defer func() { imagePruneHook = nil }()

				runWithBothDatabases(t, func(t *testing.T, db *DB) {
					now := time.Now()
					pruneDuringPull(t, db, now, point, tc.pull)

					wantKeptAfterRacedPrune(t, db, point, tc.everywhere)

					// The next prune works from what is there now.
					if err := db.PruneImageManifests(testTagRepo, 0, now.Add(time.Minute)); err != nil {
						t.Fatalf("second PruneImageManifests: %v", err)
					}
					wantRecorded(t, db, "after the second prune", false, raceGone)
					if !tc.tagged {
						return
					}
					wantRecorded(t, db, "after the second prune", true, raceIndex)
					if got, _ := db.ImageTagsContaining(testTagRepo, raceIndex, 10); !slices.Equal(got, []string{"latest"}) {
						t.Errorf("ImageTagsContaining = %v, want latest", got)
					}
				})
			})
		}
	}
}
