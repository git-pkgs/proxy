package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/enrichment"
	"github.com/go-chi/chi/v5"
)

func TestAPIVersionAvailableAt(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	recent, old := now.Add(-time.Hour), now.Add(-96*time.Hour)
	h := &APIHandler{
		enrichment: fakeEnricher{published: map[string]time.Time{"1.0.0": old, "2.0.0": recent}},
		cooldown:   testCooldownView(t, config.CooldownConfig{Default: "3d"}),
	}
	r := chi.NewRouter()
	r.Get("/api/package/{ecosystem}/*", h.HandlePackagePath)

	tests := []struct{ version, availableAt string }{
		{"2.0.0", recent.Add(72 * time.Hour).Format(apiTimeFormat)},
		{"1.0.0", ""},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/package/npm/leftpad/"+tt.version, nil))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body)
			}
			var resp EnrichmentResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.Package == nil || resp.Package.Cooldown != "3d" {
				t.Errorf("package = %+v, want cooldown 3d", resp.Package)
			}
			if resp.Version == nil || resp.Version.AvailableAt != tt.availableAt {
				t.Errorf("version = %+v, want available_at %q", resp.Version, tt.availableAt)
			}
		})
	}
}

// fakeEnricher answers version lookups with the given publish times and
// knows no packages by name alone.
type fakeEnricher struct{ published map[string]time.Time }

func (fakeEnricher) EnrichPackage(context.Context, string, string) (*enrichment.PackageInfo, error) {
	return nil, nil
}

func (f fakeEnricher) EnrichFull(_ context.Context, ecosystem, name, version string) (*enrichment.EnrichmentResult, error) {
	return &enrichment.EnrichmentResult{
		Package: &enrichment.PackageInfo{Ecosystem: ecosystem, Name: name},
		Version: &enrichment.VersionInfo{Number: version, PublishedAt: f.published[version]},
	}, nil
}

func (fakeEnricher) CheckVulnerabilities(context.Context, string, string, string) ([]enrichment.VulnInfo, error) {
	return nil, nil
}

func (fakeEnricher) GetLatestVersion(context.Context, string, string) (string, error) {
	return "", nil
}

func (fakeEnricher) BulkEnrichPackages(context.Context, []struct{ Ecosystem, Name string }) map[string]*enrichment.PackageInfo {
	return nil
}
