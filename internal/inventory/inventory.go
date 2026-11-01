// Package inventory turns SBOMs uploaded for a project into the package
// versions the proxy records for it, named the way the proxy's handlers name
// the packages they cache, so the two can be joined.
package inventory

import (
	"errors"
	"fmt"
	"strings"

	"github.com/git-pkgs/proxy/internal/packageurl"
	"github.com/git-pkgs/purl"
	"github.com/git-pkgs/sbom"
	"github.com/git-pkgs/vers"
)

// MaxProjectNameLength bounds a project name, such as a GitLab project path.
const MaxProjectNameLength = 255

// Component is one package version a project uses.
type Component struct {
	// Ecosystem is the name the proxy's handlers store the package under,
	// e.g. "npm", "composer", "golang" or "alpine".
	Ecosystem string
	// Name is the ecosystem-native package name, e.g. "@babel/core",
	// "symfony/console" or "org.slf4j:slf4j-api".
	Name string
	// Version is the version as the SBOM gives it; it may be empty.
	Version string
	// PackagePURL is the unversioned package URL the proxy records the
	// package under, so it matches packages.purl.
	PackagePURL string
}

// servedTypes are the PURL types the proxy serves packages of and the
// inventory records.
var servedTypes = map[string]bool{
	"alpm": true, "apk": true, "bsd": true, "cargo": true, "chef": true,
	"composer": true, "conan": true, "conda": true, "cran": true, "deb": true,
	"gem": true, "golang": true, "hex": true, "julia": true, "maven": true,
	"npm": true, "nuget": true, "pub": true, "pypi": true, "rpm": true,
	"swift": true,
}

// ErrUnsupportedPURL is returned for a package URL whose type the proxy
// doesn't serve.
var ErrUnsupportedPURL = errors.New("package type is not served by the proxy")

// ErrNoCondaChannel is returned for a conda package URL without a channel
// qualifier: the proxy records conda packages per channel.
var ErrNoCondaChannel = errors.New("conda package URL has no channel qualifier")

// FromPURL maps a package URL to a component, with the package named the way
// the handler for its type records it. Qualifiers and subpath are dropped;
// the version is kept as given.
func FromPURL(s string) (Component, error) {
	p, err := purl.Parse(s)
	if err != nil {
		return Component{}, err
	}
	if !servedTypes[p.Type] {
		return Component{}, fmt.Errorf("%w: %s", ErrUnsupportedPURL, p.Type)
	}
	ecosystem, name := packageurl.HandlerPackage(p)
	if ecosystem == "conda" && !strings.Contains(name, "/") {
		return Component{}, ErrNoCondaChannel
	}
	packagePURL, _ := packageurl.MakeCacheStrings(ecosystem, name, "")
	if packagePURL == "" {
		return Component{}, fmt.Errorf("package URL %q has no valid package name", s)
	}
	return Component{Ecosystem: ecosystem, Name: name, Version: p.Version, PackagePURL: packagePURL}, nil
}

// syftMainModule is the property syft sets on modules found in a Go binary,
// naming the module the binary was built from.
const syftMainModule = "syft:metadata:mainModule"

// syftMetadataType is the property naming where syft read a package from.
const syftMetadataType = "syft:package:metadataType"

// unknownVersion is what syft gives a package without a resolved version,
// such as the project's own module in go.mod or the root of a lock file.
const unknownVersion = "UNKNOWN"

// Parse reads a CycloneDX (JSON or XML) or SPDX SBOM and returns the package
// versions it lists, without duplicates. Packages without a package URL, of a
// type the proxy doesn't serve, without a version, or conda packages without
// a channel are counted in skipped.
// So are the project's own packages: Go modules its binaries were built from,
// and the root of a lock file.
func Parse(data []byte) (components []Component, skipped int, err error) {
	doc, err := sbom.Parse(data)
	if err != nil {
		return nil, 0, err
	}
	ownModules := mainModules(doc.Packages)
	lockRoots := lockFileRoots(doc)
	seen := make(map[[2]string]bool, len(doc.Packages))
	for _, pkg := range doc.Packages {
		c, ok := componentOf(pkg)
		if !ok || lockRoots[pkg.ID] || (c.Ecosystem == "golang" && ownModules[c.Name]) {
			skipped++
			continue
		}
		key := [2]string{c.PackagePURL, c.Version}
		if seen[key] {
			continue
		}
		seen[key] = true
		components = append(components, c)
	}
	return components, skipped, nil
}

func componentOf(pkg sbom.Package) (Component, bool) {
	locator := pkg.PURL()
	if locator == "" {
		return Component{}, false
	}
	c, err := FromPURL(locator)
	if err != nil {
		return Component{}, false
	}
	if c.Version == "" {
		c.Version = pkg.Version
	}
	// A package without a version is the project itself (the module in
	// go.mod, the root of package-lock.json), not something it depends on.
	if c.Version == "" || c.Version == unknownVersion {
		return Component{}, false
	}
	return c, true
}

// lockFileRoots returns the IDs of packages read from a lock file that depend
// on others while nothing depends on them: the project's own entry, such as
// the root of package-lock.json, which carries the version from package.json.
func lockFileRoots(doc *sbom.SBOM) map[string]bool {
	dependents := map[string]bool{}
	dependencies := map[string]bool{}
	for _, r := range doc.Relationships {
		if r.Type != sbom.RelDependsOn {
			continue
		}
		dependencies[r.SourceID] = true
		dependents[r.TargetID] = true
	}
	roots := map[string]bool{}
	for _, pkg := range doc.Packages {
		if pkg.ID == "" || !dependencies[pkg.ID] || dependents[pkg.ID] {
			continue
		}
		for _, prop := range pkg.Properties {
			if prop.Name == syftMetadataType && strings.Contains(prop.Value, "lock") {
				roots[pkg.ID] = true
			}
		}
	}
	return roots
}

// mainModules returns the Go modules the SBOM's binaries were built from,
// which are the project's own. Module paths are case-sensitive and package
// URLs keep their case, so they are compared as they are.
func mainModules(pkgs []sbom.Package) map[string]bool {
	own := map[string]bool{}
	for _, pkg := range pkgs {
		for _, prop := range pkg.Properties {
			if prop.Name == syftMainModule && prop.Value != "" {
				own[prop.Value] = true
			}
		}
	}
	return own
}

// ValidateProjectName checks a project name such as "group/sub/repo": one or
// more segments of letters, digits, ".", "_" and "-", none of them "." or
// "..".
func ValidateProjectName(name string) error {
	if name == "" {
		return errors.New("project name is empty")
	}
	if len(name) > MaxProjectNameLength {
		return fmt.Errorf("project name is longer than %d characters", MaxProjectNameLength)
	}
	for segment := range strings.SplitSeq(name, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("project name %q has an empty, \".\" or \"..\" segment", name)
		}
		for _, r := range segment {
			if !validNameRune(r) {
				return fmt.Errorf("project name %q may only contain letters, digits, '.', '_', '-' and '/'", name)
			}
		}
	}
	return nil
}

func validNameRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-'
}

// CompareVersions compares two versions of a package in ecosystem with the
// ecosystem's version rules, returning -1, 0 or 1.
func CompareVersions(ecosystem, a, b string) int {
	return vers.CompareWithScheme(a, b, purl.EcosystemToPURLType(ecosystem))
}

// Outdated reports whether version is lower than latest.
func Outdated(ecosystem, version, latest string) bool {
	if version == "" || latest == "" || version == latest {
		return false
	}
	return CompareVersions(ecosystem, version, latest) < 0
}
