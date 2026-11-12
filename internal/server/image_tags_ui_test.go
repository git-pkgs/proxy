package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
)

// The page of a container image lists its tags, most recently seen first,
// with the digest each one points at.
func TestPackageShowPageListsImageTags(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	pkg := &database.Package{PURL: "pkg:oci/pihole/pihole", Ecosystem: "oci", Name: "pihole/pihole"}
	if err := ts.db.UpsertPackage(pkg); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}
	const layer = "sha256:7373c5bccb12b43ec422ccd65b3cc9b55ee6e86ad3cee28189322c6419a72b02"
	if err := ts.db.UpsertVersion(&database.Version{PURL: pkg.PURL + "@" + layer, PackagePURL: pkg.PURL}); err != nil {
		t.Fatalf("UpsertVersion: %v", err)
	}
	const newDigest = "sha256:66c798d79a95c2691f280329c3611dba1746117c61c9f331907ca35a06a52778"
	const oldDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	now := time.Now()
	for tag, at := range map[string]struct {
		digest string
		seen   time.Time
	}{
		"2026.10.0": {oldDigest, now.Add(-48 * time.Hour)},
		"2026.11.0": {newDigest, now.Add(-time.Minute)},
		"latest":    {newDigest, now},
	} {
		if err := ts.db.RecordImageTag(pkg.PURL, tag, at.digest, at.seen); err != nil {
			t.Fatalf("RecordImageTag: %v", err)
		}
	}

	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ui/package/oci/pihole/pihole", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Tags (3)") {
		t.Error("page has no Tags (3) section")
	}
	latest, current, older := strings.Index(body, ">latest<"), strings.Index(body, ">2026.11.0<"), strings.Index(body, ">2026.10.0<")
	if latest < 0 || current < 0 || older < 0 || latest > current || current > older {
		t.Errorf("tags at %d, %d, %d; want latest, 2026.11.0, 2026.10.0 in that order", latest, current, older)
	}
	if !strings.Contains(body, newDigest) || !strings.Contains(body, oldDigest) {
		t.Error("page does not show the digests the tags point at")
	}
}

// Packages of other ecosystems have no tags section.
func TestPackageShowPageHasNoTagsOutsideOCI(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()
	pkg := &database.Package{PURL: "pkg:npm/no-tags", Ecosystem: "npm", Name: "no-tags"}
	if err := ts.db.UpsertPackage(pkg); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ui/package/npm/no-tags", nil))
	if strings.Contains(w.Body.String(), "Tags (") {
		t.Error("npm package page has a Tags section")
	}
}

// The page of an image's blob names the tags whose image contains it.
func TestVersionShowPageListsImageTagsContainingTheBlob(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	pkg := &database.Package{PURL: "pkg:oci/pihole/pihole", Ecosystem: "oci", Name: "pihole/pihole"}
	if err := ts.db.UpsertPackage(pkg); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}
	const layer = "sha256:7373c5bccb12b43ec422ccd65b3cc9b55ee6e86ad3cee28189322c6419a72b02"
	if err := ts.db.UpsertVersion(&database.Version{PURL: pkg.PURL + "@" + layer, PackagePURL: pkg.PURL}); err != nil {
		t.Fatalf("UpsertVersion: %v", err)
	}
	const image = "sha256:66c798d79a95c2691f280329c3611dba1746117c61c9f331907ca35a06a52778"
	if err := ts.db.RecordImageManifestReferences(pkg.PURL, image, []string{layer}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for tag, digest := range map[string]string{"latest": image, "2026.11.0": image, "2026.10.0": "sha256:other"} {
		if err := ts.db.RecordImageTag(pkg.PURL, tag, digest, now); err != nil {
			t.Fatal(err)
		}
	}

	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ui/package/oci/pihole/pihole/"+layer, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Tags:") || !strings.Contains(body, ">latest<") || !strings.Contains(body, ">2026.11.0<") {
		t.Error("version page does not name the tags containing the blob")
	}
	if strings.Contains(body, ">2026.10.0<") {
		t.Error("version page names a tag whose image lacks the blob")
	}
}

// The tags searched for a blob are not cut to the ones the image's page
// lists, so a blob of an old tag still names it.
func TestVersionShowPageSearchesPastTheListedTags(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	pkg := &database.Package{PURL: "pkg:oci/team/app", Ecosystem: "oci", Name: "team/app"}
	if err := ts.db.UpsertPackage(pkg); err != nil {
		t.Fatal(err)
	}
	const layer = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	if err := ts.db.UpsertVersion(&database.Version{PURL: pkg.PURL + "@" + layer, PackagePURL: pkg.PURL}); err != nil {
		t.Fatal(err)
	}
	if err := ts.db.RecordImageManifestReferences(pkg.PURL, "sha256:old", []string{layer}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := ts.db.RecordImageTag(pkg.PURL, "oldest", "sha256:old", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	for i := range maxImageTagsShown + 1 {
		if err := ts.db.RecordImageTag(pkg.PURL, fmt.Sprintf("v%d", i), "sha256:new", now); err != nil {
			t.Fatal(err)
		}
	}

	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ui/package/oci/team/app/"+layer, nil))
	if !strings.Contains(w.Body.String(), ">oldest<") {
		t.Errorf("status %d: version page does not name the oldest tag", w.Code)
	}
}
