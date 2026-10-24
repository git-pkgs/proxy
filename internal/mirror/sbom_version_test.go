package mirror

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/registries"
)

func TestSBOMSourceUsesComponentVersion(t *testing.T) {
	for name, data := range map[string]string{
		"CycloneDX JSON": `{"components":[{"name":"app","version":"1.0","purl":"pkg:docker/ghcr.io/team/app"}]}`,
		"CycloneDX XML":  `<bom xmlns="http://cyclonedx.org/schema/bom/1.6"><components><component type="container"><name>app</name><version>1.0</version><purl>pkg:docker/ghcr.io/team/app</purl></component></components></bom>`,
		"SPDX JSON":      `{"spdxVersion":"SPDX-2.3","packages":[{"name":"app","versionInfo":"1.0","externalRefs":[{"referenceType":"purl","referenceLocator":"pkg:docker/ghcr.io/team/app"}]}]}`,
		"SPDX tag-value": "DataLicense: CC0-1.0\nSPDXVersion: SPDX-2.3\nSPDXID: SPDXRef-DOCUMENT\nDocumentName: example\nDocumentNamespace: https://example.com/sbom\nPackageName: app\nSPDXID: SPDXRef-app\nPackageVersion: 1.0\nExternalRef: PACKAGE-MANAGER purl pkg:docker/ghcr.io/team/app\n",
	} {
		t.Run(name, func(t *testing.T) {
			source := &SBOMSource{Data: []byte(data)}
			var got []PackageVersion
			if err := source.Enumerate(context.Background(), func(pv PackageVersion) error { got = append(got, pv); return nil }); err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].Version != "1.0" {
				t.Fatalf("Enumerate = %+v, want one image at 1.0", got)
			}
		})
	}
}

func TestSBOMSourceVersionPrecedence(t *testing.T) {
	for _, test := range []struct{ name, componentVersion, imagePURL, wantVersion string }{
		{"PURL version wins", "2.0", "pkg:docker/team/app@1.0", "1.0"},
		{"digest from component", "sha256:abc123", "pkg:docker/team/app", "sha256:abc123"},
		{"unknown version", "", "pkg:docker/team/app", ""},
		{"package version", "1.0", "pkg:npm/example", "1.0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := marshalJSON(t, map[string]any{"components": []map[string]any{{"name": "app", "version": test.componentVersion, "purl": test.imagePURL}}})
			var got []PackageVersion
			source := &SBOMSource{Data: data, Versions: RegistryVersions{Client: registries.NewClient(registries.WithHTTPClient(&http.Client{Transport: rejectingMirrorTransport{}}))}}
			if err := source.Enumerate(context.Background(), func(pv PackageVersion) error { got = append(got, pv); return nil }); err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].Version != test.wantVersion {
				t.Fatalf("Enumerate = %+v, want version %q", got, test.wantVersion)
			}
		})
	}
}

func TestMirrorRunSBOMDockerReference(t *testing.T) {
	m := setupTestMirror(t, 1)
	images := &fakeImages{result: handler.ImageResult{Fetched: 1}}
	m.Images = images
	source := &SBOMSource{
		Data:      []byte(`{"components":[{"name":"app","version":"1.0","purl":"pkg:docker/registry.example:5000/team/app?arch=arm64"}]}`),
		Platforms: []string{"linux/arm64"},
	}
	progress, err := m.Run(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if progress.Completed != 1 || progress.Failed != 0 {
		t.Fatalf("progress = %+v", progress)
	}
	want := []handler.ImageRef{{Registry: "registry.example:5000", Name: "team/app", References: []string{"1.0"}}}
	if !reflect.DeepEqual(images.refs, want) {
		t.Errorf("image refs = %+v, want %+v", images.refs, want)
	}
}
