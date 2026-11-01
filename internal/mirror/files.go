package mirror

import (
	"fmt"
	"slices"
	"strings"

	"github.com/git-pkgs/proxy/internal/handler"
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
}

var fileEcosystems = map[string]fileEcosystem{
	"pypi": {
		qualifiers: []string{"file_name"},
		attrs: func(file registries.Artifact) map[string]string {
			return map[string]string{"file_name": file.Filename}
		},
		cacheKey: func(pv PackageVersion, file registries.Artifact) (string, string, string, bool) {
			name, version := handler.PyPIFilenameIdentity(file.Filename)
			if name == "" {
				name, version = pv.Name, pv.Version
			}
			return name, version, file.Filename, true
		},
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
	},
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
