package mirror

import (
	"strconv"

	"github.com/git-pkgs/purl"
	"github.com/git-pkgs/registries"
	"github.com/git-pkgs/vers"
)

// newestPerCachedLine returns, for each major line a cached version is on,
// the newest release of that line that allowed accepts, when it is newer
// than every cached version of the line. A line is the major version, or
// 0.minor below 1.0, as caret ranges read them. Prereleases and yanked,
// retracted or deprecated releases are skipped.
func newestPerCachedLine(ecosystem string, cached []string, releases []registries.Version, allowed func(registries.Version) bool) []string {
	scheme := purl.EcosystemToPURLType(ecosystem)
	newestCached := map[string]string{}
	var lines []string
	for _, v := range cached {
		line, ok := versionLine(v)
		if !ok {
			continue
		}
		current, seen := newestCached[line]
		if !seen {
			lines = append(lines, line)
		}
		if !seen || vers.CompareWithScheme(v, current, scheme) > 0 {
			newestCached[line] = v
		}
	}

	newest := map[string]string{}
	for _, r := range releases {
		line, ok := versionLine(r.Number)
		if !ok || r.Status != registries.StatusNone || vers.IsPrereleaseWithScheme(r.Number, scheme) {
			continue
		}
		if _, cachedLine := newestCached[line]; !cachedLine || !allowed(r) {
			continue
		}
		if current, seen := newest[line]; !seen || vers.CompareWithScheme(r.Number, current, scheme) > 0 {
			newest[line] = r.Number
		}
	}

	var out []string
	for _, line := range lines {
		if v, ok := newest[line]; ok && vers.CompareWithScheme(v, newestCached[line], scheme) > 0 {
			out = append(out, v)
		}
	}
	return out
}

// versionLine returns the major line of a version.
func versionLine(v string) (string, bool) {
	info, err := vers.ParseVersion(v)
	if err != nil {
		return "", false
	}
	if info.Major == 0 {
		return "0." + strconv.Itoa(info.Minor), true
	}
	return strconv.Itoa(info.Major), true
}
