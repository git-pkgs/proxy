// Command proxy runs the git-pkgs package registry proxy server.
//
// @title git-pkgs proxy API
// @version 0.1.0
// @description HTTP API for package enrichment, vulnerability lookup, cache stats, and source browsing.
// @BasePath /
//
// The proxy caches package artifacts from upstream registries (npm, cargo, etc.)
// providing faster, more reliable access for development teams.
//
// Usage:
//
//	proxy [command] [flags]
//
// Commands:
//
//	serve    Start the proxy server (default if no command given)
//	stats    Show cache statistics
//	mirror   Pre-populate cache from PURLs, SBOMs, or registries
//
// Serve Flags:
//
//	-config string
//	      Path to configuration file (YAML or JSON)
//	-listen string
//	      Address to listen on (default ":8080")
//	-base-url string
//	      Public URL of this proxy (default "http://localhost:8080")
//	-storage-url string
//	      Storage URL (file:// or s3://)
//	-storage-path string
//	      Path to artifact storage directory (deprecated, use -storage-url)
//	-database-driver string
//	      Database driver: sqlite or postgres (default "sqlite")
//	-database-path string
//	      Path to SQLite database file (default "./cache/proxy.db")
//	-database-url string
//	      PostgreSQL connection URL
//	-log-level string
//	      Log level: debug, info, warn, error (default "info")
//	-log-format string
//	      Log format: text, json (default "text")
//
// Stats Flags:
//
//	-database-driver string
//	      Database driver: sqlite or postgres (default "sqlite")
//	-database-path string
//	      Path to SQLite database file (default "./cache/proxy.db")
//	-database-url string
//	      PostgreSQL connection URL
//	-json
//	      Output as JSON
//	-popular int
//	      Show top N most popular packages (default 10)
//	-recent int
//	      Show N recently cached packages (default 10)
//
// Global Flags:
//
//	-version
//	      Print version and exit
//
// Environment Variables:
//
//	PROXY_LISTEN           - Listen address
//	PROXY_BASE_URL         - Public URL
//	PROXY_STORAGE_URL      - Storage URL (file:// or s3://)
//	PROXY_STORAGE_PATH     - Storage directory (deprecated)
//	PROXY_DATABASE_DRIVER  - Database driver (sqlite or postgres)
//	PROXY_DATABASE_PATH    - SQLite database file path
//	PROXY_DATABASE_URL     - PostgreSQL connection URL
//	PROXY_LOG_LEVEL        - Log level
//	PROXY_LOG_FORMAT       - Log format
//
// Example:
//
//	# Start with defaults
//	proxy
//
//	# Start with custom settings
//	proxy serve -listen :3000 -base-url https://proxy.example.com
//
//	# Show cache statistics
//	proxy stats
//
//	# Show stats as JSON
//	proxy stats -json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/mirror"
	"github.com/git-pkgs/proxy/internal/server"
	"github.com/git-pkgs/proxy/internal/storage"
	"github.com/git-pkgs/registries/fetch"
)

const defaultTopN = 10

var (
	// Version is set at build time.
	Version = "dev"

	// Commit is set at build time.
	Commit = "unknown"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve":
			os.Args = append(os.Args[:1], os.Args[2:]...)
			runServe()
			return
		case "stats":
			os.Args = append(os.Args[:1], os.Args[2:]...)
			runStats()
			return
		case "mirror":
			os.Args = append(os.Args[:1], os.Args[2:]...)
			runMirror()
			return
		case "-version", "--version":
			fmt.Printf("proxy %s (%s)\n", Version, Commit)
			os.Exit(0)
		case "-h", "-help", "--help":
			printUsage()
			os.Exit(0)
		}
	}

	// Default to serve
	runServe()
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `git-pkgs proxy - Package registry caching proxy

Usage: proxy [command] [flags]

Commands:
  serve    Start the proxy server (default)
  stats    Show cache statistics
  mirror   Pre-populate cache from PURLs, SBOMs, or registries

Run 'proxy <command> -help' for more information on a command.

Global Flags:
  -version   Print version and exit
  -help      Show this help message
`)
}

func runServe() {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := fs.String("config", "", "Path to configuration file (YAML or JSON)")
	listen := fs.String("listen", "", "Address to listen on")
	baseURL := fs.String("base-url", "", "Public URL of this proxy")
	storageURL := fs.String("storage-url", "", "Storage URL (file:// or s3://)")
	storagePath := fs.String("storage-path", "", "Path to artifact storage directory (deprecated, use -storage-url)")
	databaseDriver := fs.String("database-driver", "", "Database driver: sqlite or postgres")
	databasePath := fs.String("database-path", "", "Path to SQLite database file")
	databaseURL := fs.String("database-url", "", "PostgreSQL connection URL")
	logLevel := fs.String("log-level", "", "Log level: debug, info, warn, error")
	logFormat := fs.String("log-format", "", "Log format: text, json")
	version := fs.Bool("version", false, "Print version and exit")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "git-pkgs proxy - Package registry caching proxy\n\n")
		fmt.Fprintf(os.Stderr, "Usage: proxy serve [flags]\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fs.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nEnvironment Variables:\n")
		fmt.Fprintf(os.Stderr, "  PROXY_LISTEN           Listen address\n")
		fmt.Fprintf(os.Stderr, "  PROXY_BASE_URL         Public URL\n")
		fmt.Fprintf(os.Stderr, "  PROXY_STORAGE_URL      Storage URL (file:// or s3://)\n")
		fmt.Fprintf(os.Stderr, "  PROXY_STORAGE_PATH     Storage directory (deprecated)\n")
		fmt.Fprintf(os.Stderr, "  PROXY_DATABASE_DRIVER  Database driver (sqlite or postgres)\n")
		fmt.Fprintf(os.Stderr, "  PROXY_DATABASE_PATH    SQLite database file\n")
		fmt.Fprintf(os.Stderr, "  PROXY_DATABASE_URL     PostgreSQL connection URL\n")
		fmt.Fprintf(os.Stderr, "  PROXY_LOG_LEVEL        Log level\n")
		fmt.Fprintf(os.Stderr, "  PROXY_LOG_FORMAT       Log format\n")
	}

	_ = fs.Parse(os.Args[1:])

	if *version {
		fmt.Printf("proxy %s (%s)\n", Version, Commit)
		os.Exit(0)
	}

	// Load configuration
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading config: %v\n", err)
		os.Exit(1)
	}

	// Apply environment variables
	cfg.LoadFromEnv()

	// Apply command line flags (highest priority)
	if *listen != "" {
		cfg.Listen = *listen
	}
	if *baseURL != "" {
		cfg.BaseURL = *baseURL
	}
	if *storageURL != "" {
		cfg.Storage.URL = *storageURL
	}
	if *storagePath != "" {
		cfg.Storage.Path = *storagePath //nolint:staticcheck // backwards compat
	}
	if *databaseDriver != "" {
		cfg.Database.Driver = *databaseDriver
	}
	if *databasePath != "" {
		cfg.Database.Path = *databasePath
	}
	if *databaseURL != "" {
		cfg.Database.URL = *databaseURL
	}
	if *logLevel != "" {
		cfg.Log.Level = *logLevel
	}
	if *logFormat != "" {
		cfg.Log.Format = *logFormat
	}

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "invalid configuration: %v\n", err)
		os.Exit(1)
	}

	// Setup logger
	logger := setupLogger(cfg.Log.Level, cfg.Log.Format)

	// Create and start server
	srv, err := server.New(cfg, logger)
	if err != nil {
		logger.Error("failed to create server", "error", err)
		os.Exit(1)
	}

	// Handle shutdown signals
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		logger.Info("received shutdown signal")
		cancel()
	}()

	// Start server in goroutine
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Start()
	}()

	// Wait for shutdown or error
	select {
	case <-ctx.Done():
		cancel()
		if err := srv.Shutdown(context.Background()); err != nil {
			logger.Error("shutdown error", "error", err)
		}
	case err := <-errCh:
		cancel()
		if err != nil {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}
}

func runStats() {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	databaseDriver := fs.String("database-driver", "sqlite", "Database driver: sqlite or postgres")
	databasePath := fs.String("database-path", "./cache/proxy.db", "Path to SQLite database file")
	databaseURL := fs.String("database-url", "", "PostgreSQL connection URL")
	asJSON := fs.Bool("json", false, "Output as JSON")
	popular := fs.Int("popular", defaultTopN, "Show top N most popular packages")
	recent := fs.Int("recent", defaultTopN, "Show N recently cached packages")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "git-pkgs proxy - Show cache statistics\n\n")
		fmt.Fprintf(os.Stderr, "Usage: proxy stats [flags]\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fs.PrintDefaults()
	}

	_ = fs.Parse(os.Args[1:])

	// Apply environment overrides
	if v := os.Getenv("PROXY_DATABASE_DRIVER"); v != "" {
		*databaseDriver = v
	}
	if v := os.Getenv("PROXY_DATABASE_PATH"); v != "" {
		*databasePath = v
	}
	if v := os.Getenv("PROXY_DATABASE_URL"); v != "" {
		*databaseURL = v
	}

	// Open database
	var db *database.DB
	var err error

	switch *databaseDriver {
	case "postgres":
		if *databaseURL == "" {
			fmt.Fprintf(os.Stderr, "database-url is required for postgres driver\n")
			os.Exit(1)
		}
		db, err = database.OpenPostgres(*databaseURL)
	default:
		if _, statErr := os.Stat(*databasePath); os.IsNotExist(statErr) {
			fmt.Fprintf(os.Stderr, "database not found: %s\n", *databasePath)
			fmt.Fprintf(os.Stderr, "run 'proxy serve' first to create the database\n")
			os.Exit(1)
		}
		db, err = database.Open(*databasePath)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "error opening database: %v\n", err)
		os.Exit(1)
	}

	if err := printStats(db, *popular, *recent, *asJSON); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func runMirror() {
	fs := flag.NewFlagSet("mirror", flag.ExitOnError)
	configPath := fs.String("config", "", "Path to configuration file")
	storageURL := fs.String("storage-url", "", "Storage URL (file:// or s3://)")
	databaseDriver := fs.String("database-driver", "", "Database driver: sqlite or postgres")
	databasePath := fs.String("database-path", "", "Path to SQLite database file")
	databaseURL := fs.String("database-url", "", "PostgreSQL connection URL")
	sbomPath := fs.String("sbom", "", "Path to CycloneDX or SPDX SBOM file")
	concurrency := fs.Int("concurrency", 4, "Number of parallel downloads") //nolint:mnd // default concurrency
	dryRun := fs.Bool("dry-run", false, "Show what would be mirrored without downloading")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "git-pkgs proxy - Pre-populate cache\n\n")
		fmt.Fprintf(os.Stderr, "Usage: proxy mirror [flags] [purl...]\n\n")
		fmt.Fprintf(os.Stderr, "Examples:\n")
		fmt.Fprintf(os.Stderr, "  proxy mirror pkg:npm/lodash@4.17.21\n")
		fmt.Fprintf(os.Stderr, "  proxy mirror --sbom sbom.cdx.json\n")
		fmt.Fprintf(os.Stderr, "  proxy mirror pkg:npm/lodash  # all versions\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fs.PrintDefaults()
	}

	_ = fs.Parse(os.Args[1:])
	purls := fs.Args()

	// Determine source
	var source mirror.Source
	switch {
	case *sbomPath != "":
		source = &mirror.SBOMSource{Path: *sbomPath}
	case len(purls) > 0:
		source = &mirror.PURLSource{PURLs: purls}
	default:
		fmt.Fprintf(os.Stderr, "error: provide PURLs or --sbom\n")
		fs.Usage()
		os.Exit(1)
	}

	// Load config
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading config: %v\n", err)
		os.Exit(1)
	}
	cfg.LoadFromEnv()

	if *storageURL != "" {
		cfg.Storage.URL = *storageURL
	}
	if *databaseDriver != "" {
		cfg.Database.Driver = *databaseDriver
	}
	if *databasePath != "" {
		cfg.Database.Path = *databasePath
	}
	if *databaseURL != "" {
		cfg.Database.URL = *databaseURL
	}

	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "invalid configuration: %v\n", err)
		os.Exit(1)
	}

	logger := setupLogger("info", "text")

	// Open database
	var db *database.DB
	switch cfg.Database.Driver {
	case "postgres":
		db, err = database.OpenPostgresOrCreate(cfg.Database.URL)
	default:
		db, err = database.OpenOrCreate(cfg.Database.Path)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error opening database: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()

	if err := db.MigrateSchema(); err != nil {
		_ = db.Close()
		fmt.Fprintf(os.Stderr, "error migrating schema: %v\n", err)
		os.Exit(1) //nolint:gocritic // db closed above
	}

	// Open storage
	sURL := cfg.Storage.URL
	if sURL == "" {
		sURL = "file://" + cfg.Storage.Path //nolint:staticcheck // backwards compat
	}
	store, err := storage.OpenBucket(context.Background(), sURL)
	if err != nil {
		_ = db.Close()
		fmt.Fprintf(os.Stderr, "error opening storage: %v\n", err)
		os.Exit(1) //nolint:gocritic // db closed above
	}

	// Build proxy (reuses same pipeline as serve)
	fetcher := fetch.NewFetcher()
	resolver := fetch.NewResolver()
	proxy := handler.NewProxy(db, store, fetcher, resolver, logger)
	proxy.CacheMetadata = true // mirror always caches metadata

	m := mirror.New(proxy, db, store, logger, *concurrency)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		cancel()
	}()

	if *dryRun {
		items, err := m.RunDryRun(ctx, source)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Would mirror %d package versions:\n", len(items))
		for _, item := range items {
			fmt.Printf("  %s\n", item)
		}
		return
	}

	progress, err := m.Run(ctx, source)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Mirror complete: %d downloaded, %d skipped (cached), %d failed, %s total\n",
		progress.Completed, progress.Skipped, progress.Failed, formatSize(progress.Bytes))

	if len(progress.Errors) > 0 {
		fmt.Fprintf(os.Stderr, "\nErrors:\n")
		for _, e := range progress.Errors {
			fmt.Fprintf(os.Stderr, "  %s/%s@%s: %s\n", e.Ecosystem, e.Name, e.Version, e.Error)
		}
	}
}

func printStats(db *database.DB, popular, recent int, asJSON bool) error {
	defer func() { _ = db.Close() }()

	stats, err := db.GetCacheStats()
	if err != nil {
		return fmt.Errorf("error getting stats: %w", err)
	}

	popularPkgs, err := db.GetMostPopularPackages(popular)
	if err != nil {
		return fmt.Errorf("error getting popular packages: %w", err)
	}

	recentPkgs, err := db.GetRecentlyCachedPackages(recent)
	if err != nil {
		return fmt.Errorf("error getting recent packages: %w", err)
	}

	if asJSON {
		outputJSON(stats, popularPkgs, recentPkgs)
	} else {
		outputText(stats, popularPkgs, recentPkgs)
	}
	return nil
}

type jsonOutput struct {
	Packages   int64            `json:"packages"`
	Versions   int64            `json:"versions"`
	Artifacts  int64            `json:"artifacts"`
	TotalSize  int64            `json:"total_size_bytes"`
	TotalHits  int64            `json:"total_hits"`
	Ecosystems map[string]int64 `json:"ecosystems"`
	Popular    []jsonPopular    `json:"popular"`
	Recent     []jsonRecent     `json:"recent"`
}

type jsonPopular struct {
	Ecosystem string `json:"ecosystem"`
	Name      string `json:"name"`
	Hits      int64  `json:"hits"`
	Size      int64  `json:"size_bytes"`
}

type jsonRecent struct {
	Ecosystem string `json:"ecosystem"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	CachedAt  string `json:"cached_at"`
	Size      int64  `json:"size_bytes"`
}

func outputJSON(stats *database.CacheStats, popular []database.PopularPackage, recent []database.RecentPackage) {
	out := jsonOutput{
		Packages:   stats.TotalPackages,
		Versions:   stats.TotalVersions,
		Artifacts:  stats.TotalArtifacts,
		TotalSize:  stats.TotalSize,
		TotalHits:  stats.TotalHits,
		Ecosystems: stats.EcosystemCounts,
		Popular:    make([]jsonPopular, len(popular)),
		Recent:     make([]jsonRecent, len(recent)),
	}

	for i, p := range popular {
		out.Popular[i] = jsonPopular{
			Ecosystem: p.Ecosystem,
			Name:      p.Name,
			Hits:      p.Hits,
			Size:      p.Size,
		}
	}

	for i, r := range recent {
		out.Recent[i] = jsonRecent{
			Ecosystem: r.Ecosystem,
			Name:      r.Name,
			Version:   r.Version,
			CachedAt:  r.CachedAt.Format("2006-01-02 15:04:05"),
			Size:      r.Size,
		}
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

func outputText(stats *database.CacheStats, popular []database.PopularPackage, recent []database.RecentPackage) {
	fmt.Printf("Cache Statistics\n")
	fmt.Printf("================\n\n")

	fmt.Printf("Packages:   %d\n", stats.TotalPackages)
	fmt.Printf("Versions:   %d\n", stats.TotalVersions)
	fmt.Printf("Artifacts:  %d\n", stats.TotalArtifacts)
	fmt.Printf("Total size: %s\n", formatSize(stats.TotalSize))
	fmt.Printf("Total hits: %d\n", stats.TotalHits)

	if len(stats.EcosystemCounts) > 0 {
		fmt.Printf("\nPackages by ecosystem:\n")
		for eco, count := range stats.EcosystemCounts {
			fmt.Printf("  %-10s %d\n", eco, count)
		}
	}

	if len(popular) > 0 {
		fmt.Printf("\nMost popular packages:\n")
		for i, p := range popular {
			fmt.Printf("  %2d. %s/%s (%d hits, %s)\n", i+1, p.Ecosystem, p.Name, p.Hits, formatSize(p.Size))
		}
	}

	if len(recent) > 0 {
		fmt.Printf("\nRecently cached:\n")
		for _, r := range recent {
			fmt.Printf("  %s/%s@%s (%s, %s)\n", r.Ecosystem, r.Name, r.Version, r.CachedAt.Format("2006-01-02 15:04"), formatSize(r.Size))
		}
	}
}

func formatSize(bytes int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
		TB = GB * 1024
	)

	switch {
	case bytes >= TB:
		return fmt.Sprintf("%.1f TB", float64(bytes)/TB)
	case bytes >= GB:
		return fmt.Sprintf("%.1f GB", float64(bytes)/GB)
	case bytes >= MB:
		return fmt.Sprintf("%.1f MB", float64(bytes)/MB)
	case bytes >= KB:
		return fmt.Sprintf("%.1f KB", float64(bytes)/KB)
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

func loadConfig(path string) (*config.Config, error) {
	if path != "" {
		return config.Load(path)
	}
	return config.Default(), nil
}

func setupLogger(level, format string) *slog.Logger {
	var handler slog.Handler

	opts := &slog.HandlerOptions{
		Level: parseLogLevel(level),
	}

	switch strings.ToLower(format) {
	case "json":
		handler = slog.NewJSONHandler(os.Stderr, opts)
	default:
		handler = slog.NewTextHandler(os.Stderr, opts)
	}

	return slog.New(handler)
}

func parseLogLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
