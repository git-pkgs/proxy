// Package enrichment adapts github.com/git-pkgs/enrichment to the proxy's
// enrichment API. It builds package URLs from the proxy's ecosystem names,
// normalizes licenses for display and treats retracted versions as yanked.
package enrichment

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	shared "github.com/git-pkgs/enrichment"
	"github.com/git-pkgs/proxy/internal/packageurl"
	"github.com/git-pkgs/registries"
	"github.com/git-pkgs/spdx"
)

// Service looks up package, version and vulnerability data.
type Service struct {
	logger     *slog.Logger
	registries *shared.RegistriesClient
	vulns      *shared.VulnerabilityClient
}

// New creates a new enrichment service.
func New(logger *slog.Logger) *Service {
	return &Service{
		logger:     logger,
		registries: shared.NewRegistriesClient(),
		vulns:      shared.NewVulnerabilityClient(),
	}
}

type (
	PackageInfo = shared.PackageInfo
	VersionInfo = shared.VersionInfo
	VulnInfo    = shared.VulnInfo
)

// EnrichPackage fetches metadata for a package from its registry. It returns
// nil without an error when the registry doesn't have the package or the name
// can't be expressed as a PURL.
func (s *Service) EnrichPackage(ctx context.Context, ecosystem, name string) (*PackageInfo, error) {
	purlStr := packageurl.MakeString(ecosystem, name, "")
	if purlStr == "" {
		return nil, nil
	}
	info, err := s.registries.GetPackage(ctx, purlStr)
	if errors.Is(err, registries.ErrNotFound) {
		return nil, nil
	}
	if err != nil || info == nil {
		return nil, err
	}
	info.Ecosystem = ecosystem
	info.License = normalizeLicense(info.License)
	return info, nil
}

// EnrichVersion fetches metadata for a specific package version.
func (s *Service) EnrichVersion(ctx context.Context, ecosystem, name, version string) (*VersionInfo, error) {
	purlStr := packageurl.MakeString(ecosystem, name, version)
	if purlStr == "" {
		return nil, nil
	}
	info, err := s.registries.GetVersion(ctx, purlStr)
	if err != nil || info == nil {
		return nil, err
	}
	adaptVersion(info)
	return info, nil
}

// adaptVersion reports retracted versions as yanked and normalizes the license.
func adaptVersion(info *VersionInfo) {
	if info.Status == string(registries.StatusRetracted) {
		info.Yanked = true
	}
	info.License = normalizeLicense(info.License)
}

// BulkEnrichPackages fetches metadata for multiple packages in parallel,
// keyed by PURL. Packages that can't be fetched are left out.
func (s *Service) BulkEnrichPackages(ctx context.Context, packages []struct{ Ecosystem, Name string }) map[string]*PackageInfo {
	purls := make([]string, 0, len(packages))
	for _, pkg := range packages {
		if purlStr := packageurl.MakeString(pkg.Ecosystem, pkg.Name, ""); purlStr != "" {
			purls = append(purls, purlStr)
		}
	}
	if len(purls) == 0 {
		return map[string]*PackageInfo{}
	}
	result, _ := s.registries.BulkLookup(ctx, purls)
	for _, info := range result {
		info.License = normalizeLicense(info.License)
	}
	return result
}

// CheckVulnerabilities queries for vulnerabilities affecting a package version.
func (s *Service) CheckVulnerabilities(ctx context.Context, ecosystem, name, version string) ([]VulnInfo, error) {
	if packageurl.Make(ecosystem, name, version) == nil {
		return nil, nil
	}
	return s.vulns.Check(ctx, ecosystem, name, version)
}

// GetLatestVersion fetches the latest version for a package.
func (s *Service) GetLatestVersion(ctx context.Context, ecosystem, name string) (string, error) {
	info, err := s.EnrichPackage(ctx, ecosystem, name)
	if err != nil || info == nil {
		return "", err
	}
	return info.LatestVersion, nil
}

// normalizeLicense returns license as an SPDX expression when it can be
// parsed as one, and unchanged otherwise.
func normalizeLicense(license string) string {
	if license == "" {
		return ""
	}
	if normalized, err := spdx.NormalizeExpressionLax(license); err == nil {
		return normalized
	}
	return license
}

// EnrichmentResult contains all enrichment data for a package version.
type EnrichmentResult struct {
	Package         *PackageInfo
	Version         *VersionInfo
	Vulnerabilities []VulnInfo
	IsOutdated      bool
	LicenseCategory shared.LicenseCategory
}

// EnrichFull fetches package, version and vulnerability data for a package
// version concurrently. Failed lookups are logged and left empty.
func (s *Service) EnrichFull(ctx context.Context, ecosystem, name, version string) (*EnrichmentResult, error) {
	result := &EnrichmentResult{}

	var wg sync.WaitGroup
	var pkgErr, verErr, vulnErr error
	wg.Go(func() { result.Package, pkgErr = s.EnrichPackage(ctx, ecosystem, name) })
	wg.Go(func() { result.Version, verErr = s.EnrichVersion(ctx, ecosystem, name, version) })
	wg.Go(func() { result.Vulnerabilities, vulnErr = s.CheckVulnerabilities(ctx, ecosystem, name, version) })
	wg.Wait()

	if pkgErr != nil {
		s.logger.Debug("failed to enrich package", "ecosystem", ecosystem, "name", name, "error", pkgErr)
	}
	if verErr != nil {
		s.logger.Debug("failed to enrich version", "ecosystem", ecosystem, "name", name, "version", version, "error", verErr)
	}
	if vulnErr != nil {
		s.logger.Debug("failed to check vulnerabilities", "ecosystem", ecosystem, "name", name, "version", version, "error", vulnErr)
	}

	if result.Package != nil {
		result.IsOutdated = shared.IsOutdated(version, result.Package.LatestVersion)
	}

	license := ""
	if result.Version != nil && result.Version.License != "" {
		license = result.Version.License
	} else if result.Package != nil {
		license = result.Package.License
	}
	result.LicenseCategory = shared.CategorizeLicense(license)

	return result, nil
}
