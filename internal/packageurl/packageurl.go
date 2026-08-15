// Package packageurl builds package URLs from ecosystem-native package names.
package packageurl

import (
	"strings"

	"github.com/git-pkgs/purl"
)

// Make constructs a package URL, including the namespace required by Swift
// registry package identifiers such as apple/swift-argument-parser.
func Make(ecosystem, name, version string) *purl.PURL {
	if purl.NormalizeEcosystem(ecosystem) == "swift" {
		if split := strings.LastIndexByte(name, '/'); split > 0 && split < len(name)-1 {
			return purl.New("swift", name[:split], name[split+1:], version, nil)
		}
	}

	return purl.MakePURL(ecosystem, name, version)
}

// MakeString constructs a package URL string.
func MakeString(ecosystem, name, version string) string {
	return Make(ecosystem, name, version).String()
}
