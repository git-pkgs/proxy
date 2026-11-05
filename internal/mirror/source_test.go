package mirror

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
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

	source := &SBOMSource{Data: marshalJSON(t, bom)}

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

	source := &SBOMSource{Data: marshalJSON(t, doc)}

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

func TestSBOMSourceEmptyData(t *testing.T) {
	source := &SBOMSource{}

	err := source.Enumerate(context.Background(), func(pv PackageVersion) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected error for empty SBOM")
	}
}

func TestSBOMSourceInvalidFormat(t *testing.T) {
	source := &SBOMSource{Data: []byte("this is not an SBOM"), Name: "invalid.sbom"}
	err := source.Enumerate(context.Background(), func(pv PackageVersion) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected error for invalid SBOM")
	}
	if !strings.Contains(err.Error(), "invalid.sbom") {
		t.Errorf("error = %q, want source name", err)
	}
}

func TestSBOMSourceEmptyCycloneDX(t *testing.T) {
	bom := map[string]any{
		"bomFormat":   "CycloneDX",
		"specVersion": "1.4",
	}
	source := &SBOMSource{Data: marshalJSON(t, bom)}
	err := source.Enumerate(context.Background(), func(pv PackageVersion) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected error for empty SBOM")
	}
}

func marshalJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// The mirror files packages under the ecosystem and package names the
// handlers use, so a mirrored package and the same package cached by a client
// share one name.
func TestPURLSourceUsesHandlerEcosystemNames(t *testing.T) {
	source := &PURLSource{PURLs: []string{
		"pkg:gem/rake@13.2.1",
		"pkg:composer/psr/log@3.0.2",
		"pkg:apk/alpine/curl@8.14.1-r2",
		"pkg:npm/left-pad@1.3.0",
		"pkg:maven/org.slf4j/slf4j-api@2.0.13",
		"pkg:pypi/Zope.Interface@6.4",
		"pkg:nuget/Newtonsoft.Json@13.0.3",
	}}

	var got []string
	if err := source.Enumerate(context.Background(), func(pv PackageVersion) error {
		got = append(got, pv.Ecosystem+" "+pv.Name)
		return nil
	}); err != nil {
		t.Fatalf("Enumerate() error = %v", err)
	}

	want := []string{
		"gem rake",
		"composer psr/log",
		"alpine curl",
		"npm left-pad",
		"maven org.slf4j:slf4j-api",
		"pypi zope-interface",
		"nuget newtonsoft.json",
	}
	if !slices.Equal(got, want) {
		t.Errorf("packages = %q, want %q", got, want)
	}
}
