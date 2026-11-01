package inventory

import (
	"os"
	"testing"
)

// FuzzParse checks that no uploaded body makes the SBOM parsing panic.
func FuzzParse(f *testing.F) {
	if data, err := os.ReadFile("testdata/syft-cyclonedx.json"); err == nil {
		f.Add(data)
	}
	f.Add([]byte(`{"bomFormat":"CycloneDX","specVersion":"1.5","components":[{"type":"library","name":"x","version":"1","purl":"pkg:npm/x@1"}]}`))
	f.Add([]byte(`{"spdxVersion":"SPDX-2.3","SPDXID":"SPDXRef-DOCUMENT","packages":[{"SPDXID":"SPDXRef-a","name":"a","versionInfo":"1","externalRefs":[{"referenceCategory":"PACKAGE-MANAGER","referenceType":"purl","referenceLocator":"pkg:conda/a@1?channel=c"}]}]}`))
	f.Add([]byte("SPDXVersion: SPDX-2.3\nPackageName: a\nSPDXID: SPDXRef-a\nExternalRef: PACKAGE-MANAGER purl pkg:deb/debian/a@1\n"))
	f.Add([]byte(`<bom xmlns="http://cyclonedx.org/schema/bom/1.5"><components><component type="library"><name>x</name><purl>pkg:pypi/X_Y@1</purl></component></components></bom>`))
	f.Fuzz(func(t *testing.T, data []byte) {
		components, _, err := Parse(data)
		if err != nil {
			return
		}
		for _, c := range components {
			if c.PackagePURL == "" || c.Version == "" || c.Ecosystem == "" {
				t.Errorf("component with an empty field: %+v", c)
			}
		}
	})
}
