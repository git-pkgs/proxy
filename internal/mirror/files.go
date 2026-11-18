package mirror

import (
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/metadata/pypi"
	"github.com/git-pkgs/registries"
)

// fileEcosystem describes an ecosystem whose versions publish several files,
// each mirrored on its own.
type fileEcosystem struct {
	// qualifiers are the PURL qualifiers that select files. A file is
	// mirrored when each one given matches the value attrs reports for it.
	qualifiers []string
	attrs      func(file registries.Artifact) map[string]string
	// cacheKey returns the name, version and filename the download handler
	// caches file under, or false when the handler would not cache it.
	cacheKey func(pv PackageVersion, file registries.Artifact) (name, version, filename string, ok bool)
	// check refuses a package the ecosystem can't mirror, before its files
	// are listed.
	check func(pv PackageVersion) error
	// variants name the kinds of a file, such as each tag combination of a
	// wheel or a conda package's platform, so a refresh mirrors the same
	// kinds of files of a new version as were cached of an old one.
	// cachedVariants names them for the filename cacheKey returns.
	variants       func(file registries.Artifact) []string
	cachedVariants func(cacheFilename string) []string
	// split, when set, moves a file selection the version carries into the
	// qualifiers, returning the version's release number.
	split func(pv PackageVersion) (PackageVersion, error)
	// release, when set, returns the number of the release whose files are
	// cached under cachedVersion, for an ecosystem where the two differ.
	release func(cachedVersion string) string
}

var fileEcosystems = map[string]fileEcosystem{
	// Each platform build of a gem is an artifact of its version. The gem
	// handler caches a platform build under its version with the platform
	// appended, as it reads it from the filename.
	"gem": {
		qualifiers: []string{"platform"},
		attrs: func(file registries.Artifact) map[string]string {
			return map[string]string{"platform": gemArtifactPlatform(file)}
		},
		cacheKey: func(_ PackageVersion, file registries.Artifact) (string, string, string, bool) {
			name, version := handler.GemFilenameIdentity(file.Filename)
			return name, version, file.Filename, name != ""
		},
		variants: func(file registries.Artifact) []string { return []string{gemArtifactPlatform(file)} },
		cachedVariants: func(cacheFilename string) []string {
			_, version := handler.GemFilenameIdentity(cacheFilename)
			_, platform := gemVersionPlatform(version)
			return []string{platform}
		},
		// Gemfile.lock gives a platform build's version with its platform
		// appended, and so do SBOMs made from one.
		split: func(pv PackageVersion) (PackageVersion, error) {
			number, platform, ok := strings.Cut(pv.Version, "-")
			if !ok {
				return pv, nil
			}
			if q := pv.Qualifiers["platform"]; q != "" && q != platform {
				return pv, fmt.Errorf("version %s is for platform %s, not platform=%s", pv.Version, platform, q)
			}
			qualifiers := maps.Clone(pv.Qualifiers)
			if qualifiers == nil {
				qualifiers = map[string]string{}
			}
			qualifiers["platform"] = platform
			pv.Version, pv.Qualifiers = number, qualifiers
			return pv, nil
		},
		release: func(cachedVersion string) string {
			number, _ := gemVersionPlatform(cachedVersion)
			return number
		},
	},
	"pypi": {
		qualifiers: []string{"file_name"},
		attrs: func(file registries.Artifact) map[string]string {
			return map[string]string{"file_name": file.Filename}
		},
		cacheKey: func(pv PackageVersion, file registries.Artifact) (string, string, string, bool) {
			name, version := pypi.FilenameIdentity(file.Filename)
			if name == "" {
				name, version = pv.Name, pv.Version
			}
			return name, version, file.Filename, true
		},
		variants:       func(file registries.Artifact) []string { return pypiVariants(file.Filename) },
		cachedVariants: pypiVariants,
	},
	"conda": {
		qualifiers: []string{"subdir", "build", "type"},
		attrs: func(file registries.Artifact) map[string]string {
			subdir, _ := file.Metadata["subdir"].(string)
			build, _ := file.Metadata["build"].(string)
			return map[string]string{"subdir": subdir, "build": build, "type": condaFileType(file.Filename)}
		},
		cacheKey: func(pv PackageVersion, file registries.Artifact) (string, string, string, bool) {
			channel, _, _ := strings.Cut(pv.Name, "/")
			subdir, _ := file.Metadata["subdir"].(string)
			name, version := handler.CondaFilenameIdentity(file.Filename)
			if name == "" || subdir == "" || condaFileType(file.Filename) == "" {
				return "", "", "", false
			}
			return channel + "/" + name, version, subdir + "/" + file.Filename, true
		},
		check: func(pv PackageVersion) error {
			if !strings.Contains(pv.Name, "/") {
				return fmt.Errorf("conda packages need a channel, such as pkg:conda/conda-forge/%s", pv.Name)
			}
			return nil
		},
		variants: func(file registries.Artifact) []string {
			subdir, _ := file.Metadata["subdir"].(string)
			return []string{condaVariant(subdir, file.Filename)}
		},
		cachedVariants: func(cacheFilename string) []string {
			subdir, filename := path.Split(cacheFilename)
			return []string{condaVariant(strings.TrimSuffix(subdir, "/"), filename)}
		},
	},
}

// gemRubyPlatform is the platform of a gem built for none, which installs
// on all of them.
const gemRubyPlatform = "ruby"

// gemArtifactPlatform returns the platform of a gem build, which the
// RubyGems adapter gives as "ruby" for a build for none.
func gemArtifactPlatform(file registries.Artifact) string {
	platform, _ := file.Metadata["platform"].(string)
	return platform
}

// gemVersionPlatform splits a version a gem file is cached under into the
// release's number and the build's platform. A gem version has no "-", as
// RubyGems writes one as ".pre.", so the platform follows the first.
func gemVersionPlatform(version string) (number, platform string) {
	number, platform, ok := strings.Cut(version, "-")
	if !ok {
		platform = gemRubyPlatform
	}
	return number, platform
}

// pypiVariants names a wheel by each combination of its python, ABI and
// platform tags, which are alternatives where a tag lists several joined by
// dots, with the platform reduced to OS and architecture. Every sdist is
// one variant.
func pypiVariants(filename string) []string {
	stem, wheel := strings.CutSuffix(filename, ".whl")
	if !wheel {
		return []string{"sdist"}
	}
	parts := strings.Split(stem, "-")
	if len(parts) < 5 { //nolint:mnd // name, version and three tags
		return []string{stem}
	}
	tags := parts[len(parts)-3:]
	var variants []string
	for _, python := range strings.Split(tags[0], ".") {
		for _, abi := range strings.Split(tags[1], ".") {
			for _, platform := range strings.Split(tags[2], ".") {
				v := wheelPython(python, abi) + "-" + abi + "-" + wheelPlatform(platform)
				if !slices.Contains(variants, v) {
					variants = append(variants, v)
				}
			}
		}
	}
	return variants
}

// wheelPython reduces the python tag of an abi3 wheel, a minimum such as
// cp39, to its interpreter, so cp38-abi3 and cp311-abi3 wheels are the same
// kind.
func wheelPython(python, abi string) string {
	if abi != "abi3" {
		return python
	}
	return strings.TrimRight(python, "0123456789")
}

// wheelPlatformMinimum matches the glibc, musl or macOS minimum version in a
// platform tag, such as the 2_17 in manylinux_2_17_x86_64 or the 2014 in
// manylinux2014_x86_64.
var wheelPlatformMinimum = regexp.MustCompile(`^(manylinux|musllinux|macosx)_?(?:\d+_\d+|1|2010|2014)_`)

// wheelPlatform reduces a platform tag to OS and architecture, so
// manylinux_2_17_x86_64 and manylinux_2_28_x86_64 name the same platform.
func wheelPlatform(tag string) string {
	return wheelPlatformMinimum.ReplaceAllString(tag, "${1}_")
}

// condaBuildHash matches the hash and build number in a conda build string,
// such as h8753938_0 in py312h8753938_0 or h5f976f7_0 in
// h5f976f7_0_cpython.
var condaBuildHash = regexp.MustCompile(`h[0-9a-f]{7}_\d+`)

// condaVariant names a conda package file by its subdir, its build string
// without the hash, such as py312 or _cpython, and its type.
func condaVariant(subdir, filename string) string {
	fileType := condaFileType(filename)
	stem := strings.TrimSuffix(strings.TrimSuffix(filename, ".conda"), ".tar.bz2")
	build := stem[strings.LastIndex(stem, "-")+1:]
	build = condaBuildHash.ReplaceAllString(build, "")
	return subdir + "/" + build + "/" + fileType
}

// condaFileType returns the PURL type qualifier of a conda package file.
func condaFileType(filename string) string {
	switch {
	case strings.HasSuffix(filename, ".conda"):
		return "conda"
	case strings.HasSuffix(filename, ".tar.bz2"):
		return "tar.bz2"
	default:
		return ""
	}
}

// selection returns the qualifiers of pv that select files, as
// "key=value" pairs, for error messages.
func (e fileEcosystem) selection(pv PackageVersion) string {
	if pv.variants != nil {
		return "the kinds of files cached of earlier versions"
	}
	var pairs []string
	for _, key := range e.qualifiers {
		if value := pv.Qualifiers[key]; value != "" {
			pairs = append(pairs, key+"="+value)
		}
	}
	return strings.Join(pairs, " ")
}

// selects reports whether file matches every selecting qualifier of pv.
func (e fileEcosystem) selects(pv PackageVersion, file registries.Artifact) bool {
	if pv.variants != nil && !slices.ContainsFunc(e.variants(file), func(v string) bool { return pv.variants[v] }) {
		return false
	}
	attrs := e.attrs(file)
	for _, key := range e.qualifiers {
		if want := pv.Qualifiers[key]; want != "" && attrs[key] != want {
			return false
		}
	}
	return true
}

// fileQualifiers keeps the qualifiers of a PURL that select files of its
// ecosystem, or returns nil.
func fileQualifiers(ecosystem string, qualifiers map[string]string) map[string]string {
	e, ok := fileEcosystems[ecosystem]
	if !ok {
		return nil
	}
	var kept map[string]string
	for key, value := range qualifiers {
		if value != "" && slices.Contains(e.qualifiers, key) {
			if kept == nil {
				kept = map[string]string{}
			}
			kept[key] = value
		}
	}
	return kept
}
