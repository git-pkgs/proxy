package pypi

import "strings"

const (
	minWheelParts = 5 // name + version + python + abi + platform
	minEggParts   = 3 // name + version + python tag

	// MetadataSuffix is the PEP 658 core-metadata sidecar suffix that pip
	// appends to a distribution URL when the index advertises core metadata.
	// A sidecar resolves to the same name and version as the distribution it
	// describes, so it is cached alongside it; consumers that expect an openable
	// archive must skip these.
	MetadataSuffix = ".metadata"
)

// archiveExtensions are sdist formats of the form {name}-{version}{ext}. They
// carry no trailing tags, but legacy sdist names may contain hyphens.
var archiveExtensions = []string{".tar.gz", ".tar.bz2", ".tar.xz", ".tar.Z", ".tgz", ".tar", ".zip"}

// windowsInstallerExtensions are the legacy distutils bdist_wininst and
// bdist_msi formats, which share a filename layout.
var windowsInstallerExtensions = []string{".exe", ".msi"}

// FilenameIdentity extracts the package name and version a PyPI file is
// cached under from its filename, so proxy mirror stores files where the
// download handler looks for them. Handles wheels, sdists and legacy bdist
// formats:
// - requests-2.31.0-py3-none-any.whl
// - requests-2.31.0.tar.gz
// - numpy-1.8.0-py2.7-macosx-10.9-x86_64.egg
// - numpy-1.8.0.win32-py2.7.exe
func FilenameIdentity(filename string) (name, version string) {
	// PEP 658/714 core-metadata sidecars are the distribution filename plus
	// ".metadata"; they describe the same name and version. Without this, pip's
	// metadata-only fetches fall back to a hash-derived package identifier.
	filename = strings.TrimSuffix(filename, MetadataSuffix)

	switch {
	case strings.HasSuffix(filename, ".whl"):
		return parseWheelFilename(strings.TrimSuffix(filename, ".whl"))
	case strings.HasSuffix(filename, ".egg"):
		return parseEggFilename(strings.TrimSuffix(filename, ".egg"))
	}

	for _, ext := range windowsInstallerExtensions {
		if strings.HasSuffix(filename, ext) {
			return parseWindowsInstallerFilename(strings.TrimSuffix(filename, ext))
		}
	}

	for _, ext := range archiveExtensions {
		if strings.HasSuffix(filename, ext) {
			return splitNameVersion(strings.TrimSuffix(filename, ext))
		}
	}

	return "", ""
}

// parseWheelFilename parses the PEP 427 layout
// {name}-{version}(-{build})?-{python}-{abi}-{platform}, base being the
// filename without its ".whl" suffix. The spec escapes every hyphen in the name
// and version to '_', so the first two fields are authoritative even when the
// optional build tag is present.
func parseWheelFilename(base string) (name, version string) {
	parts := strings.Split(base, "-")
	if len(parts) < minWheelParts {
		return "", ""
	}

	return parts[0], parts[1]
}

// parseEggFilename parses the setuptools bdist_egg layout
// {name}-{version}-py{X.Y}(-{platform})?, base being the filename without its
// ".egg" suffix. setuptools escapes hyphens in the name and version to '_', but
// eggs built by other tooling do not always, so the version is located relative
// to the interpreter field rather than assumed to be the second field.
func parseEggFilename(base string) (name, version string) {
	parts := strings.Split(base, "-")
	// Scan from the end: the trailing platform fields never look like an
	// interpreter tag, so the last match is the real one even when the package
	// name itself carries a "py{N}" component. Stop before index 1, since a tag
	// any earlier would leave no room for both a name and a version.
	for i := len(parts) - 1; i >= minEggParts-1; i-- {
		if !isEggPythonTag(parts[i]) || !isVersionField(parts[i-1]) {
			continue
		}

		return strings.Join(parts[:i-1], "-"), parts[i-1]
	}

	// No interpreter field: {name}-{version}.
	return splitNameVersion(base)
}

// parseWindowsInstallerFilename parses the distutils bdist_wininst and
// bdist_msi layout {name}-{version}.{platform}(-py{X.Y})?, base being the
// filename without its ".exe" or ".msi" suffix. The platform is joined to the
// version with a '.' rather than a '-' and may itself contain a hyphen
// ("win-amd64"), so both trailing fields are stripped before the name and
// version are split apart.
func parseWindowsInstallerFilename(base string) (name, version string) {
	if i := strings.LastIndex(base, "-py"); i >= 0 && isDottedNumber(base[i+len("-py"):]) {
		base = base[:i]
	}

	// The platform is the final '.'-separated field. Requiring it to start with
	// a non-digit keeps a dotted version from being truncated when a filename
	// carries no platform tag.
	i := strings.LastIndex(base, ".")
	if i < 0 || i+1 >= len(base) || isVersionStart(base[i+1]) {
		return "", ""
	}

	return splitFullname(base[:i])
}

// splitFullname splits the distutils fullname {name}-{version} that precedes a
// Windows installer's platform field. Unlike an sdist, a wininst fullname may
// carry a trailing build variant ("cx_Oracle-5.1.2-11g"), which belongs to
// neither the name nor the version, so the first purely numeric field wins and
// anything after it is discarded.
func splitFullname(fullname string) (name, version string) {
	parts := strings.Split(fullname, "-")
	for i := 1; i < len(parts); i++ {
		if isDottedNumber(parts[i]) {
			return strings.Join(parts[:i], "-"), parts[i]
		}
	}

	// No purely numeric field, e.g. a prerelease version like "1.0b1".
	return splitNameVersion(fullname)
}

// splitNameVersion splits a {name}-{version} pair at the last hyphen that
// starts a version, leaving hyphens inside the name intact.
func splitNameVersion(base string) (name, version string) {
	for i := len(base) - 1; i >= 0; i-- {
		if base[i] == '-' && i+1 < len(base) && isVersionStart(base[i+1]) {
			return base[:i], base[i+1:]
		}
	}

	return "", ""
}

// isEggPythonTag reports whether field is the py{X.Y} interpreter field that
// setuptools places directly after the version in an egg filename.
func isEggPythonTag(field string) bool {
	const prefix = "py"

	return len(field) > len(prefix) && strings.HasPrefix(field, prefix) && isVersionStart(field[len(prefix)])
}

// isVersionField reports whether field can be a version, i.e. it is non-empty
// and starts with a digit as every PEP 440 release segment does.
func isVersionField(field string) bool {
	return field != "" && isVersionStart(field[0])
}

// isDottedNumber reports whether s is a dotted numeric version such as "2.7".
func isDottedNumber(s string) bool {
	if s == "" || !isVersionStart(s[0]) {
		return false
	}

	for i := range len(s) {
		if !isVersionStart(s[i]) && s[i] != '.' {
			return false
		}
	}

	return true
}

func isVersionStart(c byte) bool {
	return c >= '0' && c <= '9'
}
