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

// PolicyPackageKey maps a canonical versionless PURL to its policy key.
// Go policies match module names case-insensitively for compatibility.
func PolicyPackageKey(packagePURL string) string {
	if strings.HasPrefix(packagePURL, "pkg:golang/") {
		return strings.ToLower(packagePURL)
	}
	return packagePURL
}

// WithVersionString returns a package PURL with its version replaced. It
// returns an empty string when packagePURL is invalid.
func WithVersionString(packagePURL, version string) string {
	pkg, err := purl.Parse(packagePURL)
	if err != nil {
		return ""
	}
	return pkg.WithVersion(version).String()
}

// MakeCacheStrings returns package and version PURLs suitable for artifact
// cache records. Swift registry identities keep their generic storage PURLs.
// Cache keys are independent of the configured upstream.
func MakeCacheStrings(ecosystem, name, version string) (packagePURL, versionPURL string) {
	if purl.NormalizeEcosystem(ecosystem) == "swift" {
		if identity, ok := swiftRegistryIdentity(name); ok {
			pkg := purl.New("generic", "swift-registry", identity, "", nil)
			return pkg.String(), pkg.WithVersion(version).String()
		}
	}
	if pkg := Make(ecosystem, name, ""); pkg != nil {
		return pkg.String(), pkg.WithVersion(version).String()
	}
	return "", ""
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
