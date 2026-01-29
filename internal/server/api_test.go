package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/git-pkgs/proxy/internal/enrichment"
)

func TestNewAPIHandler(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	svc := enrichment.New(logger)
	h := NewAPIHandler(svc)

	if h == nil {
		t.Fatal("NewAPIHandler returned nil")
	}
	if h.enrichment == nil {
		t.Error("enrichment service is nil")
	}
}

func TestHandleGetPackage_MissingParams(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	svc := enrichment.New(logger)
	h := NewAPIHandler(svc)

	req := httptest.NewRequest("GET", "/api/package//", nil)
	req.SetPathValue("ecosystem", "")
	req.SetPathValue("name", "")

	w := httptest.NewRecorder()
	h.HandleGetPackage(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestHandleGetVersion_MissingParams(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	svc := enrichment.New(logger)
	h := NewAPIHandler(svc)

	req := httptest.NewRequest("GET", "/api/package///", nil)
	req.SetPathValue("ecosystem", "")
	req.SetPathValue("name", "")
	req.SetPathValue("version", "")

	w := httptest.NewRecorder()
	h.HandleGetVersion(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestHandleGetVulns_MissingParams(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	svc := enrichment.New(logger)
	h := NewAPIHandler(svc)

	req := httptest.NewRequest("GET", "/api/vulns//", nil)
	req.SetPathValue("ecosystem", "")
	req.SetPathValue("name", "")

	w := httptest.NewRecorder()
	h.HandleGetVulns(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestHandleOutdated_EmptyBody(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	svc := enrichment.New(logger)
	h := NewAPIHandler(svc)

	req := httptest.NewRequest("POST", "/api/outdated", bytes.NewReader([]byte("{}")))
	w := httptest.NewRecorder()
	h.HandleOutdated(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestHandleOutdated_InvalidJSON(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	svc := enrichment.New(logger)
	h := NewAPIHandler(svc)

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
	h := NewAPIHandler(svc)

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
	h := NewAPIHandler(svc)

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
		Ecosystem:       "npm",
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

	if decoded.Ecosystem != "npm" {
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
