package enrichment

import (
	"context"
	"time"

	"github.com/ecosyste-ms/ecosystems-go"
	"github.com/git-pkgs/registries"
	"github.com/package-url/packageurl-go"
)

// EcosystemsClient wraps the ecosyste.ms API client for bulk metadata lookups.
type EcosystemsClient struct {
	client *ecosystems.Client
}

// NewEcosystemsClient creates a client that uses the ecosyste.ms API.
func NewEcosystemsClient() (*EcosystemsClient, error) {
	client, err := ecosystems.NewClient("git-pkgs-proxy/1.0")
	if err != nil {
		return nil, err
	}
	return &EcosystemsClient{client: client}, nil
}

// BulkLookup fetches package metadata for multiple PURLs in a single request.
// This is more efficient than individual lookups when enriching many packages.
func (c *EcosystemsClient) BulkLookup(ctx context.Context, purls []string) (map[string]*PackageInfo, error) {
	packages, err := c.client.BulkLookup(ctx, purls)
	if err != nil {
		return nil, err
	}

	result := make(map[string]*PackageInfo, len(packages))
	for purl, pkg := range packages {
		if pkg == nil {
			continue
		}

		info := &PackageInfo{
			Ecosystem:   pkg.Ecosystem,
			Name:        pkg.Name,
			RegistryURL: registries.DefaultURL(pkg.Ecosystem),
		}
		if pkg.LatestReleaseNumber != nil {
			info.LatestVersion = *pkg.LatestReleaseNumber
		}
		if len(pkg.NormalizedLicenses) > 0 {
			info.License = pkg.NormalizedLicenses[0]
		} else if pkg.Licenses != nil && *pkg.Licenses != "" {
			info.License = *pkg.Licenses
		}
		if pkg.Description != nil {
			info.Description = *pkg.Description
		}
		if pkg.Homepage != nil {
			info.Homepage = *pkg.Homepage
		}
		if pkg.RepositoryUrl != nil {
			info.Repository = *pkg.RepositoryUrl
		}
		result[purl] = info
	}
	return result, nil
}

// GetVersions fetches all versions for a package by PURL.
func (c *EcosystemsClient) GetVersions(ctx context.Context, purl string) ([]VersionInfo, error) {
	p, err := packageurl.FromString(purl)
	if err != nil {
		return nil, nil
	}

	versions, err := c.client.GetAllVersionsPURL(ctx, p)
	if err != nil {
		return nil, err
	}

	result := make([]VersionInfo, 0, len(versions))
	for _, v := range versions {
		info := VersionInfo{Number: v.Number}
		if v.PublishedAt != nil {
			info.PublishedAt, _ = time.Parse(time.RFC3339, *v.PublishedAt)
		}
		result = append(result, info)
	}
	return result, nil
}

// GetVersion fetches a specific version for a package by PURL.
func (c *EcosystemsClient) GetVersion(ctx context.Context, purl string) (*VersionInfo, error) {
	p, err := packageurl.FromString(purl)
	if err != nil {
		return nil, nil
	}

	v, err := c.client.GetVersionPURL(ctx, p)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}

	info := &VersionInfo{Number: v.Number}
	if v.PublishedAt != nil {
		info.PublishedAt, _ = time.Parse(time.RFC3339, *v.PublishedAt)
	}
	if v.Integrity != nil {
		info.Integrity = *v.Integrity
	}
	return info, nil
}
