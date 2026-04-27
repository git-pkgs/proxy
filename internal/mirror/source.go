package mirror

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"

	cdx "github.com/CycloneDX/cyclonedx-go"
	"github.com/git-pkgs/purl"
	"github.com/git-pkgs/registries"
	_ "github.com/git-pkgs/registries/all"
	spdxjson "github.com/spdx/tools-golang/json"
	"github.com/spdx/tools-golang/spdx"
	spdxtv "github.com/spdx/tools-golang/tagvalue"
)

// PackageVersion identifies a specific package version to mirror.
type PackageVersion struct {
	Ecosystem string
	Name      string
	Version   string
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

		if p.Version != "" {
			if err := fn(PackageVersion{Ecosystem: ecosystem, Name: name, Version: p.Version}); err != nil {
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

// SBOMSource extracts package versions from a CycloneDX or SPDX SBOM file.
type SBOMSource struct {
	Path      string
	RegClient *registries.Client
}

func (s *SBOMSource) Enumerate(ctx context.Context, fn func(PackageVersion) error) error {
	purls, err := s.extractPURLs()
	if err != nil {
		return fmt.Errorf("reading SBOM %s: %w", s.Path, err)
	}

	inner := &PURLSource{PURLs: purls, RegClient: s.RegClient}
	return inner.Enumerate(ctx, fn)
}

func (s *SBOMSource) extractPURLs() ([]string, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, err
	}

	// Try CycloneDX first
	if purls, err := extractCycloneDXPURLs(data); err == nil && len(purls) > 0 {
		return purls, nil
	}

	// Try SPDX JSON
	if purls, err := extractSPDXJSONPURLs(data); err == nil && len(purls) > 0 {
		return purls, nil
	}

	// Try SPDX tag-value
	if purls, err := extractSPDXTVPURLs(data); err == nil && len(purls) > 0 {
		return purls, nil
	}

	return nil, fmt.Errorf("could not parse SBOM as CycloneDX or SPDX")
}

func extractCycloneDXPURLs(data []byte) ([]string, error) {
	bom := new(cdx.BOM)
	if err := json.Unmarshal(data, bom); err != nil {
		// Try XML
		decoder := cdx.NewBOMDecoder(bytes.NewReader(data), cdx.BOMFileFormatXML)
		bom = new(cdx.BOM)
		if err := decoder.Decode(bom); err != nil {
			return nil, err
		}
	}

	if bom.Components == nil {
		return nil, nil
	}

	var purls []string
	for _, c := range *bom.Components {
		if c.PackageURL != "" {
			purls = append(purls, c.PackageURL)
		}
	}
	return purls, nil
}

func extractSPDXJSONPURLs(data []byte) ([]string, error) {
	doc, err := spdxjson.Read(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return extractSPDXDocPURLs(doc), nil
}

func extractSPDXTVPURLs(data []byte) ([]string, error) {
	doc, err := spdxtv.Read(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return extractSPDXDocPURLs(doc), nil
}

func extractSPDXDocPURLs(doc *spdx.Document) []string {
	if doc == nil {
		return nil
	}
	var purls []string
	for _, pkg := range doc.Packages {
		for _, ref := range pkg.PackageExternalReferences {
			if ref.RefType == "purl" {
				purls = append(purls, ref.Locator)
			}
		}
	}
	return purls
}
