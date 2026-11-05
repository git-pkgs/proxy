package packageurl

import (
	"testing"

	"github.com/git-pkgs/purl"
)

// HandlerPackage is used by both the mirror and the project inventory, so a
// mirrored package, a package listed in an SBOM and the same package fetched
// by a client end up under one name.
func TestHandlerPackage(t *testing.T) {
	tests := []struct {
		purl      string
		ecosystem string
		name      string
	}{
		// purl names these after their registries; the handlers use the type.
		{"pkg:gem/rake@13.2.1", "gem", "rake"},
		{"pkg:composer/psr/log@3.0.2", "composer", "psr/log"},
		// Distribution namespaces are dropped.
		{"pkg:apk/alpine/busybox@1.36.1-r15?arch=x86_64", "alpine", "busybox"},
		{"pkg:apk/curl@8.14.1-r2", "alpine", "curl"},
		{"pkg:alpm/arch/glibc@2.39-1", "arch", "glibc"},
		{"pkg:bsd/freebsd/curl@8.7.1", "freebsd", "curl"},
		{"pkg:deb/debian/libc6@2.36-9?arch=amd64", "deb", "libc6"},
		{"pkg:rpm/fedora/curl@8.6.0-7.fc40", "rpm", "curl"},
		// The conda channel is part of the name.
		{"pkg:conda/numpy@1.26.4?channel=conda-forge", "conda", "conda-forge/numpy"},
		{"pkg:conda/conda-forge/numpy@1.26.4", "conda", "conda-forge/numpy"},
		{"pkg:conda/numpy@1.26.4", "conda", "numpy"},
		// PyPI names are normalized as PEP 503 does, NuGet IDs lowercased.
		{"pkg:pypi/Zope.Interface@6.4", "pypi", "zope-interface"},
		{"pkg:pypi/zope_interface", "pypi", "zope-interface"},
		{"pkg:nuget/Newtonsoft.Json@13.0.3", "nuget", "newtonsoft.json"},
		// Other namespaces stay part of the name.
		{"pkg:npm/%40babel/core@7.23.0", "npm", "@babel/core"},
		// The Maven handler stores "group:artifact".
		{"pkg:maven/org.slf4j/slf4j-api@2.0.13", "maven", "org.slf4j:slf4j-api"},
		{"pkg:docker/library/alpine@3.20", "docker", "library/alpine"},
		{"pkg:julia/Example@0.5.3?uuid=7876af07-990d-54b4-ab0e-23690620f79a", "julia", "Example"},
		// Swift keeps namespace/name, the scope or the source repository,
		// which the Swift mirror splits again; purl's full name would be the
		// registry identity "scope.name".
		{"pkg:swift/Acme/Widget@1.0.0", "swift", "Acme/Widget"},
		{"pkg:swift/github.com/apple/swift-nio@2.62.0", "swift", "github.com/apple/swift-nio"},
		// Go module paths keep their case.
		{"pkg:golang/github.com/Azure/foo@v1.2.0", "golang", "github.com/Azure/foo"},
	}
	for _, tt := range tests {
		t.Run(tt.purl, func(t *testing.T) {
			p, err := purl.Parse(tt.purl)
			if err != nil {
				t.Fatal(err)
			}
			ecosystem, name := HandlerPackage(p)
			if ecosystem != tt.ecosystem || name != tt.name {
				t.Errorf("HandlerPackage(%q) = %q, %q, want %q, %q", tt.purl, ecosystem, name, tt.ecosystem, tt.name)
			}
		})
	}
}
