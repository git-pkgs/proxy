// Package conda transforms conda channel metadata, repodata.json and
// sharded repodata (CEP 16), without storage or network access.
package conda

import (
	"strings"
	"time"

	"github.com/git-pkgs/cooldown"
)

const (
	ecosystem = "conda"
	// minFilenameParts: name-version-build needs at least three
	// hyphen-separated parts.
	minFilenameParts = 3
	// MaxSecondsTimestamp is the last second of year 9999. Like conda, a
	// larger repodata timestamp is read as milliseconds and a smaller one
	// as seconds, which some older entries use.
	MaxSecondsTimestamp = 253402300799
)

// CooldownPolicy evaluates version age at the caller's supplied time.
type CooldownPolicy interface {
	Enabled() bool
	Evaluate(ecosystem, packagePURL string, publishedAt, evaluatedAt time.Time) cooldown.Decision
}

// Options supplies the policy for a filter. EvaluatedAt is used as given;
// the transformations never read the clock.
type Options struct {
	Cooldown    CooldownPolicy
	EvaluatedAt time.Time
	// OnCooldownFilter is called synchronously for each package file
	// cooldown leaves out.
	OnCooldownFilter func(filename, name, version string)
}

// Time returns the time a repodata timestamp names.
func Time(timestamp float64) time.Time {
	if timestamp > MaxSecondsTimestamp {
		return time.UnixMilli(int64(timestamp)).Truncate(time.Second)
	}
	return time.Unix(int64(timestamp), 0)
}

// FilenameIdentity returns the name and version a download of filename is
// cached under, or empty strings when it is not a package file name. Conda
// filenames are {name}-{version}-{build}.{ext}, such as
// numpy-1.24.0-py311h64a7726_0.conda; the name can contain hyphens.
func FilenameIdentity(filename string) (name, version string) {
	base := filename
	for _, ext := range []string{".conda", ".tar.bz2"} {
		if strings.HasSuffix(base, ext) {
			base = strings.TrimSuffix(base, ext)
			break
		}
	}
	parts := strings.Split(base, "-")
	if len(parts) < minFilenameParts {
		return "", ""
	}
	version = parts[len(parts)-2]
	name = strings.Join(parts[:len(parts)-2], "-")
	if len(version) == 0 || version[0] < '0' || version[0] > '9' {
		return "", ""
	}
	return name, version
}
