// Package packageurl builds package URLs from ecosystem-native package names.
package packageurl

import (
	"strings"

	"github.com/git-pkgs/purl"
)

// Make constructs a package URL from an ecosystem-native package name.
func Make(ecosystem, name, version string) *purl.PURL {
	return purl.MakePURL(ecosystem, name, version)
}

// MakeString constructs a package URL string. It returns an empty string when
// the package identity cannot be represented as a PURL.
func MakeString(ecosystem, name, version string) string {
	return purl.MakePURLString(ecosystem, name, version)
}

// MakeCacheStrings returns package and version PURLs suitable for artifact
// cache records. Swift registry identities use an explicit generic PURL until
// their source repository has been resolved.
func MakeCacheStrings(ecosystem, name, version, registryURL string) (packagePURL, versionPURL string) {
	if pkg := Make(ecosystem, name, ""); pkg != nil {
		return pkg.String(), pkg.WithVersion(version).String()
	}
	if purl.NormalizeEcosystem(ecosystem) != "swift" {
		return "", ""
	}

	identity, ok := swiftRegistryIdentity(name)
	if !ok {
		return "", ""
	}

	var qualifiers map[string]string
	if registryURL != "" {
		qualifiers = map[string]string{"repository_url": strings.TrimRight(registryURL, "/")}
	}
	pkg := purl.New("generic", "swift-registry", identity, "", qualifiers)
	return pkg.String(), pkg.WithVersion(version).String()
}

func swiftRegistryIdentity(name string) (string, bool) {
	scope, packageName, found := strings.Cut(name, "/")
	if !found {
		scope, packageName, found = strings.Cut(name, ".")
	}
	if !found || scope == "" || packageName == "" || strings.ContainsAny(packageName, "/.") {
		return "", false
	}
	return strings.ToLower(scope) + "." + strings.ToLower(packageName), true
}
