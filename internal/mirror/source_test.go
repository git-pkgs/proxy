package mirror

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPURLSourceVersioned(t *testing.T) {
	source := &PURLSource{
		PURLs: []string{
			"pkg:npm/lodash@4.17.21",
			"pkg:cargo/serde@1.0.0",
			"pkg:pypi/requests@2.31.0",
		},
	}

	var items []PackageVersion
	err := source.Enumerate(context.Background(), func(pv PackageVersion) error {
		items = append(items, pv)
		return nil
	})
	if err != nil {
		t.Fatalf("Enumerate() error = %v", err)
	}

	if len(items) != 3 {
		t.Fatalf("got %d items, want 3", len(items))
	}

	expected := []PackageVersion{
		{Ecosystem: "npm", Name: "lodash", Version: "4.17.21"},
		{Ecosystem: "cargo", Name: "serde", Version: "1.0.0"},
		{Ecosystem: "pypi", Name: "requests", Version: "2.31.0"},
	}

	for i, want := range expected {
		got := items[i]
		if got.Ecosystem != want.Ecosystem || got.Name != want.Name || got.Version != want.Version {
			t.Errorf("items[%d] = %v, want %v", i, got, want)
		}
	}
}

func TestPURLSourceScopedPackage(t *testing.T) {
	source := &PURLSource{
		PURLs: []string{"pkg:npm/%40babel/core@7.23.0"},
	}

	var items []PackageVersion
	err := source.Enumerate(context.Background(), func(pv PackageVersion) error {
		items = append(items, pv)
		return nil
	})
	if err != nil {
		t.Fatalf("Enumerate() error = %v", err)
	}

	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}

	if items[0].Name != "@babel/core" {
		t.Errorf("name = %q, want %q", items[0].Name, "@babel/core")
	}
	if items[0].Version != "7.23.0" {
		t.Errorf("version = %q, want %q", items[0].Version, "7.23.0")
	}
}

func TestPURLSourceInvalid(t *testing.T) {
	source := &PURLSource{
		PURLs: []string{"not-a-purl"},
	}

	err := source.Enumerate(context.Background(), func(pv PackageVersion) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected error for invalid PURL")
	}
}

func TestPURLSourceCallbackError(t *testing.T) {
	source := &PURLSource{
		PURLs: []string{"pkg:npm/lodash@4.17.21"},
	}

	wantErr := context.Canceled
	err := source.Enumerate(context.Background(), func(pv PackageVersion) error {
		return wantErr
	})
	if err != wantErr {
		t.Fatalf("got error %v, want %v", err, wantErr)
	}
}

func TestPackageVersionString(t *testing.T) {
	pv := PackageVersion{Ecosystem: "npm", Name: "lodash", Version: "4.17.21"}
	got := pv.String()
	want := "pkg:npm/lodash@4.17.21"
	if got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestSBOMSourceCycloneDXJSON(t *testing.T) {
	bom := map[string]any{
		"bomFormat":   "CycloneDX",
		"specVersion": "1.4",
		"components": []map[string]any{
			{"type": "library", "name": "lodash", "version": "4.17.21", "purl": "pkg:npm/lodash@4.17.21"},
			{"type": "library", "name": "serde", "version": "1.0.0", "purl": "pkg:cargo/serde@1.0.0"},
		},
	}

	path := writeTempJSON(t, bom)
	source := &SBOMSource{Path: path}

	var items []PackageVersion
	err := source.Enumerate(context.Background(), func(pv PackageVersion) error {
		items = append(items, pv)
		return nil
	})
	if err != nil {
		t.Fatalf("Enumerate() error = %v", err)
	}

	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}

	if items[0].Ecosystem != "npm" || items[0].Name != "lodash" || items[0].Version != "4.17.21" {
		t.Errorf("items[0] = %v", items[0])
	}
	if items[1].Ecosystem != "cargo" || items[1].Name != "serde" || items[1].Version != "1.0.0" {
		t.Errorf("items[1] = %v", items[1])
	}
}

func TestSBOMSourceSPDXJSON(t *testing.T) {
	doc := map[string]any{
		"spdxVersion":       "SPDX-2.3",
		"dataLicense":       "CC0-1.0",
		"SPDXID":            "SPDXRef-DOCUMENT",
		"name":              "test",
		"documentNamespace": "https://example.com/test",
		"packages": []map[string]any{
			{
				"SPDXID":           "SPDXRef-Package",
				"name":             "lodash",
				"version":          "4.17.21",
				"downloadLocation": "https://registry.npmjs.org/lodash/-/lodash-4.17.21.tgz",
				"externalRefs": []map[string]any{
					{
						"referenceCategory": "PACKAGE-MANAGER",
						"referenceType":     "purl",
						"referenceLocator":  "pkg:npm/lodash@4.17.21",
					},
				},
			},
		},
	}

	path := writeTempJSON(t, doc)
	source := &SBOMSource{Path: path}

	var items []PackageVersion
	err := source.Enumerate(context.Background(), func(pv PackageVersion) error {
		items = append(items, pv)
		return nil
	})
	if err != nil {
		t.Fatalf("Enumerate() error = %v", err)
	}

	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}

	if items[0].Name != "lodash" || items[0].Version != "4.17.21" {
		t.Errorf("items[0] = %v", items[0])
	}
}

func TestSBOMSourceNonexistentFile(t *testing.T) {
	source := &SBOMSource{Path: "/nonexistent/sbom.json"}

	err := source.Enumerate(context.Background(), func(pv PackageVersion) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestSBOMSourceInvalidFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid.txt")
	if err := os.WriteFile(path, []byte("this is not an SBOM"), 0644); err != nil {
		t.Fatal(err)
	}

	source := &SBOMSource{Path: path}
	err := source.Enumerate(context.Background(), func(pv PackageVersion) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected error for invalid SBOM")
	}
}

func TestSBOMSourceEmptyCycloneDX(t *testing.T) {
	bom := map[string]any{
		"bomFormat":   "CycloneDX",
		"specVersion": "1.4",
	}
	path := writeTempJSON(t, bom)

	// This should fall through to SPDX parsing, which will also fail,
	// resulting in an error about not being able to parse
	source := &SBOMSource{Path: path}
	err := source.Enumerate(context.Background(), func(pv PackageVersion) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected error for empty SBOM")
	}
}

func writeTempJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "sbom.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	return path
}
