package server

import (
	"strings"

	"github.com/git-pkgs/proxy/internal/database"
)

// resolvePackageName determines the package name from a wildcard path by
// checking the database. This handles namespaced packages like Composer's
// vendor/name format where the package name contains a slash.
//
// It tries the full path as a package name first. If not found, it splits
// off the last segment as a non-name suffix (version, action, etc.) and
// tries again, working backwards until a match is found or segments run out.
//
// Returns the package name and the remaining path segments after the name.
// If no package is found, returns empty name and the original segments.
func resolvePackageName(db *database.DB, ecosystem string, segments []string) (name string, rest []string) {
	// Try increasingly longer prefixes as the package name.
	// Start with the longest possible name (all segments) and work down.
	for i := len(segments); i >= 1; i-- {
		candidate := strings.Join(segments[:i], "/")
		pkg, err := db.GetPackageByEcosystemName(ecosystem, candidate)
		if err == nil && pkg != nil {
			return candidate, segments[i:]
		}
	}

	return "", segments
}

// splitWildcardPath splits a chi wildcard path value into segments,
// trimming any leading/trailing slashes.
func splitWildcardPath(path string) []string {
	path = strings.Trim(path, "/")
	if path == "" {
		return nil
	}
	return strings.Split(path, "/")
}
