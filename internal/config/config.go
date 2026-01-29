// Package config provides configuration loading and validation for the proxy server.
//
// Configuration can be provided via:
//   - Command line flags (highest priority)
//   - Environment variables (PROXY_ prefix)
//   - Configuration file (YAML or JSON)
//
// Storage Configuration:
//
// The proxy supports multiple storage backends via gocloud.dev/blob:
//
// Local filesystem (default):
//
//	storage:
//	  url: "file:///var/cache/proxy"
//
// Amazon S3:
//
//	storage:
//	  url: "s3://bucket-name"
//
// S3-compatible (MinIO, etc.):
//
//	storage:
//	  url: "s3://bucket?endpoint=http://localhost:9000"
//
// For S3, configure credentials via AWS environment variables:
//
//	AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_REGION
//
// Database Configuration:
//
// The proxy supports two database backends:
//
// SQLite (default):
//
//	database:
//	  driver: "sqlite"
//	  path: "/var/lib/proxy/cache.db"
//
// PostgreSQL:
//
//	database:
//	  driver: "postgres"
//	  url: "postgres://user:password@localhost:5432/proxy?sslmode=disable"
//
// See config.example.yaml in the repository root for a complete example.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config holds all configuration for the proxy server.
type Config struct {
	// Listen is the address to listen on (e.g., ":8080", "127.0.0.1:8080").
	Listen string `json:"listen" yaml:"listen"`

	// BaseURL is the public URL where this proxy is accessible.
	// Used for rewriting package metadata URLs.
	// Example: "https://proxy.example.com" or "http://localhost:8080"
	BaseURL string `json:"base_url" yaml:"base_url"`

	// Storage configures artifact storage.
	Storage StorageConfig `json:"storage" yaml:"storage"`

	// Database configures the cache database.
	Database DatabaseConfig `json:"database" yaml:"database"`

	// Log configures logging.
	Log LogConfig `json:"log" yaml:"log"`

	// Upstream configures upstream registry URLs (optional overrides).
	Upstream UpstreamConfig `json:"upstream" yaml:"upstream"`
}

// StorageConfig configures artifact storage.
type StorageConfig struct {
	// URL is the storage backend URL.
	// Supported schemes:
	//   - file:///path/to/dir - Local filesystem (default)
	//   - s3://bucket-name - Amazon S3
	//   - s3://bucket?endpoint=http://localhost:9000 - S3-compatible (MinIO)
	// If empty, defaults to file:// with the Path value.
	URL string `json:"url" yaml:"url"`

	// Path is the directory where cached artifacts are stored.
	// Deprecated: Use URL with file:// scheme instead.
	// If URL is empty, this is used as file://{Path}.
	Path string `json:"path" yaml:"path"`

	// MaxSize is the maximum cache size (e.g., "10GB", "500MB").
	// When exceeded, least recently used artifacts are evicted.
	// Empty or "0" means unlimited.
	MaxSize string `json:"max_size" yaml:"max_size"`
}

// DatabaseConfig configures the cache database.
type DatabaseConfig struct {
	// Driver is the database driver: "sqlite" or "postgres".
	Driver string `json:"driver" yaml:"driver"`

	// Path is the path to the SQLite database file.
	Path string `json:"path" yaml:"path"`

	// URL is the PostgreSQL connection string.
	URL string `json:"url" yaml:"url"`
}

// LogConfig configures logging.
type LogConfig struct {
	// Level is the minimum log level: "debug", "info", "warn", "error".
	Level string `json:"level" yaml:"level"`

	// Format is the log format: "text" or "json".
	Format string `json:"format" yaml:"format"`
}

// UpstreamConfig configures upstream registry URLs.
// Leave empty to use defaults.
type UpstreamConfig struct {
	// NPM is the upstream npm registry URL.
	// Default: https://registry.npmjs.org
	NPM string `json:"npm" yaml:"npm"`

	// Cargo is the upstream cargo index URL.
	// Default: https://index.crates.io
	Cargo string `json:"cargo" yaml:"cargo"`

	// CargoDownload is the upstream cargo download URL.
	// Default: https://static.crates.io/crates
	CargoDownload string `json:"cargo_download" yaml:"cargo_download"`
}

// Default returns a Config with sensible defaults.
func Default() *Config {
	return &Config{
		Listen:  ":8080",
		BaseURL: "http://localhost:8080",
		Storage: StorageConfig{
			Path:    "./cache/artifacts",
			MaxSize: "",
		},
		Database: DatabaseConfig{
			Driver: "sqlite",
			Path:   "./cache/proxy.db",
		},
		Log: LogConfig{
			Level:  "info",
			Format: "text",
		},
		Upstream: UpstreamConfig{
			NPM:           "https://registry.npmjs.org",
			Cargo:         "https://index.crates.io",
			CargoDownload: "https://static.crates.io/crates",
		},
	}
}

// Load reads configuration from a file (YAML or JSON).
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	cfg := Default()

	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".yaml", ".yml":
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parsing YAML config: %w", err)
		}
	case ".json":
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parsing JSON config: %w", err)
		}
	default:
		// Try YAML first, then JSON
		if err := yaml.Unmarshal(data, cfg); err != nil {
			if err := json.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("parsing config (tried YAML and JSON): %w", err)
			}
		}
	}

	return cfg, nil
}

// LoadFromEnv applies environment variable overrides to a Config.
// Environment variables use the PROXY_ prefix:
//   - PROXY_LISTEN
//   - PROXY_BASE_URL
//   - PROXY_STORAGE_PATH
//   - PROXY_STORAGE_MAX_SIZE
//   - PROXY_DATABASE_PATH
//   - PROXY_LOG_LEVEL
//   - PROXY_LOG_FORMAT
func (c *Config) LoadFromEnv() {
	if v := os.Getenv("PROXY_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("PROXY_BASE_URL"); v != "" {
		c.BaseURL = v
	}
	if v := os.Getenv("PROXY_STORAGE_URL"); v != "" {
		c.Storage.URL = v
	}
	if v := os.Getenv("PROXY_STORAGE_PATH"); v != "" {
		c.Storage.Path = v
	}
	if v := os.Getenv("PROXY_STORAGE_MAX_SIZE"); v != "" {
		c.Storage.MaxSize = v
	}
	if v := os.Getenv("PROXY_DATABASE_DRIVER"); v != "" {
		c.Database.Driver = v
	}
	if v := os.Getenv("PROXY_DATABASE_PATH"); v != "" {
		c.Database.Path = v
	}
	if v := os.Getenv("PROXY_DATABASE_URL"); v != "" {
		c.Database.URL = v
	}
	if v := os.Getenv("PROXY_LOG_LEVEL"); v != "" {
		c.Log.Level = v
	}
	if v := os.Getenv("PROXY_LOG_FORMAT"); v != "" {
		c.Log.Format = v
	}
}

// Validate checks the configuration for errors.
func (c *Config) Validate() error {
	if c.Listen == "" {
		return fmt.Errorf("listen address is required")
	}
	if c.BaseURL == "" {
		return fmt.Errorf("base_url is required")
	}
	if c.Storage.URL == "" && c.Storage.Path == "" {
		return fmt.Errorf("storage.url or storage.path is required")
	}
	switch c.Database.Driver {
	case "sqlite":
		if c.Database.Path == "" {
			return fmt.Errorf("database.path is required for sqlite driver")
		}
	case "postgres":
		if c.Database.URL == "" {
			return fmt.Errorf("database.url is required for postgres driver")
		}
	default:
		return fmt.Errorf("invalid database.driver %q (must be sqlite or postgres)", c.Database.Driver)
	}

	// Validate log level
	switch strings.ToLower(c.Log.Level) {
	case "debug", "info", "warn", "error":
		// OK
	default:
		return fmt.Errorf("invalid log level %q (must be debug, info, warn, or error)", c.Log.Level)
	}

	// Validate log format
	switch strings.ToLower(c.Log.Format) {
	case "text", "json":
		// OK
	default:
		return fmt.Errorf("invalid log format %q (must be text or json)", c.Log.Format)
	}

	// Validate max size if specified
	if c.Storage.MaxSize != "" {
		if _, err := ParseSize(c.Storage.MaxSize); err != nil {
			return fmt.Errorf("invalid storage.max_size: %w", err)
		}
	}

	return nil
}

// ParseSize parses a human-readable size string (e.g., "10GB", "500MB").
// Returns the size in bytes.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" || s == "0" {
		return 0, nil
	}

	// Check suffixes in order of length (longest first) to avoid partial matches
	suffixes := []struct {
		suffix string
		mult   int64
	}{
		{"TB", 1024 * 1024 * 1024 * 1024},
		{"GB", 1024 * 1024 * 1024},
		{"MB", 1024 * 1024},
		{"KB", 1024},
		{"T", 1024 * 1024 * 1024 * 1024},
		{"G", 1024 * 1024 * 1024},
		{"M", 1024 * 1024},
		{"K", 1024},
		{"B", 1},
	}

	for _, s2 := range suffixes {
		if strings.HasSuffix(s, s2.suffix) {
			numStr := strings.TrimSuffix(s, s2.suffix)
			num, err := strconv.ParseFloat(numStr, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid number %q", numStr)
			}
			return int64(num * float64(s2.mult)), nil
		}
	}

	// Try parsing as plain number (bytes)
	num, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return num, nil
}
