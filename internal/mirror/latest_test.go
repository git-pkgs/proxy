package mirror

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestPURLSourceLatestMirrorsNewestVersion(t *testing.T) {
	versions := &fakeVersions{versions: []string{"1.0.0", "1.10.0", "1.2.0"}}
	source := &PURLSource{PURLs: []string{"pkg:npm/left-pad", "pkg:npm/is-odd@3.0.0"}, Versions: versions, Latest: true}

	items := enumerate(t, source)

	want := []PackageVersion{
		{Ecosystem: "npm", Name: "left-pad", Version: "1.10.0"},
		{Ecosystem: "npm", Name: "is-odd", Version: "3.0.0"},
	}
	if !reflect.DeepEqual(items, want) {
		t.Errorf("items = %v, want %v", items, want)
	}
}

func TestPURLSourceLatestWithoutVersions(t *testing.T) {
	source := &PURLSource{PURLs: []string{"pkg:npm/left-pad"}, Versions: &fakeVersions{}, Latest: true}
	if items := enumerate(t, source); len(items) != 0 {
		t.Errorf("items = %v, want none", items)
	}
}

func TestSBOMSourceLatest(t *testing.T) {
	bom := map[string]any{
		"bomFormat":   "CycloneDX",
		"specVersion": "1.4",
		"components":  []map[string]any{{"type": "library", "name": "left-pad", "purl": "pkg:npm/left-pad"}},
	}
	source := &SBOMSource{Data: marshalJSON(t, bom), Versions: &fakeVersions{versions: []string{"1.0.0", "1.3.0"}}, Latest: true}

	items := enumerate(t, source)

	if len(items) != 1 || items[0].Version != "1.3.0" {
		t.Errorf("items = %v, want only left-pad@1.3.0", items)
	}
}

// TestRegistryVersionsLatestSkipsYanked checks that without an advertised
// latest version the newest version by publish time that isn't yanked wins.
func TestRegistryVersionsLatestSkipsYanked(t *testing.T) {
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/crates/demo" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"crate": map[string]any{"name": "demo"},
			"versions": []map[string]any{
				{"num": "1.3.0", "yanked": true, "created_at": "2026-03-01T00:00:00Z"},
				{"num": "1.2.0", "created_at": "2026-02-01T00:00:00Z"},
				{"num": "1.10.0-alpha.1", "created_at": "2026-01-01T00:00:00Z"},
			},
		})
	}))
	defer registry.Close()
	versions := RegistryVersions{URL: func(string, string) (string, error) { return registry.URL, nil }}

	got, err := versions.LatestVersion(context.Background(), "cargo", "demo")
	if err != nil {
		t.Fatalf("LatestVersion() error = %v", err)
	}
	if got != "1.2.0" {
		t.Errorf("LatestVersion() = %q, want 1.2.0", got)
	}
}

// TestRegistryVersionsLatestPrefersAdvertised checks that the version the
// registry advertises as latest wins over a higher one, here a 4.0.0
// published by mistake and deprecated.
func TestRegistryVersionsLatestPrefersAdvertised(t *testing.T) {
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/left-pad" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name":      "left-pad",
			"dist-tags": map[string]any{"latest": "3.4.0"},
			"versions": map[string]any{
				"3.4.0": map[string]any{"name": "left-pad", "version": "3.4.0"},
				"4.0.0": map[string]any{"name": "left-pad", "version": "4.0.0", "deprecated": "published by mistake"},
			},
			"time": map[string]any{"3.4.0": "2026-01-01T00:00:00Z", "4.0.0": "2026-02-01T00:00:00Z"},
		})
	}))
	defer registry.Close()
	versions := RegistryVersions{URL: func(string, string) (string, error) { return registry.URL, nil }}

	got, err := versions.LatestVersion(context.Background(), "npm", "left-pad")
	if err != nil {
		t.Fatalf("LatestVersion() error = %v", err)
	}
	if got != "3.4.0" {
		t.Errorf("LatestVersion() = %q, want the advertised 3.4.0", got)
	}
}

func TestSourceFromRequestPassesLatest(t *testing.T) {
	js := NewJobStore(context.Background(), setupTestMirror(t, 1))

	source, err := js.sourceFromRequest(JobRequest{PURLs: []string{"pkg:npm/left-pad"}, Latest: true})
	if err != nil {
		t.Fatalf("sourceFromRequest() error = %v", err)
	}
	if !source.(*PURLSource).Latest {
		t.Error("PURL source mirrors every version, want only the newest")
	}

	source, err = js.sourceFromRequest(JobRequest{SBOM: json.RawMessage(`{"bomFormat":"CycloneDX","components":[]}`), Latest: true})
	if err != nil {
		t.Fatalf("sourceFromRequest() error = %v", err)
	}
	if !source.(*SBOMSource).Latest {
		t.Error("SBOM source mirrors every version, want only the newest")
	}
}
