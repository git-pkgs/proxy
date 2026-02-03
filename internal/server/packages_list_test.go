package server

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/enrichment"
)

func TestHandlePackagesList(t *testing.T) {
	// Create a real database for testing
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := database.Create(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	// Create test data
	pkg1 := &database.Package{
		PURL:          "pkg:npm/lodash",
		Ecosystem:     "npm",
		Name:          "lodash",
		LatestVersion: sql.NullString{String: "4.17.21", Valid: true},
		License:       sql.NullString{String: "MIT", Valid: true},
	}
	pkg2 := &database.Package{
		PURL:          "pkg:cargo/serde",
		Ecosystem:     "cargo",
		Name:          "serde",
		LatestVersion: sql.NullString{String: "1.0.0", Valid: true},
		License:       sql.NullString{String: "MIT OR Apache-2.0", Valid: true},
	}
	_ = db.UpsertPackage(pkg1)
	_ = db.UpsertPackage(pkg2)

	ver1 := &database.Version{
		PURL:        "pkg:npm/lodash@4.17.21",
		PackagePURL: pkg1.PURL,
	}
	ver2 := &database.Version{
		PURL:        "pkg:cargo/serde@1.0.0",
		PackagePURL: pkg2.PURL,
	}
	_ = db.UpsertVersion(ver1)
	_ = db.UpsertVersion(ver2)

	art1 := &database.Artifact{
		VersionPURL: ver1.PURL,
		Filename:    "lodash.tgz",
		UpstreamURL: "https://example.com/lodash.tgz",
		StoragePath: sql.NullString{String: "/cache/lodash.tgz", Valid: true},
		Size:        sql.NullInt64{Int64: 1024, Valid: true},
		HitCount:    100,
		FetchedAt:   sql.NullTime{Time: time.Now(), Valid: true},
	}
	art2 := &database.Artifact{
		VersionPURL: ver2.PURL,
		Filename:    "serde.crate",
		UpstreamURL: "https://example.com/serde.crate",
		StoragePath: sql.NullString{String: "/cache/serde.crate", Valid: true},
		Size:        sql.NullInt64{Int64: 2048, Valid: true},
		HitCount:    50,
		FetchedAt:   sql.NullTime{Time: time.Now(), Valid: true},
	}
	_ = db.UpsertArtifact(art1)
	_ = db.UpsertArtifact(art2)

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	enrichSvc := enrichment.New(logger)
	handler := NewAPIHandler(enrichSvc, db)

	t.Run("list all packages", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/packages", nil)
		w := httptest.NewRecorder()

		handler.HandlePackagesList(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", w.Code)
		}

		var resp PackagesListResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}

		if resp.Count != 2 {
			t.Errorf("expected count 2, got %d", resp.Count)
		}
		if resp.Total != 2 {
			t.Errorf("expected total 2, got %d", resp.Total)
		}
		if len(resp.Results) != 2 {
			t.Errorf("expected 2 results, got %d", len(resp.Results))
		}
		if resp.SortBy != "hits" {
			t.Errorf("expected default sort to be hits, got %s", resp.SortBy)
		}
	})

	t.Run("with ecosystem filter", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/packages?ecosystem=npm", nil)
		w := httptest.NewRecorder()

		handler.HandlePackagesList(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", w.Code)
		}

		var resp PackagesListResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}

		if resp.Ecosystem != "npm" {
			t.Errorf("expected ecosystem npm, got %s", resp.Ecosystem)
		}
		if resp.Count != 1 {
			t.Errorf("expected count 1, got %d", resp.Count)
		}
	})

	t.Run("with sort parameter", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/packages?sort=name", nil)
		w := httptest.NewRecorder()

		handler.HandlePackagesList(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", w.Code)
		}

		var resp PackagesListResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}

		if resp.SortBy != "name" {
			t.Errorf("expected sort by name, got %s", resp.SortBy)
		}
	})

	t.Run("invalid sort parameter", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/packages?sort=invalid", nil)
		w := httptest.NewRecorder()

		handler.HandlePackagesList(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected status 400, got %d", w.Code)
		}
	})
}
