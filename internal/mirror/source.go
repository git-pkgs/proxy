package mirror

import (
	"context"
	"fmt"
	"strings"

	"github.com/git-pkgs/purl"
	"github.com/git-pkgs/sbom"
)

// PackageVersion identifies a specific package version to mirror.
type PackageVersion struct {
	Ecosystem string
	Name      string
	Version   string
	// Qualifiers are the PURL qualifiers. Images use repository_url, tag and
	// arch; PyPI uses file_name, and conda subdir, build and type, to select
	// which files of the version are mirrored. Julia uses uuid to identify
	// the package.
	Qualifiers map[string]string
	// Platforms limit which platforms of an image index are mirrored.
	Platforms []string
}

func (pv PackageVersion) String() string {
	return fmt.Sprintf("pkg:%s/%s@%s", pv.Ecosystem, pv.Name, pv.Version)
}

// handlerEcosystem returns the ecosystem name the proxy's handlers store
// packages of a PURL type under. purl names gem and composer after their
// registries, rubygems and packagist; the handlers use the type.
func handlerEcosystem(purlType string) string {
	switch purlType {
	case "gem", "composer":
		return purlType
	default:
		return purl.PURLTypeToEcosystem(purlType)
	}
}

// Source produces PackageVersion items for mirroring.
type Source interface {
	Enumerate(ctx context.Context, fn func(PackageVersion) error) error
}

// PURLSource yields packages from PURL strings.
// Versioned PURLs produce a single item. Unversioned PURLs produce one item
// per version Versions lists, or only the newest with Latest.
type PURLSource struct {
	PURLs []string
	// Versions lists the versions of unversioned PURLs. Nil lists them from
	// the public registries.
	Versions VersionLister
	// Latest mirrors only the newest version of unversioned PURLs.
	Latest bool
	// Platforms limit which platforms of container images are mirrored.
	Platforms []string
}

func (s *PURLSource) Enumerate(ctx context.Context, fn func(PackageVersion) error) error {
	versions := s.Versions
	if versions == nil {
		versions = RegistryVersions{}
	}

	for _, purlStr := range s.PURLs {
		p, err := purl.Parse(purlStr)
		if err != nil {
			return fmt.Errorf("parsing PURL %q: %w", purlStr, err)
		}

		ecosystem, name, qualifiers := mirrorTarget(p)

		// Images are never expanded to all tags, and conda packages without
		// a channel aren't listed; both fail when they are mirrored.
		if p.Version != "" || isImageEcosystem(ecosystem) || (ecosystem == "conda" && !strings.Contains(name, "/")) {
			pv := PackageVersion{Ecosystem: ecosystem, Name: name, Version: p.Version, Qualifiers: qualifiers}
			if isImageEcosystem(ecosystem) {
				pv.Qualifiers = p.Qualifiers.Map()
			}
			if takesPlatforms(ecosystem) {
				pv.Platforms = s.Platforms
			}
			if err := fn(pv); err != nil {
				return err
			}
			continue
		}

		// Unversioned: enumerate all versions, or the newest
		listed, err := s.listVersions(ctx, versions, ecosystem, name)
		if err != nil {
			return fmt.Errorf("fetching versions for %s/%s: %w", ecosystem, name, err)
		}
		for _, v := range listed {
			pv := PackageVersion{Ecosystem: ecosystem, Name: name, Version: v, Qualifiers: qualifiers}
			if takesPlatforms(ecosystem) {
				pv.Platforms = s.Platforms
			}
			if err := fn(pv); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *PURLSource) listVersions(ctx context.Context, versions VersionLister, ecosystem, name string) ([]string, error) {
	if !s.Latest {
		return versions.ListVersions(ctx, ecosystem, name)
	}
	var latest string
	if finder, ok := versions.(LatestFinder); ok {
		v, err := finder.LatestVersion(ctx, ecosystem, name)
		if err != nil {
			return nil, err
		}
		latest = v
	} else {
		listed, err := versions.ListVersions(ctx, ecosystem, name)
		if err != nil {
			return nil, err
		}
		latest = newestVersion(ecosystem, listed)
	}
	if latest == "" {
		return nil, nil
	}
	return []string{latest}, nil
}

// mirrorTarget returns the ecosystem, name and file qualifiers a PURL is
// mirrored under.
func mirrorTarget(p *purl.PURL) (ecosystem, name string, qualifiers map[string]string) {
	ecosystem = handlerEcosystem(p.Type)
	name = p.Name
	if ecosystem == "conda" {
		name = condaName(p.Namespace, p.Name, p.Qualifier("channel"))
	} else if p.Namespace != "" {
		name = p.Namespace + "/" + p.Name
	}
	qualifiers = fileQualifiers(ecosystem, p.Qualifiers.Map())
	if ecosystem == "julia" {
		qualifiers = map[string]string{"uuid": p.Qualifier("uuid")}
	}
	return ecosystem, name, qualifiers
}

// SBOMSource extracts package versions from CycloneDX or SPDX SBOM data.
type SBOMSource struct {
	Data []byte
	Name string
	// Versions lists the versions of unversioned PURLs. Nil lists them from
	// the public registries.
	Versions VersionLister
	// Latest mirrors only the newest version of unversioned PURLs.
	Latest bool
	// Platforms limit which platforms of container images are mirrored.
	Platforms []string
}

func (s *SBOMSource) Enumerate(ctx context.Context, fn func(PackageVersion) error) error {
	purls, err := s.extractPURLs()
	if err != nil {
		if s.Name != "" {
			return fmt.Errorf("parsing SBOM %s: %w", s.Name, err)
		}
		return fmt.Errorf("parsing SBOM: %w", err)
	}

	inner := &PURLSource{PURLs: purls, Versions: s.Versions, Latest: s.Latest, Platforms: s.Platforms}
	return inner.Enumerate(ctx, fn)
}

func (s *SBOMSource) extractPURLs() ([]string, error) {
	doc, err := sbom.Parse(s.Data)
	if err != nil {
		return nil, err
	}
	var purls []string
	for _, p := range doc.Packages {
		for _, ref := range p.ExternalRefs {
			if ref.Type != "purl" || ref.Locator == "" {
				continue
			}
			locator := ref.Locator
			if p.Version != "" {
				parsed, err := purl.Parse(locator)
				if err != nil {
					return nil, fmt.Errorf("parsing PURL %q: %w", locator, err)
				}
				if parsed.Version == "" {
					locator = parsed.WithVersion(p.Version).String()
				}
			}
			purls = append(purls, locator)
		}
	}
	if len(purls) == 0 {
		return nil, fmt.Errorf("SBOM contains no package URLs")
	}
	return purls, nil
}
