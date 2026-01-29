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
//
// Additional endpoints:
//   - /health    - Health check endpoint
//   - /stats     - Cache statistics (JSON)
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
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/storage"
	"github.com/git-pkgs/proxy/internal/upstream"
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

	// Health and stats endpoints
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /stats", s.handleStats)
	mux.HandleFunc("GET /{$}", s.handleRoot)

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
		Registries: getRegistryConfigs(s.cfg.BaseURL),
	}

	for _, p := range popular {
		data.PopularPackages = append(data.PopularPackages, PackageInfo{
			Ecosystem: p.Ecosystem,
			Name:      p.Name,
			Hits:      p.Hits,
			Size:      formatSize(p.Size),
		})
	}

	for _, p := range recent {
		data.RecentPackages = append(data.RecentPackages, PackageInfo{
			Ecosystem: p.Ecosystem,
			Name:      p.Name,
			Version:   p.Version,
			Size:      formatSize(p.Size),
			CachedAt:  formatTimeAgo(p.CachedAt),
		})
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
