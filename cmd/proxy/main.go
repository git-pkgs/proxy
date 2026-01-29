// Command proxy runs the git-pkgs package registry proxy server.
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
//
// Serve Flags:
//
//	-config string
//	      Path to configuration file (YAML or JSON)
//	-listen string
//	      Address to listen on (default ":8080")
//	-base-url string
//	      Public URL of this proxy (default "http://localhost:8080")
//	-storage string
//	      Path to artifact storage directory (default "./cache/artifacts")
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
//	PROXY_STORAGE_PATH     - Storage directory
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
	"github.com/git-pkgs/proxy/internal/server"
)

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
	storagePath := fs.String("storage", "", "Path to artifact storage directory")
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
		fmt.Fprintf(os.Stderr, "  PROXY_STORAGE_PATH     Storage directory\n")
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
	if *storagePath != "" {
		cfg.Storage.Path = *storagePath
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
	defer cancel()

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
		if err := srv.Shutdown(context.Background()); err != nil {
			logger.Error("shutdown error", "error", err)
		}
	case err := <-errCh:
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
	popular := fs.Int("popular", 10, "Show top N most popular packages")
	recent := fs.Int("recent", 10, "Show N recently cached packages")

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
	defer func() { _ = db.Close() }()

	// Get stats
	stats, err := db.GetCacheStats()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error getting stats: %v\n", err)
		os.Exit(1)
	}

	popularPkgs, err := db.GetMostPopularPackages(*popular)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error getting popular packages: %v\n", err)
		os.Exit(1)
	}

	recentPkgs, err := db.GetRecentlyCachedPackages(*recent)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error getting recent packages: %v\n", err)
		os.Exit(1)
	}

	if *asJSON {
		outputJSON(stats, popularPkgs, recentPkgs)
	} else {
		outputText(stats, popularPkgs, recentPkgs)
	}
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
