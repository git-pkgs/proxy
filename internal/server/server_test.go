package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/storage"
	"github.com/git-pkgs/registries/fetch"
	"github.com/go-chi/chi/v5"
)

type testServer struct {
	handler http.Handler
	db      *database.DB
	storage storage.Storage
	tempDir string
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()

	tempDir, err := os.MkdirTemp("", "proxy-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	dbPath := filepath.Join(tempDir, "test.db")
	storagePath := filepath.Join(tempDir, "artifacts")

	db, err := database.Create(dbPath)
	if err != nil {
		_ = os.RemoveAll(tempDir)
		t.Fatalf("failed to create database: %v", err)
	}

	store, err := storage.NewFilesystem(storagePath)
	if err != nil {
		_ = db.Close()
		_ = os.RemoveAll(tempDir)
		t.Fatalf("failed to create storage: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	fetcher := fetch.NewFetcher()
	resolver := fetch.NewResolver()
	proxy := handler.NewProxy(db, store, fetcher, resolver, logger)

	cfg := &config.Config{
		BaseURL:  "http://localhost:8080",
		Storage:  config.StorageConfig{Path: storagePath},
		Database: config.DatabaseConfig{Path: dbPath},
	}

	r := chi.NewRouter()

	// Mount handlers
	npmHandler := handler.NewNPMHandler(proxy, cfg.BaseURL)
	cargoHandler := handler.NewCargoHandler(proxy, cfg.BaseURL)
	gemHandler := handler.NewGemHandler(proxy, cfg.BaseURL)
	goHandler := handler.NewGoHandler(proxy, cfg.BaseURL)
	pypiHandler := handler.NewPyPIHandler(proxy, cfg.BaseURL)

	r.Mount("/npm", http.StripPrefix("/npm", npmHandler.Routes()))
	r.Mount("/cargo", http.StripPrefix("/cargo", cargoHandler.Routes()))
	r.Mount("/gem", http.StripPrefix("/gem", gemHandler.Routes()))
	r.Mount("/go", http.StripPrefix("/go", goHandler.Routes()))
	r.Mount("/pypi", http.StripPrefix("/pypi", pypiHandler.Routes()))

	// Create a minimal server struct for the handlers
	s := &Server{
		cfg:       cfg,
		db:        db,
		storage:   store,
		logger:    logger,
		templates: &Templates{},
	}

	r.Get("/health", s.handleHealth)
	r.Get("/stats", s.handleStats)
	r.Get("/openapi.json", s.handleOpenAPIJSON)
	r.Mount("/static", http.StripPrefix("/static/", staticHandler()))
	r.Get("/search", s.handleSearch)
	r.Get("/package/{ecosystem}/{name}", s.handlePackageShow)
	r.Get("/package/{ecosystem}/{name}/{version}", s.handleVersionShow)
	r.Get("/package/{ecosystem}/{name}/{version}/browse", s.handleBrowseSource)
	r.Get("/api/browse/{ecosystem}/{name}/{version}", s.handleBrowseList)
	r.Get("/api/browse/{ecosystem}/{name}/{version}/file/*", s.handleBrowseFile)
	r.Get("/api/compare/{ecosystem}/{name}/{fromVersion}/{toVersion}", s.handleCompareDiff)
	r.Get("/package/{ecosystem}/{name}/compare/{versions}", s.handleComparePage)
	r.Get("/", s.handleRoot)
	r.Get("/install", s.handleInstall)
	r.Get("/packages", s.handlePackagesList)

	return &testServer{
		handler: r,
		db:      db,
		storage: store,
		tempDir: tempDir,
	}
}

func (ts *testServer) close() {
	_ = ts.db.Close()
	_ = os.RemoveAll(ts.tempDir)
}

// seedTestPackage creates a package, version, and artifact in the database for testing
// page rendering. The package is created under the npm ecosystem with version 1.0.0.
func seedTestPackage(t *testing.T, db *database.DB, name string) {
	t.Helper()

	pkg := &database.Package{
		PURL:      "pkg:npm/" + name,
		Ecosystem: "npm",
		Name:      name,
	}
	if err := db.UpsertPackage(pkg); err != nil {
		t.Fatalf("failed to upsert package: %v", err)
	}

	ver := &database.Version{
		PURL:        "pkg:npm/" + name + "@1.0.0",
		PackagePURL: pkg.PURL,
	}
	if err := db.UpsertVersion(ver); err != nil {
		t.Fatalf("failed to upsert version: %v", err)
	}

	artifact := &database.Artifact{
		VersionPURL: ver.PURL,
		Filename:    name + "-1.0.0.tgz",
		UpstreamURL: "https://registry.npmjs.org/" + name + "/-/" + name + "-1.0.0.tgz",
		StoragePath: sql.NullString{String: "/tmp/test.tgz", Valid: true},
	}
	if err := db.UpsertArtifact(artifact); err != nil {
		t.Fatalf("failed to upsert artifact: %v", err)
	}
}

func TestHandleOpenAPIJSON(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	contentType := w.Header().Get("Content-Type")
	if !strings.Contains(contentType, "application/json") {
		t.Fatalf("expected JSON content type, got %q", contentType)
	}

	if !strings.Contains(w.Body.String(), `"swagger": "2.0"`) {
		t.Fatalf("expected swagger document, got %q", w.Body.String())
	}
}

func TestHealthEndpoint(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	body := w.Body.String()
	if body != "ok" {
		t.Errorf("expected body 'ok', got %q", body)
	}
}

func TestStatsEndpoint(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	req := httptest.NewRequest("GET", "/stats", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	contentType := w.Header().Get("Content-Type")
	if contentType != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", contentType)
	}

	var stats StatsResponse
	if err := json.NewDecoder(w.Body).Decode(&stats); err != nil {
		t.Fatalf("failed to decode stats: %v", err)
	}

	if stats.CachedArtifacts != 0 {
		t.Errorf("expected 0 cached artifacts, got %d", stats.CachedArtifacts)
	}

	if !strings.HasPrefix(stats.StorageURL, "file://") {
		t.Errorf("expected storage_url to start with file://, got %q", stats.StorageURL)
	}
}

func TestDashboard(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	contentType := w.Header().Get("Content-Type")
	if !strings.HasPrefix(contentType, "text/html") {
		t.Errorf("expected Content-Type text/html, got %q", contentType)
	}

	body := w.Body.String()
	if body == "" {
		t.Fatal("dashboard returned empty body")
	}
	if !strings.Contains(body, "git-pkgs proxy") {
		t.Logf("Body: %s", body[:min(len(body), 500)])
		t.Error("dashboard should contain title")
	}
	if !strings.Contains(body, "Cached Artifacts") {
		t.Error("dashboard should contain stats")
	}
	if !strings.Contains(body, "Popular Packages") {
		t.Error("dashboard should contain popular packages section")
	}
	if !strings.Contains(body, ">composer<") {
		t.Error("dashboard should show composer in supported ecosystems")
	}
	if !strings.Contains(body, ">conan<") {
		t.Error("dashboard should show conan in supported ecosystems")
	}
	if !strings.Contains(body, ">container<") {
		t.Error("dashboard should show container in supported ecosystems")
	}
	if !strings.Contains(body, ">debian<") {
		t.Error("dashboard should show debian in supported ecosystems")
	}
	if !strings.Contains(body, "/openapi.json") {
		t.Error("page should link to the OpenAPI JSON spec")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestNPMPackageMetadata(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	// This will fail to fetch from upstream (no network in test),
	// but we can verify the handler is mounted and responds
	req := httptest.NewRequest("GET", "/npm/lodash", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	// Should get a bad gateway since we can't reach npm
	// The important thing is that the handler is mounted
	if w.Code == http.StatusNotFound {
		t.Error("npm handler should be mounted")
	}
}

func TestCargoConfig(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	req := httptest.NewRequest("GET", "/cargo/config.json", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var config map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&config); err != nil {
		t.Fatalf("failed to decode cargo config: %v", err)
	}

	if _, ok := config["dl"]; !ok {
		t.Error("cargo config should have 'dl' field")
	}
}

func TestGoList(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	// Test the /@v/list endpoint - should reach the handler even if upstream fails
	req := httptest.NewRequest("GET", "/go/example.com/test/@v/list", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	// The handler is mounted if we get a Go proxy error (not a generic 404)
	body := w.Body.String()
	if w.Code == http.StatusNotFound && !strings.Contains(body, "example.com") {
		t.Errorf("go handler should be mounted, got status %d, body: %s", w.Code, body)
	}
}

func TestPyPISimple(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	req := httptest.NewRequest("GET", "/pypi/simple/requests/", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code == http.StatusNotFound {
		t.Error("pypi handler should be mounted")
	}
}

func TestGemSpecs(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	req := httptest.NewRequest("GET", "/gem/specs.4.8.gz", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code == http.StatusNotFound {
		t.Error("gem handler should be mounted")
	}
}

func TestStaticFiles(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	tests := []struct {
		path         string
		contentTypes []string
	}{
		{"/static/tailwind.js", []string{"text/javascript", "application/javascript"}},
		{"/static/style.css", []string{"text/css"}},
	}

	for _, tc := range tests {
		req := httptest.NewRequest("GET", tc.path, nil)
		w := httptest.NewRecorder()
		ts.handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("%s: expected status 200, got %d", tc.path, w.Code)
		}

		contentType := w.Header().Get("Content-Type")
		found := false
		for _, ct := range tc.contentTypes {
			if strings.Contains(contentType, ct) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s: expected Content-Type containing one of %v, got %q", tc.path, tc.contentTypes, contentType)
		}
	}
}

func TestCategorizeLicenseCSS(t *testing.T) {
	tests := []struct {
		license  string
		expected string
	}{
		{"MIT", "permissive"},
		{"Apache-2.0", "permissive"},
		{"BSD-3-Clause", "permissive"},
		{"ISC", "permissive"},
		{"GPL-3.0", "copyleft"},
		{"AGPL-3.0", "copyleft"},
		{"LGPL-2.1", "copyleft"},
		{"MPL-2.0", "copyleft"},
		{"", "unknown"},
		{"Proprietary", "unknown"},
	}

	for _, tc := range tests {
		result := categorizeLicenseCSS(tc.license)
		if result != tc.expected {
			t.Errorf("categorizeLicenseCSS(%q) = %q, want %q", tc.license, result, tc.expected)
		}
	}
}

func TestDashboardWithEnrichmentStats(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	body := w.Body.String()

	// Dashboard should link to Tailwind JS
	if !strings.Contains(body, "/static/tailwind.js") {
		t.Error("dashboard should link to Tailwind JS")
	}

	// Dashboard should have dark mode toggle
	if !strings.Contains(body, "theme-toggle") {
		t.Error("dashboard should have dark mode toggle")
	}
}

func TestVersionShowWithHitCount(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	pkg := &database.Package{
		PURL:      "pkg:npm/test",
		Ecosystem: "npm",
		Name:      "test",
	}
	if err := ts.db.UpsertPackage(pkg); err != nil {
		t.Fatalf("failed to upsert package: %v", err)
	}

	ver := &database.Version{
		PURL:        "pkg:npm/test@1.0.0",
		PackagePURL: pkg.PURL,
	}
	if err := ts.db.UpsertVersion(ver); err != nil {
		t.Fatalf("failed to upsert version: %v", err)
	}

	artifact := &database.Artifact{
		VersionPURL: ver.PURL,
		Filename:    "test-1.0.0.tgz",
		UpstreamURL: "https://registry.npmjs.org/test/-/test-1.0.0.tgz",
		HitCount:    42,
	}
	if err := ts.db.UpsertArtifact(artifact); err != nil {
		t.Fatalf("failed to upsert artifact: %v", err)
	}

	req := httptest.NewRequest("GET", "/package/npm/test/1.0.0", nil)
	w := httptest.NewRecorder()

	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "42 cache hits") {
		t.Error("expected page to show hit count")
	}
}

func TestSearchWithNullValues(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	pkg := &database.Package{
		PURL:      "pkg:npm/test-pkg",
		Ecosystem: "npm",
		Name:      "test-pkg",
	}
	if err := ts.db.UpsertPackage(pkg); err != nil {
		t.Fatalf("failed to upsert package: %v", err)
	}

	ver := &database.Version{
		PURL:        "pkg:npm/test-pkg@1.0.0",
		PackagePURL: pkg.PURL,
	}
	if err := ts.db.UpsertVersion(ver); err != nil {
		t.Fatalf("failed to upsert version: %v", err)
	}

	storagePath := filepath.Join(ts.tempDir, "test.tgz")
	if err := os.WriteFile(storagePath, []byte("test content"), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	artifact := &database.Artifact{
		VersionPURL: ver.PURL,
		Filename:    "test-pkg-1.0.0.tgz",
		UpstreamURL: "https://registry.npmjs.org/test-pkg/-/test-pkg-1.0.0.tgz",
		StoragePath: sql.NullString{String: storagePath, Valid: true},
		HitCount:    5,
	}
	if err := ts.db.UpsertArtifact(artifact); err != nil {
		t.Fatalf("failed to upsert artifact: %v", err)
	}

	req := httptest.NewRequest("GET", "/search?q=test", nil)
	w := httptest.NewRecorder()

	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "test-pkg") {
		t.Error("expected search results to contain package name")
	}
}

func TestFormatTimeAgo_AllRanges(t *testing.T) {
	tests := []struct {
		name     string
		input    time.Time
		expected string
	}{
		{"zero time", time.Time{}, ""},
		{"now", time.Now(), "just now"},
		{"30 seconds ago", time.Now().Add(-30 * time.Second), "just now"},
		{"1 minute ago", time.Now().Add(-1 * time.Minute), "1 min ago"},
		{"5 minutes ago", time.Now().Add(-5 * time.Minute), "5 mins ago"},
		{"1 hour ago", time.Now().Add(-1 * time.Hour), "1 hour ago"},
		{"3 hours ago", time.Now().Add(-3 * time.Hour), "3 hours ago"},
		{"1 day ago", time.Now().Add(-24 * time.Hour), "1 day ago"},
		{"3 days ago", time.Now().Add(-3 * 24 * time.Hour), "3 days ago"},
		{"10 days ago", time.Now().Add(-10 * 24 * time.Hour), time.Now().Add(-10 * 24 * time.Hour).Format("Jan 2")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatTimeAgo(tc.input)
			if got != tc.expected {
				t.Errorf("formatTimeAgo() = %q, want %q", got, tc.expected)
			}
		})
	}
}

func TestFormatSize_AllUnits(t *testing.T) {
	tests := []struct {
		bytes    int64
		expected string
	}{
		{0, "0 B"},
		{500, "500 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
		{1073741824, "1.0 GB"},
	}

	for _, tc := range tests {
		t.Run(tc.expected, func(t *testing.T) {
			got := formatSize(tc.bytes)
			if got != tc.expected {
				t.Errorf("formatSize(%d) = %q, want %q", tc.bytes, got, tc.expected)
			}
		})
	}
}

func TestCategorizeLicense_NullString(t *testing.T) {
	tests := []struct {
		name     string
		license  sql.NullString
		expected string
	}{
		{"invalid null string", sql.NullString{Valid: false}, "unknown"},
		{"MIT", sql.NullString{String: "MIT", Valid: true}, "permissive"},
		{"GPL-3.0", sql.NullString{String: "GPL-3.0", Valid: true}, "copyleft"},
		{"empty string", sql.NullString{String: "", Valid: true}, "unknown"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := categorizeLicense(tc.license)
			if got != tc.expected {
				t.Errorf("categorizeLicense(%v) = %q, want %q", tc.license, got, tc.expected)
			}
		})
	}
}

func TestSearchRedirectsWhenEmpty(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	req := httptest.NewRequest("GET", "/search", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Errorf("expected status 303, got %d", w.Code)
	}

	loc := w.Header().Get("Location")
	if loc != "/" {
		t.Errorf("expected redirect to /, got %q", loc)
	}
}

func TestPackageShowPage_NotFoundServer(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	req := httptest.NewRequest("GET", "/package/npm/nonexistent-srv", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", w.Code)
	}
}

func TestVersionShowPage_NotFoundServer(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	req := httptest.NewRequest("GET", "/package/npm/nonexistent-srv/1.0.0", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", w.Code)
	}
}

func TestPackageShowPage_WithLicense(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	pkg := &database.Package{
		PURL:      "pkg:npm/show-test-lic",
		Ecosystem: "npm",
		Name:      "show-test-lic",
		License:   sql.NullString{String: "MIT", Valid: true},
	}
	if err := ts.db.UpsertPackage(pkg); err != nil {
		t.Fatalf("failed to upsert package: %v", err)
	}

	ver := &database.Version{
		PURL:        "pkg:npm/show-test-lic@1.0.0",
		PackagePURL: pkg.PURL,
	}
	if err := ts.db.UpsertVersion(ver); err != nil {
		t.Fatalf("failed to upsert version: %v", err)
	}

	req := httptest.NewRequest("GET", "/package/npm/show-test-lic", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "show-test-lic") {
		t.Error("expected page to contain the package name")
	}
}

func TestSearchPage_WithSeededResults(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	seedTestPackage(t, ts.db, "searchable-pkg")

	req := httptest.NewRequest("GET", "/search?q=searchable", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "searchable-pkg") {
		t.Error("expected search results to contain package name")
	}
}

func TestSearchPage_PaginationMultiPage(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	// Seed 55 packages to exceed one page (limit=50)
	for i := 0; i < 55; i++ {
		name := fmt.Sprintf("page-test-%03d", i)
		pkg := &database.Package{
			PURL:      fmt.Sprintf("pkg:npm/%s", name),
			Ecosystem: "npm",
			Name:      name,
		}
		if err := ts.db.UpsertPackage(pkg); err != nil {
			t.Fatalf("failed to upsert package %d: %v", i, err)
		}
		ver := &database.Version{
			PURL:        fmt.Sprintf("pkg:npm/%s@1.0.0", name),
			PackagePURL: pkg.PURL,
		}
		if err := ts.db.UpsertVersion(ver); err != nil {
			t.Fatalf("failed to upsert version %d: %v", i, err)
		}
		artifact := &database.Artifact{
			VersionPURL: ver.PURL,
			Filename:    fmt.Sprintf("%s-1.0.0.tgz", name),
			UpstreamURL: fmt.Sprintf("https://registry.npmjs.org/%s/-/%s-1.0.0.tgz", name, name),
			StoragePath: sql.NullString{String: "/tmp/test.tgz", Valid: true},
		}
		if err := ts.db.UpsertArtifact(artifact); err != nil {
			t.Fatalf("failed to upsert artifact %d: %v", i, err)
		}
	}

	// First page
	req := httptest.NewRequest("GET", "/search?q=page-test", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "page-test-") {
		t.Error("expected first page to contain results")
	}

	// Second page
	req = httptest.NewRequest("GET", "/search?q=page-test&page=2", nil)
	w = httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200 for page 2, got %d", w.Code)
	}
}

func TestSearchPage_EcosystemFilterWithSeededData(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	// Seed npm package
	npmPkg := &database.Package{
		PURL:      "pkg:npm/eco-filter-npm",
		Ecosystem: "npm",
		Name:      "eco-filter-npm",
	}
	if err := ts.db.UpsertPackage(npmPkg); err != nil {
		t.Fatalf("failed to upsert npm package: %v", err)
	}
	npmVer := &database.Version{
		PURL:        "pkg:npm/eco-filter-npm@1.0.0",
		PackagePURL: npmPkg.PURL,
	}
	if err := ts.db.UpsertVersion(npmVer); err != nil {
		t.Fatalf("failed to upsert npm version: %v", err)
	}
	npmArt := &database.Artifact{
		VersionPURL: npmVer.PURL,
		Filename:    "eco-filter-npm-1.0.0.tgz",
		UpstreamURL: "https://registry.npmjs.org/eco-filter-npm/-/eco-filter-npm-1.0.0.tgz",
		StoragePath: sql.NullString{String: "/tmp/test.tgz", Valid: true},
	}
	if err := ts.db.UpsertArtifact(npmArt); err != nil {
		t.Fatalf("failed to upsert npm artifact: %v", err)
	}

	// Seed pypi package
	pypiPkg := &database.Package{
		PURL:      "pkg:pypi/eco-filter-pypi",
		Ecosystem: "pypi",
		Name:      "eco-filter-pypi",
	}
	if err := ts.db.UpsertPackage(pypiPkg); err != nil {
		t.Fatalf("failed to upsert pypi package: %v", err)
	}
	pypiVer := &database.Version{
		PURL:        "pkg:pypi/eco-filter-pypi@1.0.0",
		PackagePURL: pypiPkg.PURL,
	}
	if err := ts.db.UpsertVersion(pypiVer); err != nil {
		t.Fatalf("failed to upsert pypi version: %v", err)
	}
	pypiArt := &database.Artifact{
		VersionPURL: pypiVer.PURL,
		Filename:    "eco-filter-pypi-1.0.0.tar.gz",
		UpstreamURL: "https://files.pythonhosted.org/eco-filter-pypi-1.0.0.tar.gz",
		StoragePath: sql.NullString{String: "/tmp/test.tar.gz", Valid: true},
	}
	if err := ts.db.UpsertArtifact(pypiArt); err != nil {
		t.Fatalf("failed to upsert pypi artifact: %v", err)
	}

	// Search with ecosystem filter for npm only
	req := httptest.NewRequest("GET", "/search?q=eco-filter&ecosystem=npm", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "eco-filter-npm") {
		t.Error("expected npm package in filtered results")
	}
	if strings.Contains(body, "eco-filter-pypi") {
		t.Error("did not expect pypi package in npm-filtered results")
	}
}

func TestHandlePackagesListPage(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	seedTestPackage(t, ts.db, "list-test")

	req := httptest.NewRequest("GET", "/packages", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "list-test") {
		t.Error("expected packages list to contain seeded package")
	}
}

func TestNewServer_StorageConnectivityCheck(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test.db")
	storagePath := filepath.Join(tempDir, "artifacts")

	cfg := &config.Config{
		Listen:   ":0",
		BaseURL:  "http://localhost:8080",
		Storage:  config.StorageConfig{URL: "file://" + storagePath},
		Database: config.DatabaseConfig{Path: dbPath},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv, err := New(cfg, logger)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	// On Windows, OpenBucket normalises to file:///C:/path; on Unix the
	// absolute path already starts with /, so file:// + /path == file:///path.
	wantPrefix := "file://"
	wantSuffix := filepath.ToSlash(storagePath)
	got := srv.storage.URL()
	if !strings.HasPrefix(got, wantPrefix) || !strings.HasSuffix(got, wantSuffix) {
		t.Errorf("expected storage URL ending with %s, got %s", wantSuffix, got)
	}

	_ = srv.db.Close()
}

func TestStatsEndpoint_StorageURL(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()

	req := httptest.NewRequest("GET", "/stats", nil)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	// Verify the JSON response uses storage_url (not storage_path)
	body := w.Body.String()
	if !strings.Contains(body, `"storage_url"`) {
		t.Errorf("expected JSON key storage_url in response, got: %s", body)
	}
	if strings.Contains(body, `"storage_path"`) {
		t.Errorf("unexpected JSON key storage_path in response (should be storage_url)")
	}
}
