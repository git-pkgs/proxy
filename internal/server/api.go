package server

import (
	"encoding/json"
	"net/http"
	"strings"

	shared "github.com/git-pkgs/enrichment"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/enrichment"
	"github.com/git-pkgs/purl"
	"github.com/go-chi/chi/v5"
)

const (
	maxBodySize            = 1 << 20 // 1 MB
	licenseCategoryUnknown = "unknown"
	defaultSortBy          = "hits"
)

// APIHandler provides REST endpoints for package enrichment data.
type APIHandler struct {
	enrichment *enrichment.Service
	ecosystems *shared.EcosystemsClient
	db         DBSearcher
}

// DBSearcher defines the interface for database search operations.
type DBSearcher interface {
	SearchPackages(query string, ecosystem string, limit int, offset int) ([]database.SearchResult, error)
	CountSearchResults(query string, ecosystem string) (int64, error)
	ListCachedPackages(ecosystem string, sortBy string, limit int, offset int) ([]database.PackageListItem, error)
	CountCachedPackages(ecosystem string) (int64, error)
}

// NewAPIHandler creates a new API handler with enrichment services.
func NewAPIHandler(svc *enrichment.Service, db DBSearcher) *APIHandler {
	h := &APIHandler{
		enrichment: svc,
		db:         db,
	}
	// Try to initialize ecosystems client for bulk lookups
	if client, err := shared.NewEcosystemsClient(); err == nil {
		h.ecosystems = client
	}
	return h
}

// PackageResponse contains enriched package metadata.
type PackageResponse struct {
	Ecosystem       string `json:"ecosystem"`
	Name            string `json:"name"`
	LatestVersion   string `json:"latest_version,omitempty"`
	License         string `json:"license,omitempty"`
	LicenseCategory string `json:"license_category,omitempty"`
	Description     string `json:"description,omitempty"`
	Homepage        string `json:"homepage,omitempty"`
	Repository      string `json:"repository,omitempty"`
	RegistryURL     string `json:"registry_url,omitempty"`
}

// VersionResponse contains enriched version metadata.
type VersionResponse struct {
	Ecosystem   string `json:"ecosystem"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	License     string `json:"license,omitempty"`
	PublishedAt string `json:"published_at,omitempty"`
	Integrity   string `json:"integrity,omitempty"`
	Yanked      bool   `json:"yanked"`
	IsOutdated  bool   `json:"is_outdated"`
}

// VulnResponse contains vulnerability information.
type VulnResponse struct {
	ID           string   `json:"id"`
	Summary      string   `json:"summary,omitempty"`
	Severity     string   `json:"severity,omitempty"`
	CVSSScore    float64  `json:"cvss_score,omitempty"`
	FixedVersion string   `json:"fixed_version,omitempty"`
	References   []string `json:"references,omitempty"`
}

// VulnsResponse contains vulnerabilities for a package/version.
type VulnsResponse struct {
	Ecosystem       string         `json:"ecosystem"`
	Name            string         `json:"name"`
	Version         string         `json:"version,omitempty"`
	Vulnerabilities []VulnResponse `json:"vulnerabilities"`
	Count           int            `json:"count"`
}

// EnrichmentResponse contains full enrichment data.
type EnrichmentResponse struct {
	Package         *PackageResponse `json:"package,omitempty"`
	Version         *VersionResponse `json:"version,omitempty"`
	Vulnerabilities []VulnResponse   `json:"vulnerabilities,omitempty"`
	IsOutdated      bool             `json:"is_outdated"`
	LicenseCategory string           `json:"license_category"`
}

// OutdatedRequest is the request body for checking outdated packages.
type OutdatedRequest struct {
	Packages []OutdatedPackage `json:"packages"`
}

// OutdatedPackage represents a package to check for outdatedness.
type OutdatedPackage struct {
	Ecosystem string `json:"ecosystem"`
	Name      string `json:"name"`
	Version   string `json:"version"`
}

// OutdatedResponse contains outdated check results.
type OutdatedResponse struct {
	Results []OutdatedResult `json:"results"`
}

// OutdatedResult contains the outdated status for a package.
type OutdatedResult struct {
	Ecosystem     string `json:"ecosystem"`
	Name          string `json:"name"`
	Version       string `json:"version"`
	LatestVersion string `json:"latest_version,omitempty"`
	IsOutdated    bool   `json:"is_outdated"`
}

// BulkRequest is the request body for bulk package lookups.
type BulkRequest struct {
	PURLs []string `json:"purls"`
}

// BulkResponse contains bulk lookup results.
type BulkResponse struct {
	Packages map[string]*PackageResponse `json:"packages"`
}

// HandlePackagePath dispatches /api/package/{ecosystem}/* to the appropriate handler.
// Resolves namespaced package names (Composer vendor/name, npm @scope/name) from the path.
func (h *APIHandler) HandlePackagePath(w http.ResponseWriter, r *http.Request) {
	ecosystem := chi.URLParam(r, "ecosystem")
	wildcard := chi.URLParam(r, "*")
	segments := splitWildcardPath(wildcard)

	if ecosystem == "" || len(segments) == 0 {
		http.Error(w, "ecosystem and name are required", http.StatusBadRequest)
		return
	}

	// For the API, we don't have a DB to resolve names, so we use a heuristic:
	// the last segment that looks like a version (contains a digit) is the version,
	// everything before it is the name. If no version-like segment, it's all name.
	//
	// With 1 segment: package lookup (name only)
	// With 2+ segments: last segment is version, rest is name
	//   Exception: if this is a namespaced ecosystem and we have exactly 2 segments,
	//   it could be vendor/name with no version. The enrichment service handles
	//   both cases (it will try to look up the package either way).
	if len(segments) == 1 {
		h.getPackage(w, r, ecosystem, segments[0])
		return
	}

	// Try the full path as a package name first via enrichment.
	// If it resolves, this is a package-only lookup.
	fullName := strings.Join(segments, "/")
	info, err := h.enrichment.EnrichPackage(r.Context(), ecosystem, fullName)
	if err == nil && info != nil {
		resp := &PackageResponse{
			Ecosystem:       info.Ecosystem,
			Name:            info.Name,
			LatestVersion:   info.LatestVersion,
			License:         info.License,
			LicenseCategory: string(h.enrichment.CategorizeLicense(info.License)),
			Description:     info.Description,
			Homepage:        info.Homepage,
			Repository:      info.Repository,
			RegistryURL:     info.RegistryURL,
		}
		writeJSON(w, resp)
		return
	}

	// Otherwise, last segment is the version.
	name := strings.Join(segments[:len(segments)-1], "/")
	version := segments[len(segments)-1]
	h.getVersion(w, r, ecosystem, name, version)
}

func (h *APIHandler) getPackage(w http.ResponseWriter, r *http.Request, ecosystem, name string) {
	info, err := h.enrichment.EnrichPackage(r.Context(), ecosystem, name)
	if err != nil {
		http.Error(w, "failed to enrich package", http.StatusInternalServerError)
		return
	}

	if info == nil {
		http.Error(w, "package not found", http.StatusNotFound)
		return
	}

	resp := &PackageResponse{
		Ecosystem:       info.Ecosystem,
		Name:            info.Name,
		LatestVersion:   info.LatestVersion,
		License:         info.License,
		LicenseCategory: string(h.enrichment.CategorizeLicense(info.License)),
		Description:     info.Description,
		Homepage:        info.Homepage,
		Repository:      info.Repository,
		RegistryURL:     info.RegistryURL,
	}

	writeJSON(w, resp)
}

func (h *APIHandler) getVersion(w http.ResponseWriter, r *http.Request, ecosystem, name, version string) {
	result, err := h.enrichment.EnrichFull(r.Context(), ecosystem, name, version)
	if err != nil {
		http.Error(w, "failed to enrich version", http.StatusInternalServerError)
		return
	}

	resp := &EnrichmentResponse{
		IsOutdated:      result.IsOutdated,
		LicenseCategory: string(result.LicenseCategory),
	}

	if result.Package != nil {
		resp.Package = &PackageResponse{
			Ecosystem:       result.Package.Ecosystem,
			Name:            result.Package.Name,
			LatestVersion:   result.Package.LatestVersion,
			License:         result.Package.License,
			LicenseCategory: string(h.enrichment.CategorizeLicense(result.Package.License)),
			Description:     result.Package.Description,
			Homepage:        result.Package.Homepage,
			Repository:      result.Package.Repository,
			RegistryURL:     result.Package.RegistryURL,
		}
	}

	if result.Version != nil {
		resp.Version = &VersionResponse{
			Ecosystem:  ecosystem,
			Name:       name,
			Version:    result.Version.Number,
			License:    result.Version.License,
			Integrity:  result.Version.Integrity,
			Yanked:     result.Version.Yanked,
			IsOutdated: result.IsOutdated,
		}
		if !result.Version.PublishedAt.IsZero() {
			resp.Version.PublishedAt = result.Version.PublishedAt.Format("2006-01-02T15:04:05Z")
		}
	}

	for _, v := range result.Vulnerabilities {
		resp.Vulnerabilities = append(resp.Vulnerabilities, VulnResponse{
			ID:           v.ID,
			Summary:      v.Summary,
			Severity:     v.Severity,
			CVSSScore:    v.CVSSScore,
			FixedVersion: v.FixedVersion,
			References:   v.References,
		})
	}

	writeJSON(w, resp)
}

// HandleVulnsPath dispatches /api/vulns/{ecosystem}/* to the vulns handler.
// Supports both {name} and {name}/{version} paths with namespaced package names.
func (h *APIHandler) HandleVulnsPath(w http.ResponseWriter, r *http.Request) {
	ecosystem := chi.URLParam(r, "ecosystem")
	wildcard := chi.URLParam(r, "*")
	segments := splitWildcardPath(wildcard)

	if ecosystem == "" || len(segments) == 0 {
		http.Error(w, "ecosystem and name are required", http.StatusBadRequest)
		return
	}

	// Last segment could be a version. Try full path as name first,
	// then split off the last segment as version.
	name := strings.Join(segments, "/")
	version := "0"

	if len(segments) > 1 {
		// Try enrichment with the full path as name.
		// If it doesn't resolve, assume last segment is version.
		info, err := h.enrichment.EnrichPackage(r.Context(), ecosystem, name)
		if err != nil || info == nil {
			name = strings.Join(segments[:len(segments)-1], "/")
			version = segments[len(segments)-1]
		}
	}

	vulns, err := h.enrichment.CheckVulnerabilities(r.Context(), ecosystem, name, version)
	if err != nil {
		http.Error(w, "failed to check vulnerabilities", http.StatusInternalServerError)
		return
	}

	resp := &VulnsResponse{
		Ecosystem: ecosystem,
		Name:      name,
		Version:   version,
		Count:     len(vulns),
	}

	for _, v := range vulns {
		resp.Vulnerabilities = append(resp.Vulnerabilities, VulnResponse{
			ID:           v.ID,
			Summary:      v.Summary,
			Severity:     v.Severity,
			CVSSScore:    v.CVSSScore,
			FixedVersion: v.FixedVersion,
			References:   v.References,
		})
	}

	writeJSON(w, resp)
}

// HandleOutdated handles POST /api/outdated
// @Summary Check outdated packages
// @Tags api
// @Accept json
// @Produce json
// @Param request body OutdatedRequest true "Packages to check"
// @Success 200 {object} OutdatedResponse
// @Failure 400 {string} string
// @Failure 500 {string} string
// @Router /api/outdated [post]
func (h *APIHandler) HandleOutdated(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	var req OutdatedRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if len(req.Packages) == 0 {
		http.Error(w, "packages list is required", http.StatusBadRequest)
		return
	}

	resp := OutdatedResponse{
		Results: make([]OutdatedResult, 0, len(req.Packages)),
	}

	for _, pkg := range req.Packages {
		result := OutdatedResult{
			Ecosystem: pkg.Ecosystem,
			Name:      pkg.Name,
			Version:   pkg.Version,
		}

		latest, err := h.enrichment.GetLatestVersion(r.Context(), pkg.Ecosystem, pkg.Name)
		if err == nil && latest != "" {
			result.LatestVersion = latest
			result.IsOutdated = h.enrichment.IsOutdated(pkg.Version, latest)
		}

		resp.Results = append(resp.Results, result)
	}

	writeJSON(w, resp)
}

// HandleBulkLookup handles POST /api/bulk
// @Summary Bulk package lookup by PURL
// @Tags api
// @Accept json
// @Produce json
// @Param request body BulkRequest true "PURLs"
// @Success 200 {object} BulkResponse
// @Failure 400 {string} string
// @Failure 500 {string} string
// @Router /api/bulk [post]
func (h *APIHandler) HandleBulkLookup(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	var req BulkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if len(req.PURLs) == 0 {
		http.Error(w, "purls list is required", http.StatusBadRequest)
		return
	}

	resp := BulkResponse{
		Packages: make(map[string]*PackageResponse),
	}

	// Use ecosystems client for bulk lookup if available
	if h.ecosystems != nil {
		packages, err := h.ecosystems.BulkLookup(r.Context(), req.PURLs)
		if err == nil {
			for purl, info := range packages {
				if info != nil {
					resp.Packages[purl] = &PackageResponse{
						Ecosystem:       info.Ecosystem,
						Name:            info.Name,
						LatestVersion:   info.LatestVersion,
						License:         info.License,
						LicenseCategory: string(h.enrichment.CategorizeLicense(info.License)),
						Description:     info.Description,
						Homepage:        info.Homepage,
						Repository:      info.Repository,
						RegistryURL:     info.RegistryURL,
					}
				}
			}
		}
	} else {
		// Fall back to individual lookups via registries
		packages := make([]struct{ Ecosystem, Name string }, 0, len(req.PURLs))

		for _, purlStr := range req.PURLs {
			p, err := purl.Parse(purlStr)
			if err != nil {
				continue
			}
			ecosystem := purl.PURLTypeToEcosystem(p.Type)
			name := p.FullName()
			packages = append(packages, struct{ Ecosystem, Name string }{ecosystem, name})
		}

		results := h.enrichment.BulkEnrichPackages(r.Context(), packages)
		for purlStr, info := range results {
			if info != nil {
				resp.Packages[purlStr] = &PackageResponse{
					Ecosystem:       info.Ecosystem,
					Name:            info.Name,
					LatestVersion:   info.LatestVersion,
					License:         info.License,
					LicenseCategory: string(h.enrichment.CategorizeLicense(info.License)),
					Description:     info.Description,
					Homepage:        info.Homepage,
					Repository:      info.Repository,
					RegistryURL:     info.RegistryURL,
				}
			}
		}
	}

	writeJSON(w, resp)
}

// SearchResponse contains search results.
type SearchResponse struct {
	Results []SearchPackageResult `json:"results"`
	Query   string                `json:"query"`
	Count   int                   `json:"count"`
}

// SearchPackageResult represents a single search result.
type SearchPackageResult struct {
	Ecosystem     string `json:"ecosystem"`
	Name          string `json:"name"`
	LatestVersion string `json:"latest_version,omitempty"`
	License       string `json:"license,omitempty"`
	Hits          int64  `json:"hits"`
	Size          int64  `json:"size"`
	CachedAt      string `json:"cached_at,omitempty"`
}

// HandleSearch handles GET /api/search
// @Summary Search cached packages
// @Tags api
// @Produce json
// @Param q query string true "Query"
// @Param ecosystem query string false "Ecosystem"
// @Success 200 {object} SearchResponse
// @Failure 400 {string} string
// @Failure 500 {string} string
// @Router /api/search [get]
func (h *APIHandler) HandleSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	ecosystem := r.URL.Query().Get("ecosystem")

	if query == "" {
		http.Error(w, "query parameter 'q' is required", http.StatusBadRequest)
		return
	}

	page := 1
	limit := 50

	// Search in database
	results, err := h.db.SearchPackages(query, ecosystem, limit, (page-1)*limit)
	if err != nil {
		http.Error(w, "search failed", http.StatusInternalServerError)
		return
	}

	total, err := h.db.CountSearchResults(query, ecosystem)
	if err != nil {
		total = 0
	}

	resp := &SearchResponse{
		Query:   query,
		Count:   int(total),
		Results: make([]SearchPackageResult, 0, len(results)),
	}

	for _, result := range results {
		latestVersion := ""
		if result.LatestVersion.Valid {
			latestVersion = result.LatestVersion.String
		}
		license := ""
		if result.License.Valid {
			license = result.License.String
		}
		searchResult := SearchPackageResult{
			Ecosystem:     result.Ecosystem,
			Name:          result.Name,
			LatestVersion: latestVersion,
			License:       license,
			Hits:          result.Hits,
			Size:          result.Size,
		}
		if result.CachedAt.Valid {
			searchResult.CachedAt = result.CachedAt.String
		}
		resp.Results = append(resp.Results, searchResult)
	}

	writeJSON(w, resp)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, "failed to encode response", http.StatusInternalServerError)
	}
}

// PackagesListResponse contains a list of cached packages.
type PackagesListResponse struct {
	Results   []PackageListResult `json:"results"`
	Count     int                 `json:"count"`
	Total     int64               `json:"total"`
	Ecosystem string              `json:"ecosystem,omitempty"`
	SortBy    string              `json:"sort_by"`
	Page      int                 `json:"page"`
	PerPage   int                 `json:"per_page"`
}

// PackageListResult represents a single package in the list.
type PackageListResult struct {
	Ecosystem       string `json:"ecosystem"`
	Name            string `json:"name"`
	LatestVersion   string `json:"latest_version,omitempty"`
	License         string `json:"license,omitempty"`
	LicenseCategory string `json:"license_category,omitempty"`
	Hits            int64  `json:"hits"`
	Size            int64  `json:"size"`
	CachedAt        string `json:"cached_at,omitempty"`
	VulnCount       int64  `json:"vuln_count"`
}

// HandlePackagesList handles GET /api/packages
// @Summary List cached packages
// @Tags api
// @Produce json
// @Param ecosystem query string false "Ecosystem"
// @Param sort query string false "Sort" Enums(hits,name,size,cached_at,ecosystem,vulns)
// @Success 200 {object} PackagesListResponse
// @Failure 400 {string} string
// @Failure 500 {string} string
// @Router /api/packages [get]
func (h *APIHandler) HandlePackagesList(w http.ResponseWriter, r *http.Request) {
	ecosystem := r.URL.Query().Get("ecosystem")
	sortBy := r.URL.Query().Get("sort")
	if sortBy == "" {
		sortBy = defaultSortBy
	}

	validSorts := map[string]bool{
		defaultSortBy: true,
		"name":        true,
		"size":        true,
		"cached_at":   true,
		"ecosystem":   true,
		"vulns":       true,
	}
	if !validSorts[sortBy] {
		http.Error(w, "invalid sort parameter", http.StatusBadRequest)
		return
	}

	page := 1
	limit := 50

	packages, err := h.db.ListCachedPackages(ecosystem, sortBy, limit, (page-1)*limit)
	if err != nil {
		http.Error(w, "failed to list packages", http.StatusInternalServerError)
		return
	}

	total, err := h.db.CountCachedPackages(ecosystem)
	if err != nil {
		total = 0
	}

	resp := &PackagesListResponse{
		Results:   make([]PackageListResult, 0, len(packages)),
		Count:     len(packages),
		Total:     total,
		Ecosystem: ecosystem,
		SortBy:    sortBy,
		Page:      page,
		PerPage:   limit,
	}

	for _, pkg := range packages {
		latestVersion := ""
		if pkg.LatestVersion.Valid {
			latestVersion = pkg.LatestVersion.String
		}
		license := ""
		licenseCategory := licenseCategoryUnknown
		if pkg.License.Valid {
			license = pkg.License.String
			if h.enrichment != nil {
				licenseCategory = string(h.enrichment.CategorizeLicense(license))
			}
		}
		cachedAt := ""
		if pkg.CachedAt.Valid {
			cachedAt = pkg.CachedAt.String
		}

		resp.Results = append(resp.Results, PackageListResult{
			Ecosystem:       pkg.Ecosystem,
			Name:            pkg.Name,
			LatestVersion:   latestVersion,
			License:         license,
			LicenseCategory: licenseCategory,
			Hits:            pkg.Hits,
			Size:            pkg.Size,
			CachedAt:        cachedAt,
			VulnCount:       pkg.VulnCount,
		})
	}

	writeJSON(w, resp)
}
