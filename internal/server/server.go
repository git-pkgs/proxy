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
//
// API endpoints for enrichment data:
//   - GET  /api/package/{ecosystem}/{name}          - Package metadata
//   - GET  /api/package/{ecosystem}/{name}/{version} - Version metadata with vulns
//   - GET  /api/vulns/{ecosystem}/{name}            - Package vulnerabilities
//   - GET  /api/vulns/{ecosystem}/{name}/{version}  - Version vulnerabilities
//   - POST /api/outdated                            - Check outdated packages
//   - POST /api/bulk                                - Bulk package lookup
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/enrichment"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/storage"
	"github.com/git-pkgs/proxy/internal/upstream"
	"github.com/git-pkgs/spdx"
)

// Server is the main proxy server.
type Server struct {
	cfg     *config.Config
	db      *database.DB
	storage storage.Storage
	logger  *slog.Logger
	http    *http.Server
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

	return &Server{
		cfg:     cfg,
		db:      db,
		storage: store,
		logger:  logger,
	}, nil
}

// Start starts the HTTP server.
func (s *Server) Start() error {
	// Create shared components
	fetcher := upstream.New(upstream.WithAuthFunc(s.authForURL))
	resolver := upstream.NewResolver()
	proxy := handler.NewProxy(s.db, s.storage, fetcher, resolver, s.logger)

	// Create router
	mux := http.NewServeMux()

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

	mux.Handle("GET /npm/{path...}", http.StripPrefix("/npm", npmHandler.Routes()))
	mux.Handle("GET /cargo/{path...}", http.StripPrefix("/cargo", cargoHandler.Routes()))
	mux.Handle("GET /gem/{path...}", http.StripPrefix("/gem", gemHandler.Routes()))
	mux.Handle("GET /go/{path...}", http.StripPrefix("/go", goHandler.Routes()))
	mux.Handle("GET /hex/{path...}", http.StripPrefix("/hex", hexHandler.Routes()))
	mux.Handle("GET /pub/{path...}", http.StripPrefix("/pub", pubHandler.Routes()))
	mux.Handle("GET /pypi/{path...}", http.StripPrefix("/pypi", pypiHandler.Routes()))
	mux.Handle("GET /maven/{path...}", http.StripPrefix("/maven", mavenHandler.Routes()))
	mux.Handle("GET /nuget/{path...}", http.StripPrefix("/nuget", nugetHandler.Routes()))
	mux.Handle("GET /composer/{path...}", http.StripPrefix("/composer", composerHandler.Routes()))
	mux.Handle("GET /conan/{path...}", http.StripPrefix("/conan", conanHandler.Routes()))
	mux.Handle("GET /conda/{path...}", http.StripPrefix("/conda", condaHandler.Routes()))
	mux.Handle("GET /cran/{path...}", http.StripPrefix("/cran", cranHandler.Routes()))
	mux.Handle("GET /v2/{path...}", http.StripPrefix("/v2", containerHandler.Routes()))
	mux.Handle("HEAD /v2/{path...}", http.StripPrefix("/v2", containerHandler.Routes()))
	mux.Handle("GET /debian/{path...}", http.StripPrefix("/debian", debianHandler.Routes()))
	mux.Handle("HEAD /debian/{path...}", http.StripPrefix("/debian", debianHandler.Routes()))
	mux.Handle("GET /rpm/{path...}", http.StripPrefix("/rpm", rpmHandler.Routes()))
	mux.Handle("HEAD /rpm/{path...}", http.StripPrefix("/rpm", rpmHandler.Routes()))

	// Health, stats, and static endpoints
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /stats", s.handleStats)
	mux.Handle("GET /static/", http.StripPrefix("/static/", staticHandler()))
	mux.HandleFunc("GET /{$}", s.handleRoot)

	// API endpoints for enrichment data
	enrichSvc := enrichment.New(s.logger)
	apiHandler := NewAPIHandler(enrichSvc)

	mux.HandleFunc("GET /api/package/{ecosystem}/{name}", apiHandler.HandleGetPackage)
	mux.HandleFunc("GET /api/package/{ecosystem}/{name}/{version}", apiHandler.HandleGetVersion)
	mux.HandleFunc("GET /api/vulns/{ecosystem}/{name}", apiHandler.HandleGetVulns)
	mux.HandleFunc("GET /api/vulns/{ecosystem}/{name}/{version}", apiHandler.HandleGetVulns)
	mux.HandleFunc("POST /api/outdated", apiHandler.HandleOutdated)
	mux.HandleFunc("POST /api/bulk", apiHandler.HandleBulkLookup)

	// Wrap with logging middleware
	handler := s.loggingMiddleware(mux)

	s.http = &http.Server{
		Addr:         s.cfg.Listen,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute, // Large artifacts need time
		IdleTimeout:  60 * time.Second,
	}

	s.logger.Info("starting server",
		"listen", s.cfg.Listen,
		"base_url", s.cfg.BaseURL,
		"storage", s.cfg.Storage.Path,
		"database", s.cfg.Database.Path)

	return s.http.ListenAndServe()
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
		Registries: getRegistryConfigs(s.cfg.BaseURL),
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

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTemplate.Execute(w, data); err != nil {
		s.logger.Error("failed to render dashboard", "error", err)
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

// responseWriter wraps http.ResponseWriter to capture status code.
type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
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
