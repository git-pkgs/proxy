package server

import (
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/storage"
	"github.com/git-pkgs/proxy/internal/upstream"
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
	fetcher := upstream.New()
	resolver := upstream.NewResolver()
	proxy := handler.NewProxy(db, store, fetcher, resolver, logger)

	cfg := &config.Config{
		BaseURL: "http://localhost:8080",
		Storage: config.StorageConfig{Path: storagePath},
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

	// Load templates
	templates, err := NewTemplates()
	if err != nil {
		_ = db.Close()
		_ = os.RemoveAll(tempDir)
		t.Fatalf("failed to load templates: %v", err)
	}

	// Create a minimal server struct for the handlers
	s := &Server{
		cfg:       cfg,
		db:        db,
		storage:   store,
		logger:    logger,
		templates: templates,
	}

	r.Get("/health", s.handleHealth)
	r.Get("/stats", s.handleStats)
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
