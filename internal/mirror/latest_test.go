package mirror

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestNewestVersion(t *testing.T) {
	tests := []struct {
		ecosystem string
		versions  []string
		want      string
	}{
		{"npm", []string{"1.9.0", "1.10.0", "2.0.0-rc.1", "1.2.3"}, "1.10.0"},
		{"npm", []string{"2.0.0-beta.1", "2.0.0-beta.2"}, "2.0.0-beta.2"},
		{"pypi", []string{"1.9", "1.10", "2.0rc1", "2.0.dev3"}, "1.10"},
		{"gem", []string{"7.1.0", "7.2.0.beta1", "7.0.8"}, "7.1.0"},
		{"golang", []string{"v1.2.0", "v1.10.0", "v2.0.0-beta.1"}, "v1.10.0"},
		{"maven", []string{"1.0", "1.10", "1.9", "2.0-alpha-1"}, "1.10"},
		{"npm", nil, ""},
	}
	for _, tt := range tests {
		if got := newestVersion(tt.ecosystem, tt.versions); got != tt.want {
			t.Errorf("newestVersion(%s, %v) = %q, want %q", tt.ecosystem, tt.versions, got, tt.want)
		}
	}
}

func TestPURLSourceLatestMirrorsNewestVersion(t *testing.T) {
	versions := &fakeVersions{versions: []string{"1.0.0", "1.2.0", "1.1.0", "2.0.0-rc.1"}}
	source := &PURLSource{PURLs: []string{"pkg:npm/left-pad", "pkg:npm/is-odd@3.0.0"}, Versions: versions, Latest: true}

	items := enumerate(t, source)

	want := []PackageVersion{
		{Ecosystem: "npm", Name: "left-pad", Version: "1.2.0"},
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

func TestRegistryVersionsLatestSkipsYanked(t *testing.T) {
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/crates/demo" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"crate": map[string]any{"name": "demo"},
			"versions": []map[string]any{
				{"num": "1.3.0", "yanked": true},
				{"num": "1.2.0"},
				{"num": "1.10.0-alpha.1"},
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
