package server

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/git-pkgs/proxy/internal/database"
)

func TestTemplatesRenderAllPages(t *testing.T) {
	templates := &Templates{}

	tests := []struct {
		page string
		data any
	}{
		{"dashboard", DashboardData{
			Stats: DashboardStats{
				CachedArtifacts: 42,
				TotalSize:       "1.5 GB",
				TotalPackages:   10,
				TotalVersions:   25,
			},
			EnrichmentStats: EnrichmentStatsView{
				EnrichedPackages:     5,
				TotalVulnerabilities: 3,
				CriticalVulns:        1,
				HasVulns:             true,
			},
			PopularPackages: []PackageInfo{
				{Ecosystem: "npm", Name: "lodash", Hits: 100, Size: "500 KB", License: "MIT", LicenseCategory: "permissive"},
			},
			RecentPackages: []PackageInfo{
				{Ecosystem: "cargo", Name: "serde", Version: "1.0.0", Size: "200 KB", CachedAt: "1 hour ago"},
			},
		}},
		{"install", struct{ Registries []RegistryConfig }{
			Registries: getRegistryConfigs("http://localhost:8080"),
		}},
		{"search", SearchPageData{
			Query:      "lodash",
			Ecosystem:  "npm",
			Results:    []SearchResultItem{{Ecosystem: "npm", Name: "lodash", LatestVersion: "4.17.21", Hits: 50, SizeFormatted: "1 MB"}},
			Count:      1,
			Page:       1,
			PerPage:    50,
			TotalPages: 1,
		}},
		{"search", SearchPageData{
			Query:      "nothing",
			Results:    []SearchResultItem{},
			Count:      0,
			Page:       1,
			PerPage:    50,
			TotalPages: 0,
		}},
		{"packages_list", PackagesListPageData{
			Ecosystem:  "",
			SortBy:     defaultSortBy,
			Results:    []SearchResultItem{{Ecosystem: "npm", Name: "express", Hits: 200, SizeFormatted: "2 MB"}},
			Count:      1,
			Page:       1,
			PerPage:    50,
			TotalPages: 1,
		}},
		{"package_show", PackageShowData{
			Package: &database.Package{
				PURL:          "pkg:npm/lodash",
				Ecosystem:     "npm",
				Name:          "lodash",
				LatestVersion: sql.NullString{String: "4.17.21", Valid: true},
				License:       sql.NullString{String: "MIT", Valid: true},
			},
			Versions: []database.Version{
				{PURL: "pkg:npm/lodash@4.17.21", PackagePURL: "pkg:npm/lodash"},
			},
			Vulnerabilities: []database.Vulnerability{},
			LicenseCategory: "permissive",
		}},
		{"package_show", PackageShowData{
			Package: &database.Package{
				PURL:      "pkg:npm/minimal",
				Ecosystem: "npm",
				Name:      "minimal",
			},
			Versions:        []database.Version{},
			Vulnerabilities: []database.Vulnerability{},
			LicenseCategory: "unknown",
		}},
		{"version_show", VersionShowData{
			Package: &database.Package{
				PURL:          "pkg:npm/lodash",
				Ecosystem:     "npm",
				Name:          "lodash",
				LatestVersion: sql.NullString{String: "4.17.21", Valid: true},
				License:       sql.NullString{String: "MIT", Valid: true},
			},
			Version: &database.Version{
				PURL:        "pkg:npm/lodash@4.17.20",
				PackagePURL: "pkg:npm/lodash",
			},
			Artifacts: []database.Artifact{
				{
					VersionPURL: "pkg:npm/lodash@4.17.20",
					Filename:    "lodash-4.17.20.tgz",
					StoragePath: sql.NullString{String: "npm/lodash/4.17.20/lodash-4.17.20.tgz", Valid: true},
					Size:        sql.NullInt64{Int64: 1024, Valid: true},
					HitCount:    42,
				},
			},
			Vulnerabilities:   []database.Vulnerability{},
			IsOutdated:        true,
			LicenseCategory:   "permissive",
			HasCachedArtifact: true,
		}},
		{"browse_source", BrowseSourceData{
			Ecosystem:   "npm",
			PackageName: "lodash",
			Version:     "4.17.21",
		}},
		{"compare_versions", ComparePageData{
			Ecosystem:   "npm",
			PackageName: "lodash",
			FromVersion: "4.17.20",
			ToVersion:   "4.17.21",
		}},
	}

	for _, tt := range tests {
		t.Run(tt.page, func(t *testing.T) {
			w := httptest.NewRecorder()
			err := templates.Render(w, tt.page, tt.data)
			if err != nil {
				t.Fatalf("Render(%q) failed: %v", tt.page, err)
			}
			if w.Code != http.StatusOK {
				t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
			}
			if w.Header().Get("Content-Type") != "text/html; charset=utf-8" {
				t.Errorf("Content-Type = %q, want text/html", w.Header().Get("Content-Type"))
			}
			body := w.Body.String()
			if body == "" {
				t.Error("rendered page is empty")
			}
			if !strings.Contains(body, "<!DOCTYPE html>") && !strings.Contains(body, "<html") {
				t.Error("rendered page doesn't look like HTML")
			}
		})
	}
}

func TestTemplatesLazyLoading(t *testing.T) {
	templates := &Templates{}

	if templates.pages != nil {
		t.Fatal("expected pages to be nil before first Render call")
	}

	w := httptest.NewRecorder()
	_ = templates.Render(w, "dashboard", DashboardData{})

	if templates.pages == nil {
		t.Fatal("expected pages to be populated after first Render call")
	}
}

func TestTemplatesRenderUnknownPage(t *testing.T) {
	templates := &Templates{}

	w := httptest.NewRecorder()
	err := templates.Render(w, "nonexistent_page", nil)
	if err == nil {
		t.Error("expected error for unknown page")
	}
}

func TestInstallPage(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	req := httptest.NewRequest("GET", "/install", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	body := w.Body.String()

	// Should contain instructions for all registries
	registries := []string{"npm", "Cargo", "RubyGems", "Go Modules", "PyPI", "Maven", "NuGet", "Composer", "Conan", "Conda", "CRAN"}
	for _, reg := range registries {
		if !strings.Contains(body, reg) {
			t.Errorf("install page should contain %s instructions", reg)
		}
	}
}

func TestPackageShowPage(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	// Create a package with license and version
	pkg := &database.Package{
		PURL:          "pkg:npm/test-show",
		Ecosystem:     "npm",
		Name:          "test-show",
		LatestVersion: sql.NullString{String: "2.0.0", Valid: true},
		License:       sql.NullString{String: "MIT", Valid: true},
	}
	if err := ts.db.UpsertPackage(pkg); err != nil {
		t.Fatalf("failed to upsert package: %v", err)
	}
	ver := &database.Version{PURL: "pkg:npm/test-show@2.0.0", PackagePURL: pkg.PURL}
	if err := ts.db.UpsertVersion(ver); err != nil {
		t.Fatalf("failed to upsert version: %v", err)
	}

	req := httptest.NewRequest("GET", "/package/npm/test-show", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	if !strings.Contains(body, "test-show") {
		t.Error("page should contain package name")
	}
	if !strings.Contains(body, "2.0.0") {
		t.Error("page should contain version")
	}
	if !strings.Contains(body, "MIT") {
		t.Error("page should contain license")
	}
}

func TestPackageShowPage_NotFound(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	req := httptest.NewRequest("GET", "/package/npm/nonexistent", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", w.Code)
	}
}

func TestVersionShowPage_NotFound(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	req := httptest.NewRequest("GET", "/package/npm/nonexistent/1.0.0", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", w.Code)
	}
}

func TestSearchPage_EmptyQuery(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	req := httptest.NewRequest("GET", "/search", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	// Empty query should redirect to homepage
	if w.Code != http.StatusSeeOther {
		t.Errorf("expected redirect (303), got %d", w.Code)
	}
}

func TestSearchPage_WithQuery(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	req := httptest.NewRequest("GET", "/search?q=test", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "test") {
		t.Error("search page should contain the query")
	}
}

func TestSearchPage_Pagination(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	// Page 0 or negative should default to page 1
	req := httptest.NewRequest("GET", "/search?q=test&page=0", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	// Non-numeric page should default to page 1
	req = httptest.NewRequest("GET", "/search?q=test&page=abc", nil)
	w = httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
}

func TestSearchPage_EcosystemFilter(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	req := httptest.NewRequest("GET", "/search?q=test&ecosystem=npm", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
}

func TestEcosystemBadgeLabel(t *testing.T) {
	tests := []struct {
		ecosystem string
		want      string
	}{
		{"oci", "container"},
		{"deb", "debian"},
		{"npm", "npm"},
		{"cargo", "cargo"},
	}

	for _, tt := range tests {
		got := ecosystemBadgeLabel(tt.ecosystem)
		if got != tt.want {
			t.Errorf("ecosystemBadgeLabel(%q) = %q, want %q", tt.ecosystem, got, tt.want)
		}
	}
}

func TestEcosystemBadgeClasses(t *testing.T) {
	// Every supported ecosystem should return a non-empty class string
	ecosystems := supportedEcosystems()
	for _, eco := range ecosystems {
		classes := ecosystemBadgeClasses(eco)
		if classes == "" {
			t.Errorf("ecosystemBadgeClasses(%q) returned empty string", eco)
		}
		if !strings.Contains(classes, "inline-flex") {
			t.Errorf("ecosystemBadgeClasses(%q) missing base classes", eco)
		}
	}

	// Unknown ecosystem should still return classes
	classes := ecosystemBadgeClasses("unknown")
	if !strings.Contains(classes, "gray") {
		t.Error("unknown ecosystem should use gray classes")
	}
}

func TestFormatSize(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{100, "100 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
		{1073741824, "1.0 GB"},
	}

	for _, tt := range tests {
		got := formatSize(tt.bytes)
		if got != tt.want {
			t.Errorf("formatSize(%d) = %q, want %q", tt.bytes, got, tt.want)
		}
	}
}

func TestCategorizeLicense(t *testing.T) {
	tests := []struct {
		license sql.NullString
		want    string
	}{
		{sql.NullString{Valid: false}, "unknown"},
		{sql.NullString{String: "", Valid: true}, "unknown"},
		{sql.NullString{String: "MIT", Valid: true}, "permissive"},
		{sql.NullString{String: "GPL-3.0", Valid: true}, "copyleft"},
	}

	for _, tt := range tests {
		got := categorizeLicense(tt.license)
		if got != tt.want {
			t.Errorf("categorizeLicense(%v) = %q, want %q", tt.license, got, tt.want)
		}
	}
}

func BenchmarkTemplatesParse(b *testing.B) {
	for b.Loop() {
		t := &Templates{}
		if err := t.load(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkServerCreate(b *testing.B) {
	for b.Loop() {
		_ = &Server{
			templates: &Templates{},
		}
	}
}

func BenchmarkFirstRender(b *testing.B) {
	for b.Loop() {
		t := &Templates{}
		w := httptest.NewRecorder()
		if err := t.Render(w, "dashboard", DashboardData{}); err != nil {
			b.Fatal(err)
		}
	}
}
