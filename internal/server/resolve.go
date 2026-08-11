package server

import (
	"fmt"
	"net/url"
	"strings"
	"unicode"

	"github.com/git-pkgs/proxy/internal/database"
)

// maxPackagePathLen bounds the wildcard portion of package routes (name plus
// version and any suffix). npm caps names at 214 and Maven coordinates can be
// longer, so 512 leaves room without admitting pathological inputs.
const maxPackagePathLen = 512

// validatePackagePath rejects wildcard package paths that cannot be valid in
// any supported ecosystem. It is a coarse filter applied before database or
// enrichment lookups; ecosystem-specific name rules are layered on top.
func validatePackagePath(path string) error {
	if path == "" {
		return fmt.Errorf("package name required")
	}
	if len(path) > maxPackagePathLen {
		return fmt.Errorf("package path exceeds %d bytes", maxPackagePathLen)
	}
	// Validate the decoded segments: the handlers work with decoded values, so
	// an escape such as "%00" or "%2E%2E" must not slip past these checks.
	for _, seg := range splitWildcardPath(path) {
		// A decoded segment can itself contain slashes (from "%2F"), and the
		// segments are later rejoined into a package name that registries
		// interpolate straight into an upstream URL. Check every path element,
		// not just the segment as a whole, or "a%2F..%2F..%2Fb" traverses.
		for _, elem := range strings.Split(seg, "/") {
			if elem == ".." {
				return fmt.Errorf("package path contains parent directory segment")
			}
		}
		for _, r := range seg {
			if r == 0 {
				return fmt.Errorf("package path contains null byte")
			}
			if unicode.IsControl(r) {
				return fmt.Errorf("package path contains control character %#U", r)
			}
		}
	}
	return nil
}

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
//
// chi routes on the raw (still percent-encoded) path whenever the request URL
// contains an escape, so each segment is decoded after splitting. Splitting
// first keeps an encoded "%2F" inside a name from being mistaken for a
// separator. Decoding matters for versions such as "1.0%2Bbuild1", which must
// reach the handlers as "1.0+build1" so that rebuilding the PURL yields the
// value that was stored rather than a double-encoded one.
func splitWildcardPath(path string) []string {
	path = strings.Trim(path, "/")
	if path == "" {
		return nil
	}
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		segments[i] = decodePathSegment(seg)
	}
	return segments
}

// decodePathSegment percent-decodes a single URL path segment, returning it
// unchanged if it is not valid percent-encoding.
func decodePathSegment(seg string) string {
	if !strings.Contains(seg, "%") {
		return seg
	}
	decoded, err := url.PathUnescape(seg)
	if err != nil {
		return seg
	}
	return decoded
}
