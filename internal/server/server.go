// Package server provides the HTTP server and router for the proxy.
//
// The server mounts protocol handlers at their respective paths:
//   - /npm/*      - npm registry protocol
//   - /cargo/*    - Cargo registry protocol (sparse index)
//   - /gem/*      - RubyGems registry protocol
//   - /go/*       - Go module proxy protocol
//   - /hex/*      - Hex.pm registry protocol
//   - /pub/*      - pub.dev registry protocol
//   - /pypi/*     - PyPI registry protocol
//   - /maven/*    - Maven repository protocol
//   - /nuget/*    - NuGet V3 API protocol
//   - /composer/* - Composer/Packagist protocol
//   - /conan/*    - Conan C/C++ protocol
//   - /conda/*    - Conda/Anaconda protocol
//   - /cran/*     - CRAN (R) protocol
//   - /v2/*       - OCI/Docker container registry protocol
//   - /debian/*   - Debian/APT repository protocol
//   - /rpm/*      - RPM/Yum repository protocol
//
// Additional endpoints:
//   - /health    - Health check endpoint
//   - /stats     - Cache statistics (JSON)
//   - /packages  - List all cached packages (HTML)
//   - /search    - Search packages (HTML)
//
// API endpoints for enrichment data:
//   - GET  /api/package/{ecosystem}/{name}          - Package metadata
//   - GET  /api/package/{ecosystem}/{name}/{version} - Version metadata with vulns
//   - GET  /api/vulns/{ecosystem}/{name}            - Package vulnerabilities
//   - GET  /api/vulns/{ecosystem}/{name}/{version}  - Version vulnerabilities
//   - POST /api/outdated                            - Check outdated packages
//   - POST /api/bulk                                - Bulk package lookup
//   - GET  /api/packages                            - List cached packages (JSON)
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/enrichment"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/metrics"
	"github.com/git-pkgs/proxy/internal/storage"
	"github.com/git-pkgs/proxy/internal/upstream"
	"github.com/git-pkgs/spdx"
)

// Server is the main proxy server.
type Server struct {
	cfg       *config.Config
	db        *database.DB
	storage   storage.Storage
	logger    *slog.Logger
	http      *http.Server
	templates *Templates
}

// New creates a new Server with the given configuration.
func New(cfg *config.Config, logger *slog.Logger) (*Server, error) {
	// Initialize database
	var db *database.DB
	var err error

	switch cfg.Database.Driver {
	case "postgres":
		db, err = database.OpenPostgresOrCreate(cfg.Database.URL)
	default:
		db, err = database.OpenOrCreate(cfg.Database.Path)
	}
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	// Run schema migration to add missing columns
	if err := db.MigrateSchema(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrating database schema: %w", err)
	}

	// Initialize storage
	storageURL := cfg.Storage.URL
	if storageURL == "" {
		// Fall back to file:// with Path
		storageURL = "file://" + cfg.Storage.Path
	}
	store, err := storage.OpenBucket(context.Background(), storageURL)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initializing storage: %w", err)
	}

	// Load templates
	templates, err := NewTemplates()
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("loading templates: %w", err)
	}

	return &Server{
		cfg:       cfg,
		db:        db,
		storage:   store,
		logger:    logger,
		templates: templates,
	}, nil
}

// Start starts the HTTP server.
func (s *Server) Start() error {
	// Create shared components with circuit breaker
	baseFetcher := upstream.New(upstream.WithAuthFunc(s.authForURL))
	fetcher := upstream.NewCircuitBreakerFetcher(baseFetcher)
	resolver := upstream.NewResolver()
	proxy := handler.NewProxy(s.db, s.storage, fetcher, resolver, s.logger)

	// Create router with Chi
	r := chi.NewRouter()

	// Add middleware
	r.Use(middleware.RequestID)
	r.Use(RequestIDMiddleware)
	r.Use(middleware.RealIP)
	r.Use(s.LoggerMiddleware)
	r.Use(middleware.Recoverer)
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/metrics" {
				metrics.IncrementActiveRequests()
				defer metrics.DecrementActiveRequests()
			}
			next.ServeHTTP(w, r)
		})
	})

	// Mount protocol handlers
	npmHandler := handler.NewNPMHandler(proxy, s.cfg.BaseURL)
	cargoHandler := handler.NewCargoHandler(proxy, s.cfg.BaseURL)
	gemHandler := handler.NewGemHandler(proxy, s.cfg.BaseURL)
	goHandler := handler.NewGoHandler(proxy, s.cfg.BaseURL)
	hexHandler := handler.NewHexHandler(proxy, s.cfg.BaseURL)
	pubHandler := handler.NewPubHandler(proxy, s.cfg.BaseURL)
	pypiHandler := handler.NewPyPIHandler(proxy, s.cfg.BaseURL)
	mavenHandler := handler.NewMavenHandler(proxy, s.cfg.BaseURL)
	nugetHandler := handler.NewNuGetHandler(proxy, s.cfg.BaseURL)
	composerHandler := handler.NewComposerHandler(proxy, s.cfg.BaseURL)
	conanHandler := handler.NewConanHandler(proxy, s.cfg.BaseURL)
	condaHandler := handler.NewCondaHandler(proxy, s.cfg.BaseURL)
	cranHandler := handler.NewCRANHandler(proxy, s.cfg.BaseURL)
	containerHandler := handler.NewContainerHandler(proxy, s.cfg.BaseURL)
	debianHandler := handler.NewDebianHandler(proxy, s.cfg.BaseURL)
	rpmHandler := handler.NewRPMHandler(proxy, s.cfg.BaseURL)

	r.Mount("/npm", http.StripPrefix("/npm", npmHandler.Routes()))
	r.Mount("/cargo", http.StripPrefix("/cargo", cargoHandler.Routes()))
	r.Mount("/gem", http.StripPrefix("/gem", gemHandler.Routes()))
	r.Mount("/go", http.StripPrefix("/go", goHandler.Routes()))
	r.Mount("/hex", http.StripPrefix("/hex", hexHandler.Routes()))
	r.Mount("/pub", http.StripPrefix("/pub", pubHandler.Routes()))
	r.Mount("/pypi", http.StripPrefix("/pypi", pypiHandler.Routes()))
	r.Mount("/maven", http.StripPrefix("/maven", mavenHandler.Routes()))
	r.Mount("/nuget", http.StripPrefix("/nuget", nugetHandler.Routes()))
	r.Mount("/composer", http.StripPrefix("/composer", composerHandler.Routes()))
	r.Mount("/conan", http.StripPrefix("/conan", conanHandler.Routes()))
	r.Mount("/conda", http.StripPrefix("/conda", condaHandler.Routes()))
	r.Mount("/cran", http.StripPrefix("/cran", cranHandler.Routes()))
	r.Mount("/v2", http.StripPrefix("/v2", containerHandler.Routes()))
	r.Mount("/debian", http.StripPrefix("/debian", debianHandler.Routes()))
	r.Mount("/rpm", http.StripPrefix("/rpm", rpmHandler.Routes()))

	// Health, stats, and static endpoints
	r.Get("/health", s.handleHealth)
	r.Get("/stats", s.handleStats)
	r.Get("/metrics", func(w http.ResponseWriter, r *http.Request) {
		metrics.Handler().ServeHTTP(w, r)
	})
	r.Mount("/static", http.StripPrefix("/static/", staticHandler()))
	r.Get("/", s.handleRoot)
	r.Get("/install", s.handleInstall)
	r.Get("/search", s.handleSearch)
	r.Get("/packages", s.handlePackagesList)
	r.Get("/package/{ecosystem}/{name}", s.handlePackageShow)
	r.Get("/package/{ecosystem}/{name}/{version}", s.handleVersionShow)
	r.Get("/package/{ecosystem}/{name}/{version}/browse", s.handleBrowseSource)

	// API endpoints for enrichment data
	enrichSvc := enrichment.New(s.logger)
	apiHandler := NewAPIHandler(enrichSvc, s.db)

	r.Get("/api/package/{ecosystem}/{name}", apiHandler.HandleGetPackage)
	r.Get("/api/package/{ecosystem}/{name}/{version}", apiHandler.HandleGetVersion)
	r.Get("/api/vulns/{ecosystem}/{name}", apiHandler.HandleGetVulns)
	r.Get("/api/vulns/{ecosystem}/{name}/{version}", apiHandler.HandleGetVulns)
	r.Post("/api/outdated", apiHandler.HandleOutdated)
	r.Post("/api/bulk", apiHandler.HandleBulkLookup)
	r.Get("/api/search", apiHandler.HandleSearch)
	r.Get("/api/packages", apiHandler.HandlePackagesList)

	// Archive browsing endpoints
	r.Get("/api/browse/{ecosystem}/{name}/{version}", s.handleBrowseList)
	r.Get("/api/browse/{ecosystem}/{name}/{version}/file/*", s.handleBrowseFile)

	// Version comparison endpoints
	r.Get("/api/compare/{ecosystem}/{name}/{fromVersion}/{toVersion}", s.handleCompareDiff)
	r.Get("/package/{ecosystem}/{name}/compare/{versions}", s.handleComparePage)

	s.http = &http.Server{
		Addr:         s.cfg.Listen,
		Handler:      r,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute, // Large artifacts need time
		IdleTimeout:  60 * time.Second,
	}

	s.logger.Info("starting server",
		"listen", s.cfg.Listen,
		"base_url", s.cfg.BaseURL,
		"storage", s.cfg.Storage.Path,
		"database", s.cfg.Database.Path)

	// Start background goroutine to update cache stats metrics
	go s.updateCacheStatsMetrics()

	return s.http.ListenAndServe()
}

// updateCacheStatsMetrics periodically updates cache statistics in Prometheus metrics.
func (s *Server) updateCacheStatsMetrics() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	// Update once immediately
	s.updateCacheStats()

	for range ticker.C {
		s.updateCacheStats()
	}
}

func (s *Server) updateCacheStats() {
	stats, err := s.db.GetCacheStats()
	if err != nil {
		s.logger.Warn("failed to get cache stats for metrics", "error", err)
		return
	}
	metrics.UpdateCacheStats(stats.TotalSize, stats.TotalArtifacts)
}

// Shutdown gracefully shuts down the server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("shutting down server")

	var errs []error

	if s.http != nil {
		if err := s.http.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("http shutdown: %w", err))
		}
	}

	if s.db != nil {
		if err := s.db.Close(); err != nil {
			errs = append(errs, fmt.Errorf("database close: %w", err))
		}
	}

	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

// authForURL returns the authentication header for a given URL based on config.
func (s *Server) authForURL(url string) (headerName, headerValue string) {
	auth := s.cfg.Upstream.AuthForURL(url)
	if auth == nil {
		return "", ""
	}
	return auth.Header()
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	// Get cache statistics
	stats, err := s.db.GetCacheStats()
	if err != nil {
		s.logger.Error("failed to get cache stats", "error", err)
		stats = &database.CacheStats{}
	}

	// Get enrichment statistics
	enrichStats, err := s.db.GetEnrichmentStats()
	if err != nil {
		s.logger.Error("failed to get enrichment stats", "error", err)
		enrichStats = &database.EnrichmentStats{}
	}

	// Get popular packages
	popular, err := s.db.GetMostPopularPackages(10)
	if err != nil {
		s.logger.Error("failed to get popular packages", "error", err)
	}

	// Get recent packages
	recent, err := s.db.GetRecentlyCachedPackages(10)
	if err != nil {
		s.logger.Error("failed to get recent packages", "error", err)
	}

	// Build dashboard data
	data := DashboardData{
		Stats: DashboardStats{
			CachedArtifacts: stats.TotalArtifacts,
			TotalSize:       formatSize(stats.TotalSize),
			TotalPackages:   stats.TotalPackages,
			TotalVersions:   stats.TotalVersions,
		},
		EnrichmentStats: EnrichmentStatsView{
			EnrichedPackages:     enrichStats.EnrichedPackages,
			VulnSyncedPackages:   enrichStats.VulnSyncedPackages,
			TotalVulnerabilities: enrichStats.TotalVulnerabilities,
			CriticalVulns:        enrichStats.CriticalVulns,
			HighVulns:            enrichStats.HighVulns,
			MediumVulns:          enrichStats.MediumVulns,
			LowVulns:             enrichStats.LowVulns,
			HasVulns:             enrichStats.TotalVulnerabilities > 0,
		},
	}

	for _, p := range popular {
		pkgInfo := PackageInfo{
			Ecosystem: p.Ecosystem,
			Name:      p.Name,
			Hits:      p.Hits,
			Size:      formatSize(p.Size),
		}

		// Fetch enrichment data for this package
		if pkg, err := s.db.GetPackageByEcosystemName(p.Ecosystem, p.Name); err == nil && pkg != nil {
			if pkg.License.Valid {
				pkgInfo.License = pkg.License.String
				pkgInfo.LicenseCategory = categorizeLicenseCSS(pkg.License.String)
			}
			if pkg.LatestVersion.Valid {
				pkgInfo.LatestVersion = pkg.LatestVersion.String
			}
		}

		// Get vulnerability count
		if vulnCount, err := s.db.GetVulnCountForPackage(p.Ecosystem, p.Name); err == nil {
			pkgInfo.VulnCount = vulnCount
		}

		data.PopularPackages = append(data.PopularPackages, pkgInfo)
	}

	for _, p := range recent {
		pkgInfo := PackageInfo{
			Ecosystem: p.Ecosystem,
			Name:      p.Name,
			Version:   p.Version,
			Size:      formatSize(p.Size),
			CachedAt:  formatTimeAgo(p.CachedAt),
		}

		// Fetch enrichment data for this package
		if pkg, err := s.db.GetPackageByEcosystemName(p.Ecosystem, p.Name); err == nil && pkg != nil {
			if pkg.License.Valid {
				pkgInfo.License = pkg.License.String
				pkgInfo.LicenseCategory = categorizeLicenseCSS(pkg.License.String)
			}
			if pkg.LatestVersion.Valid {
				pkgInfo.LatestVersion = pkg.LatestVersion.String
				pkgInfo.IsOutdated = p.Version != "" && pkg.LatestVersion.String != "" && p.Version != pkg.LatestVersion.String
			}
		}

		// Get vulnerability count
		if vulnCount, err := s.db.GetVulnCountForPackage(p.Ecosystem, p.Name); err == nil {
			pkgInfo.VulnCount = vulnCount
		}

		data.RecentPackages = append(data.RecentPackages, pkgInfo)
	}

	if err := s.templates.Render(w, "dashboard", data); err != nil {
		s.logger.Error("failed to render dashboard", "error", err)
	}
}

func (s *Server) handleInstall(w http.ResponseWriter, r *http.Request) {
	data := struct {
		Registries []RegistryConfig
	}{
		Registries: getRegistryConfigs(s.cfg.BaseURL),
	}

	if err := s.templates.Render(w, "install", data); err != nil {
		s.logger.Error("failed to render install page", "error", err)
	}
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	ecosystem := r.URL.Query().Get("ecosystem")

	if query == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	page := 1
	if pageStr := r.URL.Query().Get("page"); pageStr != "" {
		if p, err := strconv.Atoi(pageStr); err == nil && p > 0 {
			page = p
		}
	}
	limit := 50

	results, err := s.db.SearchPackages(query, ecosystem, limit, (page-1)*limit)
	if err != nil {
		s.logger.Error("search failed", "error", err)
		http.Error(w, "search failed", http.StatusInternalServerError)
		return
	}

	total, err := s.db.CountSearchResults(query, ecosystem)
	if err != nil {
		s.logger.Error("failed to count search results", "error", err)
		total = 0
	}

	items := make([]SearchResultItem, len(results))
	for i, result := range results {
		latestVersion := ""
		if result.LatestVersion.Valid {
			latestVersion = result.LatestVersion.String
		}
		license := ""
		if result.License.Valid {
			license = result.License.String
		}
		items[i] = SearchResultItem{
			Ecosystem:     result.Ecosystem,
			Name:          result.Name,
			LatestVersion: latestVersion,
			License:       license,
			Hits:          result.Hits,
			Size:          result.Size,
			SizeFormatted: formatSize(result.Size),
		}
	}

	totalPages := int((total + int64(limit) - 1) / int64(limit))

	data := SearchPageData{
		Query:      query,
		Ecosystem:  ecosystem,
		Results:    items,
		Count:      int(total),
		Page:       page,
		PerPage:    limit,
		TotalPages: totalPages,
	}

	if err := s.templates.Render(w, "search", data); err != nil {
		s.logger.Error("failed to render search page", "error", err)
	}
}

func (s *Server) handlePackagesList(w http.ResponseWriter, r *http.Request) {
	ecosystem := r.URL.Query().Get("ecosystem")
	sortBy := r.URL.Query().Get("sort")
	if sortBy == "" {
		sortBy = "hits"
	}

	page := 1
	if pageStr := r.URL.Query().Get("page"); pageStr != "" {
		if p, err := strconv.Atoi(pageStr); err == nil && p > 0 {
			page = p
		}
	}
	limit := 50

	packages, err := s.db.ListCachedPackages(ecosystem, sortBy, limit, (page-1)*limit)
	if err != nil {
		s.logger.Error("failed to list packages", "error", err)
		http.Error(w, "failed to list packages", http.StatusInternalServerError)
		return
	}

	total, err := s.db.CountCachedPackages(ecosystem)
	if err != nil {
		s.logger.Error("failed to count packages", "error", err)
		total = 0
	}

	items := make([]SearchResultItem, len(packages))
	for i, pkg := range packages {
		latestVersion := ""
		if pkg.LatestVersion.Valid {
			latestVersion = pkg.LatestVersion.String
		}
		license := ""
		if pkg.License.Valid {
			license = pkg.License.String
		}
		cachedAt := ""
		if pkg.CachedAt.Valid && pkg.CachedAt.String != "" {
			if t, err := time.Parse("2006-01-02 15:04:05.999999999-07:00", pkg.CachedAt.String); err == nil {
				cachedAt = formatTimeAgo(t)
			}
		}
		items[i] = SearchResultItem{
			Ecosystem:      pkg.Ecosystem,
			Name:           pkg.Name,
			LatestVersion:  latestVersion,
			License:        license,
			LicenseCategory: categorizeLicenseCSS(license),
			Hits:           pkg.Hits,
			Size:           pkg.Size,
			SizeFormatted:  formatSize(pkg.Size),
			CachedAt:       cachedAt,
			VulnCount:      pkg.VulnCount,
		}
	}

	totalPages := int((total + int64(limit) - 1) / int64(limit))

	data := PackagesListPageData{
		Ecosystem:  ecosystem,
		SortBy:     sortBy,
		Results:    items,
		Count:      int(total),
		Page:       page,
		PerPage:    limit,
		TotalPages: totalPages,
	}

	if err := s.templates.Render(w, "packages_list", data); err != nil {
		s.logger.Error("failed to render packages list page", "error", err)
	}
}

func (s *Server) handlePackageShow(w http.ResponseWriter, r *http.Request) {
	ecosystem := chi.URLParam(r, "ecosystem")
	name := chi.URLParam(r, "name")

	if ecosystem == "" || name == "" {
		http.Error(w, "ecosystem and name required", http.StatusBadRequest)
		return
	}

	pkg, err := s.db.GetPackageByEcosystemName(ecosystem, name)
	if err != nil {
		s.logger.Error("failed to get package", "error", err, "ecosystem", ecosystem, "name", name)
		http.Error(w, "package not found", http.StatusNotFound)
		return
	}
	if pkg == nil {
		http.Error(w, "package not found", http.StatusNotFound)
		return
	}

	versions, err := s.db.GetVersionsByPackagePURL(pkg.PURL)
	if err != nil {
		s.logger.Error("failed to get versions", "error", err)
		versions = []database.Version{}
	}

	vulns, err := s.db.GetVulnerabilitiesForPackage(ecosystem, name)
	if err != nil {
		s.logger.Error("failed to get vulnerabilities", "error", err)
		vulns = []database.Vulnerability{}
	}

	data := PackageShowData{
		Package:         pkg,
		Versions:        versions,
		Vulnerabilities: vulns,
		LicenseCategory: categorizeLicense(pkg.License),
	}

	if err := s.templates.Render(w, "package_show", data); err != nil {
		s.logger.Error("failed to render package show", "error", err)
	}
}

func (s *Server) handleVersionShow(w http.ResponseWriter, r *http.Request) {
	ecosystem := chi.URLParam(r, "ecosystem")
	name := chi.URLParam(r, "name")
	version := chi.URLParam(r, "version")

	if ecosystem == "" || name == "" || version == "" {
		http.Error(w, "ecosystem, name, and version required", http.StatusBadRequest)
		return
	}

	pkg, err := s.db.GetPackageByEcosystemName(ecosystem, name)
	if err != nil || pkg == nil {
		s.logger.Error("failed to get package", "error", err)
		http.Error(w, "package not found", http.StatusNotFound)
		return
	}

	purl := fmt.Sprintf("pkg:%s/%s@%s", ecosystem, name, version)
	ver, err := s.db.GetVersionByPURL(purl)
	if err != nil || ver == nil {
		s.logger.Error("failed to get version", "error", err)
		http.Error(w, "version not found", http.StatusNotFound)
		return
	}

	artifacts, err := s.db.GetArtifactsByVersionPURL(purl)
	if err != nil {
		s.logger.Error("failed to get artifacts", "error", err)
		artifacts = []database.Artifact{}
	}

	vulns, err := s.db.GetVulnerabilitiesForPackage(ecosystem, name)
	if err != nil {
		s.logger.Error("failed to get vulnerabilities", "error", err)
		vulns = []database.Vulnerability{}
	}

	isOutdated := false
	if pkg.LatestVersion.Valid && pkg.LatestVersion.String != version {
		isOutdated = true
	}

	// Check if any artifact is cached
	hasCached := false
	for _, art := range artifacts {
		if art.StoragePath.Valid {
			hasCached = true
			break
		}
	}

	data := VersionShowData{
		Package:           pkg,
		Version:           ver,
		Artifacts:         artifacts,
		Vulnerabilities:   vulns,
		IsOutdated:        isOutdated,
		LicenseCategory:   categorizeLicense(ver.License),
		HasCachedArtifact: hasCached,
	}

	if err := s.templates.Render(w, "version_show", data); err != nil {
		s.logger.Error("failed to render version show", "error", err)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// Check database connectivity
	if _, err := s.db.SchemaVersion(); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintf(w, "database error: %v", err)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, "ok")
}

// StatsResponse contains cache statistics.
type StatsResponse struct {
	CachedArtifacts int64  `json:"cached_artifacts"`
	TotalSize       int64  `json:"total_size_bytes"`
	TotalSizeHuman  string `json:"total_size"`
	StoragePath     string `json:"storage_path"`
	DatabasePath    string `json:"database_path"`
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	count, err := s.db.GetCachedArtifactCount()
	if err != nil {
		http.Error(w, "failed to get artifact count", http.StatusInternalServerError)
		return
	}

	size, err := s.db.GetTotalCacheSize()
	if err != nil {
		http.Error(w, "failed to get cache size", http.StatusInternalServerError)
		return
	}

	_ = ctx // Could use for storage.UsedSpace if needed

	stats := StatsResponse{
		CachedArtifacts: count,
		TotalSize:       size,
		TotalSizeHuman:  formatSize(size),
		StoragePath:     s.cfg.Storage.Path,
		DatabasePath:    s.cfg.Database.Path,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(stats)
}

func formatSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

func formatTimeAgo(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			return "1 min ago"
		}
		return fmt.Sprintf("%d mins ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", h)
	case d < 7*24*time.Hour:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	default:
		return t.Format("Jan 2")
	}
}

// categorizeLicenseCSS returns the CSS class suffix for a license category using the spdx module.
func categorizeLicenseCSS(license string) string {
	if license == "" {
		return "unknown"
	}

	if spdx.HasCopyleft(license) {
		return "copyleft"
	}

	if spdx.IsFullyPermissive(license) {
		return "permissive"
	}

	return "unknown"
}

// categorizeLicense is a helper that handles sql.NullString.
func categorizeLicense(license sql.NullString) string {
	if !license.Valid {
		return "unknown"
	}
	return categorizeLicenseCSS(license.String)
}

// responseWriter wraps http.ResponseWriter to capture status code.
type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

// chiLogger is a middleware that logs requests using Chi's middleware pattern.
func (s *Server) chiLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)

		s.logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration", time.Since(start).String(),
			"remote", r.RemoteAddr)
	})
}
