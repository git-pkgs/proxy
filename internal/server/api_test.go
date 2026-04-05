package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/enrichment"
	"github.com/go-chi/chi/v5"
)

const testEcosystemNPM = "npm"

func TestNewAPIHandler(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	svc := enrichment.New(logger)
	h := NewAPIHandler(svc, nil)

	if h == nil {
		t.Fatal("NewAPIHandler returned nil")
	}
	if h.enrichment == nil {
		t.Error("enrichment service is nil")
	}
}

func TestHandlePackagePath_MissingParams(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	svc := enrichment.New(logger)
	h := NewAPIHandler(svc, nil)

	r := chi.NewRouter()
	r.Get("/api/package/{ecosystem}/*", h.HandlePackagePath)

	req := httptest.NewRequest("GET", "/api/package//", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest && w.Code != http.StatusNotFound {
		t.Errorf("expected status 400 or 404, got %d", w.Code)
	}
}

func TestHandleVulnsPath_MissingParams(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	svc := enrichment.New(logger)
	h := NewAPIHandler(svc, nil)

	r := chi.NewRouter()
	r.Get("/api/vulns/{ecosystem}/*", h.HandleVulnsPath)

	req := httptest.NewRequest("GET", "/api/vulns//", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest && w.Code != http.StatusNotFound {
		t.Errorf("expected status 400 or 404, got %d", w.Code)
	}
}

func TestHandleOutdated_EmptyBody(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	svc := enrichment.New(logger)
	h := NewAPIHandler(svc, nil)

	req := httptest.NewRequest("POST", "/api/outdated", bytes.NewReader([]byte("{}")))
	w := httptest.NewRecorder()
	h.HandleOutdated(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestHandleOutdated_OversizedBody(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	svc := enrichment.New(logger)
	h := NewAPIHandler(svc, nil)

	// Send a body larger than 1 MB
	body := make([]byte, 2<<20)
	for i := range body {
		body[i] = 'x'
	}
	req := httptest.NewRequest("POST", "/api/outdated", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleOutdated(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d for oversized body, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestHandleOutdated_InvalidJSON(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	svc := enrichment.New(logger)
	h := NewAPIHandler(svc, nil)

	req := httptest.NewRequest("POST", "/api/outdated", bytes.NewReader([]byte("not json")))
	w := httptest.NewRecorder()
	h.HandleOutdated(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestHandleBulkLookup_EmptyBody(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	svc := enrichment.New(logger)
	h := NewAPIHandler(svc, nil)

	req := httptest.NewRequest("POST", "/api/bulk", bytes.NewReader([]byte("{}")))
	w := httptest.NewRecorder()
	h.HandleBulkLookup(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestHandleBulkLookup_InvalidJSON(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	svc := enrichment.New(logger)
	h := NewAPIHandler(svc, nil)

	req := httptest.NewRequest("POST", "/api/bulk", bytes.NewReader([]byte("not json")))
	w := httptest.NewRecorder()
	h.HandleBulkLookup(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestWriteJSON(t *testing.T) {
	w := httptest.NewRecorder()

	data := map[string]string{"foo": "bar"}
	writeJSON(w, data)

	if w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("expected Content-Type application/json, got %s", w.Header().Get("Content-Type"))
	}

	var result map[string]string
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if result["foo"] != "bar" {
		t.Errorf("expected foo=bar, got foo=%s", result["foo"])
	}
}

func TestPackageResponseJSON(t *testing.T) {
	resp := &PackageResponse{
		Ecosystem:       testEcosystemNPM,
		Name:            "lodash",
		LatestVersion:   "4.17.21",
		License:         "MIT",
		LicenseCategory: "permissive",
		Description:     "A utility library",
		Homepage:        "https://lodash.com",
		Repository:      "https://github.com/lodash/lodash",
		RegistryURL:     "https://registry.npmjs.org",
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var decoded PackageResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if decoded.Ecosystem != testEcosystemNPM {
		t.Errorf("expected ecosystem npm, got %s", decoded.Ecosystem)
	}
	if decoded.Name != "lodash" {
		t.Errorf("expected name lodash, got %s", decoded.Name)
	}
}

func TestVulnResponseJSON(t *testing.T) {
	resp := &VulnResponse{
		ID:           "CVE-2021-1234",
		Summary:      "Test vulnerability",
		Severity:     "HIGH",
		CVSSScore:    8.5,
		FixedVersion: "1.2.3",
		References:   []string{"https://example.com/advisory"},
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var decoded VulnResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if decoded.ID != "CVE-2021-1234" {
		t.Errorf("expected ID CVE-2021-1234, got %s", decoded.ID)
	}
	if decoded.CVSSScore != 8.5 {
		t.Errorf("expected CVSS 8.5, got %f", decoded.CVSSScore)
	}
}

func TestOutdatedRequestJSON(t *testing.T) {
	req := &OutdatedRequest{
		Packages: []OutdatedPackage{
			{Ecosystem: "npm", Name: "lodash", Version: "4.17.0"},
			{Ecosystem: "pypi", Name: "requests", Version: "2.25.0"},
		},
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var decoded OutdatedRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if len(decoded.Packages) != 2 {
		t.Errorf("expected 2 packages, got %d", len(decoded.Packages))
	}
}

func TestBulkRequestJSON(t *testing.T) {
	req := &BulkRequest{
		PURLs: []string{
			"pkg:npm/lodash@4.17.21",
			"pkg:pypi/requests@2.28.0",
		},
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var decoded BulkRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if len(decoded.PURLs) != 2 {
		t.Errorf("expected 2 purls, got %d", len(decoded.PURLs))
	}
}

func TestHandleSearch_MissingQuery(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	svc := enrichment.New(logger)
	h := NewAPIHandler(svc, nil)

	req := httptest.NewRequest("GET", "/api/search", nil)
	w := httptest.NewRecorder()
	h.HandleSearch(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestHandleSearch_WithNullValues(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	svc := enrichment.New(logger)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := database.Create(dbPath)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	defer func() { _ = db.Close() }()

	pkg := &database.Package{
		PURL:      "pkg:npm/api-test",
		Ecosystem: testEcosystemNPM,
		Name:      "api-test",
	}
	if err := db.UpsertPackage(pkg); err != nil {
		t.Fatalf("UpsertPackage failed: %v", err)
	}

	ver := &database.Version{
		PURL:        "pkg:npm/api-test@1.0.0",
		PackagePURL: pkg.PURL,
	}
	if err := db.UpsertVersion(ver); err != nil {
		t.Fatalf("UpsertVersion failed: %v", err)
	}

	artifact := &database.Artifact{
		VersionPURL: ver.PURL,
		Filename:    "api-test-1.0.0.tgz",
		UpstreamURL: "https://registry.npmjs.org/api-test/-/api-test-1.0.0.tgz",
		StoragePath: sql.NullString{String: "./cache/test.tgz", Valid: true},
		HitCount:    3,
	}
	if err := db.UpsertArtifact(artifact); err != nil {
		t.Fatalf("UpsertArtifact failed: %v", err)
	}

	h := NewAPIHandler(svc, db)

	req := httptest.NewRequest("GET", "/api/search?q=api-test", nil)
	w := httptest.NewRecorder()
	h.HandleSearch(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp SearchResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(resp.Results))
	}

	result := resp.Results[0]
	if result.Name != "api-test" {
		t.Errorf("expected name api-test, got %s", result.Name)
	}
	if result.LatestVersion != "" {
		t.Errorf("expected empty LatestVersion, got %s", result.LatestVersion)
	}
	if result.License != "" {
		t.Errorf("expected empty License, got %s", result.License)
	}
	if result.Hits != 3 {
		t.Errorf("expected 3 hits, got %d", result.Hits)
	}
}

func TestHandlePackagesListAPI(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	svc := enrichment.New(logger)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := database.Create(dbPath)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Seed two packages
	for _, name := range []string{"api-list-one", "api-list-two"} {
		pkg := &database.Package{
			PURL:      "pkg:npm/" + name,
			Ecosystem: testEcosystemNPM,
			Name:      name,
		}
		if err := db.UpsertPackage(pkg); err != nil {
			t.Fatalf("UpsertPackage failed: %v", err)
		}
		ver := &database.Version{
			PURL:        "pkg:npm/" + name + "@1.0.0",
			PackagePURL: pkg.PURL,
		}
		if err := db.UpsertVersion(ver); err != nil {
			t.Fatalf("UpsertVersion failed: %v", err)
		}
		art := &database.Artifact{
			VersionPURL: ver.PURL,
			Filename:    name + "-1.0.0.tgz",
			UpstreamURL: "https://registry.npmjs.org/" + name + "/-/" + name + "-1.0.0.tgz",
			StoragePath: sql.NullString{String: "/tmp/test.tgz", Valid: true},
		}
		if err := db.UpsertArtifact(art); err != nil {
			t.Fatalf("UpsertArtifact failed: %v", err)
		}
	}

	h := NewAPIHandler(svc, db)

	r := chi.NewRouter()
	r.Get("/api/packages", h.HandlePackagesList)

	req := httptest.NewRequest("GET", "/api/packages", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var resp PackagesListResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Results) < 2 {
		t.Fatalf("expected at least 2 results, got %d", len(resp.Results))
	}

	if resp.SortBy != defaultSortBy {
		t.Errorf("expected default sort by hits, got %q", resp.SortBy)
	}

	found := false
	for _, pkg := range resp.Results {
		if pkg.Name == "api-list-one" || pkg.Name == "api-list-two" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected seeded packages in results")
	}
}

func TestHandlePackagesListAPI_InvalidSort(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	svc := enrichment.New(logger)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := database.Create(dbPath)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	defer func() { _ = db.Close() }()

	h := NewAPIHandler(svc, db)

	r := chi.NewRouter()
	r.Get("/api/packages", h.HandlePackagesList)

	req := httptest.NewRequest("GET", "/api/packages?sort=invalid", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 for invalid sort, got %d", w.Code)
	}
}
