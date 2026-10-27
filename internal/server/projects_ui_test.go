package server

import (
	"database/sql"
	"net/http"
	"strings"
	"testing"

	"github.com/git-pkgs/proxy/internal/database"
)

// seedLodash records lodash with 4.17.21 as its latest and only cached
// version, and a vulnerability fixed in 4.17.21. The syft fixture uses
// 4.17.20.
func seedLodash(t *testing.T, db *database.DB, withVuln bool) {
	t.Helper()
	if err := db.UpsertPackage(&database.Package{
		PURL: "pkg:npm/lodash", Ecosystem: "npm", Name: "lodash",
		LatestVersion: sql.NullString{String: "4.17.21", Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertVersion(&database.Version{PURL: "pkg:npm/lodash@4.17.21", PackagePURL: "pkg:npm/lodash"}); err != nil {
		t.Fatal(err)
	}
	if !withVuln {
		return
	}
	if err := db.UpsertVulnerability(&database.Vulnerability{
		VulnID: "GHSA-35jh-r3h4-6jhm", Ecosystem: "npm", PackageName: "lodash",
		Severity:     sql.NullString{String: "high", Valid: true},
		FixedVersion: sql.NullString{String: "4.17.21", Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
}

func (ts projectsTestServer) page(t *testing.T, target string) string {
	t.Helper()
	rec := ts.do(t, http.MethodGet, target, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", target, rec.Code, rec.Body)
	}
	return rec.Body.String()
}

func assertContains(t *testing.T, page string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(page, w) {
			t.Errorf("page does not contain %q", w)
		}
	}
}

func TestProjectPageShowsComponents(t *testing.T) {
	ts := newProjectsTestServer(t)
	seedLodash(t, ts.db, true)
	ts.upload(t, "/api/projects/acme/web/sbom?branch=main", syftSBOM(t))

	page := ts.page(t, "/ui/projects/acme/web")
	assertContains(t, page,
		"acme/web",
		`href="/ui/package/npm/lodash"`, // cached, so linked
		"outdated, latest 4.17.21",
		"1 vulns",
		"1 vulnerable",
		"symfony/console", // not cached: listed without a link
		"not cached",
	)
	if strings.Contains(page, `href="/ui/package/composer/symfony/console"`) {
		t.Error("links to the package page of a package the proxy has no record of")
	}

	// Encoded as one segment, too.
	assertContains(t, ts.page(t, "/ui/projects/acme%2Fweb"), "symfony/console")

	if rec := ts.do(t, http.MethodGet, "/ui/projects/acme/other", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown project status = %d, want 404", rec.Code)
	}
	if rec := ts.do(t, http.MethodGet, "/ui/projects/acme/../web", "", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid project status = %d, want 400", rec.Code)
	}

	list := ts.page(t, "/ui/projects")
	assertContains(t, list, `href="/ui/projects/acme/web"`, "9 packages")
}

func TestPackagePageShowsUsedBy(t *testing.T) {
	ts := newProjectsTestServer(t)
	seedLodash(t, ts.db, false)
	ts.upload(t, "/api/projects/acme/web/sbom", syftSBOM(t))
	ts.upload(t, "/api/projects/acme/api/sbom", []byte(`{"bomFormat":"CycloneDX","specVersion":"1.5","components":[
		{"type":"library","name":"lodash","version":"4.17.21","purl":"pkg:npm/lodash@4.17.21"}]}`))

	page := ts.page(t, "/ui/package/npm/lodash")
	assertContains(t, page,
		"Used by",
		`href="/ui/package/npm/lodash/4.17.21"`,
		`href="/ui/projects/acme/api"`,
		`href="/ui/projects/acme/web"`,
	)
	// 4.17.20 is used but not cached.
	start := strings.Index(page, "Used by")
	if start < 0 {
		t.Fatal("no Used by section")
	}
	used := page[start:]
	if i := strings.Index(used, "4.17.20"); i < 0 || !strings.Contains(used[i:], "not cached") {
		t.Error("the uncached version 4.17.20 is not listed as not cached")
	}
	if strings.Index(used, "4.17.21") > strings.Index(used, "4.17.20") {
		t.Error("versions are not listed highest first")
	}
}

func TestDashboardShowsVulnerableVersionsInUse(t *testing.T) {
	ts := newProjectsTestServer(t)
	seedLodash(t, ts.db, false)
	ts.upload(t, "/api/projects/acme/web/sbom", syftSBOM(t))

	if page := ts.page(t, "/ui/"); strings.Contains(page, "Vulnerable versions in use") {
		t.Error("dashboard shows the vulnerable versions card without vulnerabilities")
	}
	assertContains(t, ts.page(t, "/ui/"), `href="/ui/projects"`)

	seedLodash(t, ts.db, true)
	page := ts.page(t, "/ui/")
	assertContains(t, page, "Vulnerable versions in use", "@4.17.20", `href="/ui/projects/acme/web"`, ">high<")

	// A project on the fixed version doesn't count.
	ts.upload(t, "/api/projects/acme/web/sbom", []byte(`{"bomFormat":"CycloneDX","specVersion":"1.5","components":[
		{"type":"library","name":"lodash","version":"4.17.21","purl":"pkg:npm/lodash@4.17.21"}]}`))
	if page := ts.page(t, "/ui/"); strings.Contains(page, "Vulnerable versions in use") {
		t.Error("dashboard lists a version the vulnerability is fixed in")
	}
}
