package gem

import (
	"regexp"
	"strings"
)

// maxFilenameLength bounds the gem filenames FilenameIdentity accepts.
// Real gem filenames are a small fraction of this.
const maxFilenameLength = 512

var (
	versionPattern  = regexp.MustCompile(`^[0-9]+(\.[0-9A-Za-z]+)*$`)
	platformPattern = regexp.MustCompile(`^[A-Za-z][0-9A-Za-z_]*(-[0-9A-Za-z_]+)*$`)
)

// FilenameIdentity returns the name and version a gem file is cached
// under, read from its filename. A platform build's version keeps its
// platform.
// e.g., "rails-7.1.0.gem" -> ("rails", "7.1.0")
// e.g., "aws-sdk-s3-1.142.0.gem" -> ("aws-sdk-s3", "1.142.0")
// e.g., "sorbet-static-0.5.11002-universal-darwin-22.gem" -> ("sorbet-static", "0.5.11002-universal-darwin-22")
// e.g., "devise-2fa-1.0.0.gem" -> ("devise-2fa", "1.0.0")
//
// A gem file is named {name}-{version}[-{platform}]. RubyGems writes a
// version as dot-separated alphanumeric segments starting with a digit,
// turning any "-" into ".pre.", and a platform starts with a letter (its
// CPU, or java), though it can end in a number (universal-java-17), so
// splitting at the last "-<digit>" would cut the platform. The version
// starts at the first "-<digit>" that leaves such a version and platform,
// so a name with a "-<digit>" segment of its own, such as devise-2fa, is
// kept whole. A filename no split fits is split at the first "-<digit>".
//
// Trying every "-<digit>" costs time quadratic in the filename's length, so
// a filename longer than 512 bytes is refused.
func FilenameIdentity(filename string) (name, version string) {
	if len(filename) > maxFilenameLength {
		return "", ""
	}
	base := strings.TrimSuffix(filename, ".gem")
	first := -1
	for i := 0; i+1 < len(base); i++ {
		if base[i] != '-' || base[i+1] < '0' || base[i+1] > '9' {
			continue
		}
		if first < 0 {
			first = i
		}
		if versionAndPlatform(base[i+1:]) {
			return base[:i], base[i+1:]
		}
	}
	if first < 0 {
		return "", ""
	}
	return base[:first], base[first+1:]
}

// versionAndPlatform reports whether s reads as {version}[-{platform}].
func versionAndPlatform(s string) bool {
	version, platform, hasPlatform := strings.Cut(s, "-")
	return versionPattern.MatchString(version) && (!hasPlatform || platformPattern.MatchString(platform))
}
