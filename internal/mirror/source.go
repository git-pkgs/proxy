package mirror

import (
	"context"
	"fmt"

	"github.com/git-pkgs/purl"
	"github.com/git-pkgs/registries"
	_ "github.com/git-pkgs/registries/all"
	"github.com/git-pkgs/sbom"
)

// PackageVersion identifies a specific package version to mirror.
type PackageVersion struct {
	Ecosystem string
	Name      string
	Version   string
	// Qualifiers are the PURL qualifiers. Images use repository_url, tag and
	// arch.
	Qualifiers map[string]string
	// Platforms limit which platforms of an image index are mirrored.
	Platforms []string
}

func (pv PackageVersion) String() string {
	return fmt.Sprintf("pkg:%s/%s@%s", pv.Ecosystem, pv.Name, pv.Version)
}

// Source produces PackageVersion items for mirroring.
type Source interface {
	Enumerate(ctx context.Context, fn func(PackageVersion) error) error
}

// PURLSource yields packages from PURL strings.
// Versioned PURLs produce a single item. Unversioned PURLs look up all versions from the registry.
type PURLSource struct {
	PURLs     []string
	RegClient *registries.Client
	// Platforms limit which platforms of container images are mirrored.
	Platforms []string
}

func (s *PURLSource) Enumerate(ctx context.Context, fn func(PackageVersion) error) error {
	client := s.RegClient
	if client == nil {
		client = registries.DefaultClient()
	}

	for _, purlStr := range s.PURLs {
		p, err := purl.Parse(purlStr)
		if err != nil {
			return fmt.Errorf("parsing PURL %q: %w", purlStr, err)
		}

		ecosystem := purl.PURLTypeToEcosystem(p.Type)
		name := p.Name
		if p.Namespace != "" {
			name = p.Namespace + "/" + p.Name
		}

		if p.Version != "" || isImageEcosystem(ecosystem) {
			// Images are never expanded to all tags; an unversioned image
			// fails when it is mirrored.
			pv := PackageVersion{Ecosystem: ecosystem, Name: name, Version: p.Version}
			if isImageEcosystem(ecosystem) {
				pv.Qualifiers = p.Qualifiers.Map()
				pv.Platforms = s.Platforms
			}
			if err := fn(pv); err != nil {
				return err
			}
			continue
		}

		// Unversioned: enumerate all versions
		versions, err := s.fetchVersions(ctx, client, ecosystem, name)
		if err != nil {
			return fmt.Errorf("fetching versions for %s/%s: %w", ecosystem, name, err)
		}
		for _, v := range versions {
			if err := fn(PackageVersion{Ecosystem: ecosystem, Name: name, Version: v}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *PURLSource) fetchVersions(ctx context.Context, client *registries.Client, ecosystem, name string) ([]string, error) {
	reg, err := registries.New(purl.EcosystemToPURLType(ecosystem), "", client)
	if err != nil {
		return nil, err
	}
	versions, err := reg.FetchVersions(ctx, name)
	if err != nil {
		return nil, err
	}
	result := make([]string, len(versions))
	for i, v := range versions {
		result[i] = v.Number
	}
	return result, nil
}

// SBOMSource extracts package versions from CycloneDX or SPDX SBOM data.
type SBOMSource struct {
	Data      []byte
	Name      string
	RegClient *registries.Client
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

	inner := &PURLSource{PURLs: purls, RegClient: s.RegClient, Platforms: s.Platforms}
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
			if ref.Type == "purl" && ref.Locator != "" {
				purls = append(purls, ref.Locator)
			}
		}
	}
	if len(purls) == 0 {
		return nil, fmt.Errorf("SBOM contains no package URLs")
	}
	return purls, nil
}
