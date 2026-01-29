package server

import (
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

	mux := http.NewServeMux()

	// Mount handlers
	npmHandler := handler.NewNPMHandler(proxy, cfg.BaseURL)
	cargoHandler := handler.NewCargoHandler(proxy, cfg.BaseURL)
	gemHandler := handler.NewGemHandler(proxy, cfg.BaseURL)
	goHandler := handler.NewGoHandler(proxy, cfg.BaseURL)
	pypiHandler := handler.NewPyPIHandler(proxy, cfg.BaseURL)

	mux.Handle("GET /npm/{path...}", http.StripPrefix("/npm", npmHandler.Routes()))
	mux.Handle("GET /cargo/{path...}", http.StripPrefix("/cargo", cargoHandler.Routes()))
	mux.Handle("GET /gem/{path...}", http.StripPrefix("/gem", gemHandler.Routes()))
	mux.Handle("GET /go/{path...}", http.StripPrefix("/go", goHandler.Routes()))
	mux.Handle("GET /pypi/{path...}", http.StripPrefix("/pypi", pypiHandler.Routes()))

	// Create a minimal server struct for the handlers
	s := &Server{
		cfg:     cfg,
		db:      db,
		storage: store,
		logger:  logger,
	}

	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /stats", s.handleStats)
	mux.Handle("GET /static/", http.StripPrefix("/static/", staticHandler()))
	mux.HandleFunc("GET /{$}", s.handleRoot)

	return &testServer{
		handler: mux,
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
	if !strings.Contains(body, "git-pkgs proxy") {
		t.Error("dashboard should contain title")
	}
	if !strings.Contains(body, "Cached Artifacts") {
		t.Error("dashboard should contain stats")
	}
	if !strings.Contains(body, "Configure Your Package Manager") {
		t.Error("dashboard should contain configuration section")
	}
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
		path        string
		contentType string
	}{
		{"/static/tailwind.js", "text/javascript"},
		{"/static/style.css", "text/css"},
	}

	for _, tc := range tests {
		req := httptest.NewRequest("GET", tc.path, nil)
		w := httptest.NewRecorder()
		ts.handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("%s: expected status 200, got %d", tc.path, w.Code)
		}

		contentType := w.Header().Get("Content-Type")
		if !strings.Contains(contentType, tc.contentType) {
			t.Errorf("%s: expected Content-Type containing %s, got %q", tc.path, tc.contentType, contentType)
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
