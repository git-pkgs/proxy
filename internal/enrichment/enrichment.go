// Package enrichment provides package metadata enrichment using external data sources.
// It fetches license information, vulnerability data, and version information
// from package registries and vulnerability databases.
package enrichment

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/git-pkgs/purl"
	"github.com/git-pkgs/registries"
	_ "github.com/git-pkgs/registries/all" // Import all registry implementations
	"github.com/git-pkgs/spdx"
	"github.com/git-pkgs/vers"
	"github.com/git-pkgs/vulns"
	"github.com/git-pkgs/vulns/osv"
)

// Service provides package enrichment capabilities.
type Service struct {
	logger     *slog.Logger
	regClient  *registries.Client
	vulnSource vulns.Source
}

// New creates a new enrichment service.
func New(logger *slog.Logger) *Service {
	return &Service{
		logger:     logger,
		regClient:  registries.DefaultClient(),
		vulnSource: osv.New(),
	}
}

// PackageInfo contains enriched package metadata.
type PackageInfo struct {
	Ecosystem     string
	Name          string
	LatestVersion string
	License       string
	Description   string
	Homepage      string
	Repository    string
	RegistryURL   string
}

// VersionInfo contains enriched version metadata.
type VersionInfo struct {
	Number      string
	License     string
	PublishedAt time.Time
	Integrity   string
	Yanked      bool
}

// VulnInfo contains vulnerability information for a package version.
type VulnInfo struct {
	ID           string
	Summary      string
	Severity     string
	CVSSScore    float64
	FixedVersion string
	References   []string
}

// EnrichPackage fetches metadata for a package from registry APIs.
func (s *Service) EnrichPackage(ctx context.Context, ecosystem, name string) (*PackageInfo, error) {
	purlStr := purl.MakePURLString(ecosystem, name, "")

	pkg, err := registries.FetchPackageFromPURL(ctx, purlStr, s.regClient)
	if err != nil {
		return nil, err
	}

	if pkg == nil {
		return nil, nil
	}

	info := &PackageInfo{
		Ecosystem:     ecosystem,
		Name:          pkg.Name,
		LatestVersion: pkg.LatestVersion,
		Description:   pkg.Description,
		Homepage:      pkg.Homepage,
		Repository:    pkg.Repository,
		RegistryURL:   registries.DefaultURL(ecosystem),
	}

	// Normalize license to SPDX format
	if pkg.Licenses != "" {
		if normalized, err := spdx.NormalizeExpressionLax(pkg.Licenses); err == nil {
			info.License = normalized
		} else {
			info.License = pkg.Licenses
		}
	}

	return info, nil
}

// EnrichVersion fetches metadata for a specific package version.
func (s *Service) EnrichVersion(ctx context.Context, ecosystem, name, version string) (*VersionInfo, error) {
	purlStr := purl.MakePURLString(ecosystem, name, version)

	ver, err := registries.FetchVersionFromPURL(ctx, purlStr, s.regClient)
	if err != nil {
		return nil, err
	}

	if ver == nil {
		return nil, nil
	}

	info := &VersionInfo{
		Number:      ver.Number,
		PublishedAt: ver.PublishedAt,
		Integrity:   ver.Integrity,
		Yanked:      ver.Status == registries.StatusYanked || ver.Status == registries.StatusRetracted,
	}

	// Normalize license
	if ver.Licenses != "" {
		if normalized, err := spdx.NormalizeExpressionLax(ver.Licenses); err == nil {
			info.License = normalized
		} else {
			info.License = ver.Licenses
		}
	}

	return info, nil
}

// BulkEnrichPackages fetches metadata for multiple packages in parallel.
func (s *Service) BulkEnrichPackages(ctx context.Context, packages []struct{ Ecosystem, Name string }) map[string]*PackageInfo {
	purls := make([]string, len(packages))
	for i, pkg := range packages {
		purls[i] = purl.MakePURLString(pkg.Ecosystem, pkg.Name, "")
	}

	pkgData := registries.BulkFetchPackages(ctx, purls, s.regClient)

	result := make(map[string]*PackageInfo, len(pkgData))
	for purlStr, pkg := range pkgData {
		if pkg == nil {
			continue
		}

		p, _ := purl.Parse(purlStr)
		info := &PackageInfo{
			Ecosystem:     p.Type,
			Name:          pkg.Name,
			LatestVersion: pkg.LatestVersion,
			Description:   pkg.Description,
			Homepage:      pkg.Homepage,
			Repository:    pkg.Repository,
			RegistryURL:   registries.DefaultURL(p.Type),
		}

		if pkg.Licenses != "" {
			if normalized, err := spdx.NormalizeExpressionLax(pkg.Licenses); err == nil {
				info.License = normalized
			} else {
				info.License = pkg.Licenses
			}
		}

		result[purlStr] = info
	}

	return result
}

// CheckVulnerabilities queries for vulnerabilities affecting a package version.
func (s *Service) CheckVulnerabilities(ctx context.Context, ecosystem, name, version string) ([]VulnInfo, error) {
	p := purl.MakePURL(ecosystem, name, version)

	vulnList, err := s.vulnSource.Query(ctx, p)
	if err != nil {
		return nil, err
	}

	results := make([]VulnInfo, 0, len(vulnList))
	for _, v := range vulnList {
		info := VulnInfo{
			ID:           v.ID,
			Summary:      v.Summary,
			Severity:     v.SeverityLevel(),
			CVSSScore:    v.CVSSScore(),
			FixedVersion: v.FixedVersion(ecosystem, name),
		}

		for _, ref := range v.References {
			info.References = append(info.References, ref.URL)
		}

		results = append(results, info)
	}

	return results, nil
}

// BulkCheckVulnerabilities queries vulnerabilities for multiple package versions.
func (s *Service) BulkCheckVulnerabilities(ctx context.Context, packages []struct{ Ecosystem, Name, Version string }) (map[string][]VulnInfo, error) {
	purls := make([]*purl.PURL, len(packages))
	for i, pkg := range packages {
		purls[i] = purl.MakePURL(pkg.Ecosystem, pkg.Name, pkg.Version)
	}

	vulnResults, err := s.vulnSource.QueryBatch(ctx, purls)
	if err != nil {
		return nil, err
	}

	result := make(map[string][]VulnInfo, len(packages))
	for i, vulnList := range vulnResults {
		pkg := packages[i]
		key := purl.MakePURLString(pkg.Ecosystem, pkg.Name, pkg.Version)

		var infos []VulnInfo
		for _, v := range vulnList {
			info := VulnInfo{
				ID:           v.ID,
				Summary:      v.Summary,
				Severity:     v.SeverityLevel(),
				CVSSScore:    v.CVSSScore(),
				FixedVersion: v.FixedVersion(pkg.Ecosystem, pkg.Name),
			}
			for _, ref := range v.References {
				info.References = append(info.References, ref.URL)
			}
			infos = append(infos, info)
		}
		result[key] = infos
	}

	return result, nil
}

// IsOutdated checks if a version is older than the latest version.
func (s *Service) IsOutdated(currentVersion, latestVersion string) bool {
	if latestVersion == "" || currentVersion == "" {
		return false
	}
	return vers.Compare(currentVersion, latestVersion) < 0
}

// GetLatestVersion fetches the latest version for a package.
func (s *Service) GetLatestVersion(ctx context.Context, ecosystem, name string) (string, error) {
	purlStr := purl.MakePURLString(ecosystem, name, "")

	latest, err := registries.FetchLatestVersionFromPURL(ctx, purlStr, s.regClient)
	if err != nil {
		return "", err
	}

	if latest == nil {
		return "", nil
	}

	return latest.Number, nil
}

// LicenseCategory represents the category of a license.
type LicenseCategory string

const (
	LicensePermissive LicenseCategory = "permissive"
	LicenseCopyleft   LicenseCategory = "copyleft"
	LicenseUnknown    LicenseCategory = "unknown"
)

// CategorizeLicense returns the category of a license.
func (s *Service) CategorizeLicense(license string) LicenseCategory {
	if license == "" || license == "Unknown" {
		return LicenseUnknown
	}

	if spdx.HasCopyleft(license) {
		return LicenseCopyleft
	}

	if spdx.IsFullyPermissive(license) {
		return LicensePermissive
	}

	return LicenseUnknown
}

// NormalizeLicense normalizes a license string to SPDX format.
func (s *Service) NormalizeLicense(license string) string {
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
	LicenseCategory LicenseCategory
}

// EnrichFull performs full enrichment for a package version.
func (s *Service) EnrichFull(ctx context.Context, ecosystem, name, version string) (*EnrichmentResult, error) {
	result := &EnrichmentResult{}

	var wg sync.WaitGroup
	var pkgErr, verErr, vulnErr error

	// Fetch package info
	wg.Add(1)
	go func() {
		defer wg.Done()
		result.Package, pkgErr = s.EnrichPackage(ctx, ecosystem, name)
	}()

	// Fetch version info
	wg.Add(1)
	go func() {
		defer wg.Done()
		result.Version, verErr = s.EnrichVersion(ctx, ecosystem, name, version)
	}()

	// Check vulnerabilities
	wg.Add(1)
	go func() {
		defer wg.Done()
		result.Vulnerabilities, vulnErr = s.CheckVulnerabilities(ctx, ecosystem, name, version)
	}()

	wg.Wait()

	// Log errors but don't fail
	if pkgErr != nil {
		s.logger.Debug("failed to enrich package", "ecosystem", ecosystem, "name", name, "error", pkgErr)
	}
	if verErr != nil {
		s.logger.Debug("failed to enrich version", "ecosystem", ecosystem, "name", name, "version", version, "error", verErr)
	}
	if vulnErr != nil {
		s.logger.Debug("failed to check vulnerabilities", "ecosystem", ecosystem, "name", name, "version", version, "error", vulnErr)
	}

	// Determine outdated status
	if result.Package != nil && result.Package.LatestVersion != "" {
		result.IsOutdated = s.IsOutdated(version, result.Package.LatestVersion)
	}

	// Categorize license
	license := ""
	if result.Version != nil && result.Version.License != "" {
		license = result.Version.License
	} else if result.Package != nil && result.Package.License != "" {
		license = result.Package.License
	}
	result.LicenseCategory = s.CategorizeLicense(license)

	return result, nil
}
