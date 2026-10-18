package mirror

import (
	"context"
	"reflect"
	"testing"
)

func TestSBOMSourcePreservesPackageIdentities(t *testing.T) {
	for name, data := range map[string]string{
		"CycloneDX JSON": `{"components":[
			{"name":"shared","version":"1.0","purl":"pkg:npm/%40alpha/shared@1.0"},
			{"name":"shared","version":"1.0","purl":"pkg:npm/%40beta/shared@1.0"}]}`,
		"CycloneDX XML": `<bom xmlns="http://cyclonedx.org/schema/bom/1.6"><components>
			<component type="library"><name>shared</name><version>1.0</version><purl>pkg:npm/%40alpha/shared@1.0</purl></component>
			<component type="library"><name>shared</name><version>1.0</version><purl>pkg:npm/%40beta/shared@1.0</purl></component>
			</components></bom>`,
		"SPDX JSON": `{"spdxVersion":"SPDX-2.3","packages":[{"name":"shared","versionInfo":"1.0","externalRefs":[
			{"referenceType":"purl","referenceLocator":"pkg:npm/%40alpha/shared@1.0"},
			{"referenceType":"purl","referenceLocator":"pkg:npm/%40beta/shared@1.0"}]}]}`,
		"SPDX tag-value": `DataLicense: CC0-1.0
SPDXVersion: SPDX-2.3
SPDXID: SPDXRef-DOCUMENT
DocumentName: example
DocumentNamespace: https://example.com/sbom
PackageName: shared
SPDXID: SPDXRef-shared
PackageVersion: 1.0
ExternalRef: PACKAGE-MANAGER purl pkg:npm/%40alpha/shared@1.0
ExternalRef: PACKAGE-MANAGER purl pkg:npm/%40beta/shared@1.0
`,
	} {
		t.Run(name, func(t *testing.T) {
			source := &SBOMSource{Data: []byte(data), Name: name}
			var got []PackageVersion
			err := source.Enumerate(context.Background(), func(pv PackageVersion) error {
				got = append(got, pv)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			want := []PackageVersion{
				{Ecosystem: "npm", Name: "@alpha/shared", Version: "1.0"},
				{Ecosystem: "npm", Name: "@beta/shared", Version: "1.0"},
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Enumerate = %+v, want %+v", got, want)
			}
		})
	}
}

func TestSBOMSourcePreservesImageOptions(t *testing.T) {
	source := &SBOMSource{
		Data:      []byte(`{"bomFormat":"CycloneDX","components":[{"name":"alpine","purl":"pkg:docker/library/alpine@3.20?repository_url=ghcr.io&arch=arm64"}]}`),
		Platforms: []string{"linux/amd64"},
	}
	var got []PackageVersion
	if err := source.Enumerate(context.Background(), func(pv PackageVersion) error {
		got = append(got, pv)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []PackageVersion{{
		Ecosystem: "docker", Name: "library/alpine", Version: "3.20",
		Qualifiers: map[string]string{"repository_url": "ghcr.io", "arch": "arm64"},
		Platforms:  []string{"linux/amd64"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Enumerate = %+v, want %+v", got, want)
	}
}
