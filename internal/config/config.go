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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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

	// Cooldown configures version age filtering to mitigate supply chain attacks.
	Cooldown CooldownConfig `json:"cooldown" yaml:"cooldown"`

	// CacheMetadata enables caching of upstream metadata responses for offline fallback.
	// When enabled, metadata is stored in the database and storage backend.
	// The mirror command always enables this regardless of this setting.
	CacheMetadata bool `json:"cache_metadata" yaml:"cache_metadata"`

	// MetadataTTL is how long cached metadata is considered fresh before
	// revalidating with upstream. Uses Go duration syntax (e.g. "5m", "1h").
	// Default: "5m". Set to "0" to always revalidate.
	MetadataTTL string `json:"metadata_ttl" yaml:"metadata_ttl"`

	// MirrorAPI enables the /api/mirror endpoints for starting mirror jobs via HTTP.
	// Disabled by default to prevent unauthenticated users from triggering downloads.
	MirrorAPI bool `json:"mirror_api" yaml:"mirror_api"`

	// Gradle configures Gradle HttpBuildCache behavior.
	Gradle GradleConfig `json:"gradle" yaml:"gradle"`

	// Health configures the /health endpoint behavior.
	Health HealthConfig `json:"health" yaml:"health"`
}

// CooldownConfig configures version cooldown periods.
// Versions published more recently than the cooldown are hidden from metadata responses.
type CooldownConfig struct {
	// Default is the global default cooldown (e.g., "3d", "48h", "0" to disable).
	Default string `json:"default" yaml:"default"`

	// Ecosystems overrides the default for specific ecosystems.
	Ecosystems map[string]string `json:"ecosystems" yaml:"ecosystems"`

	// Packages overrides the cooldown for specific packages (keyed by PURL).
	Packages map[string]string `json:"packages" yaml:"packages"`
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
	// If URL is empty, this is used as file://{Path}.
	//
	// Deprecated: Use URL with file:// scheme instead.
	Path string `json:"path" yaml:"path"`

	// MaxSize is the maximum cache size (e.g., "10GB", "500MB").
	// When exceeded, least recently used artifacts are evicted.
	// Empty or "0" means unlimited.
	MaxSize string `json:"max_size" yaml:"max_size"`

	// DirectServe enables redirecting cached artifact downloads to presigned
	// storage URLs (HTTP 302) instead of streaming bytes through the proxy.
	// Only effective for backends that support URL signing (S3, Azure).
	DirectServe bool `json:"direct_serve" yaml:"direct_serve"`

	// DirectServeTTL is how long presigned URLs remain valid.
	// Uses Go duration syntax (e.g. "5m", "1h"). Default: "15m".
	DirectServeTTL string `json:"direct_serve_ttl" yaml:"direct_serve_ttl"`

	// DirectServeBaseURL overrides the scheme and host of presigned URLs
	// before returning them to clients. Useful when the proxy reaches
	// storage at an internal address (e.g. 127.0.0.1 or a Docker hostname)
	// but clients must use a public one.
	DirectServeBaseURL string `json:"direct_serve_base_url" yaml:"direct_serve_base_url"`
}

// GradleConfig configures Gradle-specific features.
type GradleConfig struct {
	// BuildCache configures the /gradle HttpBuildCache endpoint.
	BuildCache GradleBuildCacheConfig `json:"build_cache" yaml:"build_cache"`
}

// GradleBuildCacheConfig configures Gradle HttpBuildCache safeguards.
type GradleBuildCacheConfig struct {
	// ReadOnly disables PUT uploads and keeps cache reads (GET/HEAD) enabled.
	ReadOnly bool `json:"read_only" yaml:"read_only"`

	// MaxUploadSize caps a single PUT body size (e.g., "100MB"). Must be > 0.
	// Default: "100MB".
	MaxUploadSize string `json:"max_upload_size" yaml:"max_upload_size"`

	// MaxAge evicts entries older than this duration (e.g., "24h", "7d").
	// Empty or "0" disables age-based eviction.
	MaxAge string `json:"max_age" yaml:"max_age"`

	// MaxSize evicts oldest entries until total Gradle cache size is <= MaxSize.
	// Empty or "0" disables size-based eviction.
	MaxSize string `json:"max_size" yaml:"max_size"`

	// SweepInterval controls periodic eviction frequency.
	// Default: "10m".
	SweepInterval string `json:"sweep_interval" yaml:"sweep_interval"`
}

// HealthConfig configures the /health endpoint.
type HealthConfig struct {
	// StorageProbeInterval is the minimum time between storage backend probes.
	// Uses Go duration syntax (e.g. "30s", "1m"). Default: "30s".
	// Set to "0" to probe on every /health request (useful for low-traffic deployments).
	StorageProbeInterval string `json:"storage_probe_interval" yaml:"storage_probe_interval"`
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

// UpstreamConfig configures upstream registry URLs and authentication.
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

	// Auth configures authentication for upstream registries.
	// Keys are URL prefixes that are matched against request URLs.
	// Example: "https://npm.pkg.github.com" matches all requests to that host.
	Auth map[string]AuthConfig `json:"auth" yaml:"auth"`
}

// AuthForURL returns the auth config that matches the given URL.
// Matches are based on URL prefix - the longest matching prefix wins.
func (u *UpstreamConfig) AuthForURL(url string) *AuthConfig {
	if u.Auth == nil {
		return nil
	}

	var bestMatch *AuthConfig
	var bestLen int

	for pattern, auth := range u.Auth {
		if strings.HasPrefix(url, pattern) && len(pattern) > bestLen {
			a := auth // copy to avoid loop variable capture
			bestMatch = &a
			bestLen = len(pattern)
		}
	}

	return bestMatch
}

// AuthConfig configures authentication for an upstream registry.
type AuthConfig struct {
	// Type is the authentication type: "bearer", "basic", or "header".
	Type string `json:"type" yaml:"type"`

	// Token is used for bearer authentication.
	// Can reference environment variables with ${VAR_NAME} syntax.
	Token string `json:"token" yaml:"token"`

	// Username is used for basic authentication.
	Username string `json:"username" yaml:"username"`

	// Password is used for basic authentication.
	// Can reference environment variables with ${VAR_NAME} syntax.
	Password string `json:"password" yaml:"password"`

	// HeaderName is the custom header name (for type "header").
	HeaderName string `json:"header_name" yaml:"header_name"`

	// HeaderValue is the custom header value (for type "header").
	// Can reference environment variables with ${VAR_NAME} syntax.
	HeaderValue string `json:"header_value" yaml:"header_value"`
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
		Gradle: GradleConfig{
			BuildCache: GradleBuildCacheConfig{
				ReadOnly:      false,
				MaxUploadSize: defaultGradleMaxUploadSizeStr,
				MaxAge:        "168h",
				MaxSize:       "",
				SweepInterval: defaultGradleSweepIntervalStr,
			},
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
//   - PROXY_HEALTH_STORAGE_PROBE_INTERVAL
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
	if v := os.Getenv("PROXY_STORAGE_DIRECT_SERVE"); v != "" {
		c.Storage.DirectServe = envBool(v)
	}
	if v := os.Getenv("PROXY_STORAGE_DIRECT_SERVE_TTL"); v != "" {
		c.Storage.DirectServeTTL = v
	}
	if v := os.Getenv("PROXY_STORAGE_DIRECT_SERVE_BASE_URL"); v != "" {
		c.Storage.DirectServeBaseURL = v
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
	if v := os.Getenv("PROXY_COOLDOWN_DEFAULT"); v != "" {
		c.Cooldown.Default = v
	}
	if v := os.Getenv("PROXY_CACHE_METADATA"); v != "" {
		c.CacheMetadata = envBool(v)
	}
	if v := os.Getenv("PROXY_MIRROR_API"); v != "" {
		c.MirrorAPI = envBool(v)
	}
	if v := os.Getenv("PROXY_METADATA_TTL"); v != "" {
		c.MetadataTTL = v
	}
	if v := os.Getenv("PROXY_GRADLE_BUILD_CACHE_READ_ONLY"); v != "" {
		c.Gradle.BuildCache.ReadOnly = v == "true" || v == "1"
	}
	if v := os.Getenv("PROXY_GRADLE_BUILD_CACHE_MAX_UPLOAD_SIZE"); v != "" {
		c.Gradle.BuildCache.MaxUploadSize = v
	}
	if v := os.Getenv("PROXY_GRADLE_BUILD_CACHE_MAX_AGE"); v != "" {
		c.Gradle.BuildCache.MaxAge = v
	}
	if v := os.Getenv("PROXY_GRADLE_BUILD_CACHE_MAX_SIZE"); v != "" {
		c.Gradle.BuildCache.MaxSize = v
	}
	if v := os.Getenv("PROXY_GRADLE_BUILD_CACHE_SWEEP_INTERVAL"); v != "" {
		c.Gradle.BuildCache.SweepInterval = v
	}
	if v := os.Getenv("PROXY_HEALTH_STORAGE_PROBE_INTERVAL"); v != "" {
		c.Health.StorageProbeInterval = v
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

	// Validate direct serve TTL if specified
	if c.Storage.DirectServeTTL != "" {
		if _, err := time.ParseDuration(c.Storage.DirectServeTTL); err != nil {
			return fmt.Errorf("invalid storage.direct_serve_ttl %q: %w", c.Storage.DirectServeTTL, err)
		}
	}

	// Validate direct serve base URL if specified
	if c.Storage.DirectServeBaseURL != "" {
		u, err := url.Parse(c.Storage.DirectServeBaseURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("invalid storage.direct_serve_base_url %q: must be an absolute URL", c.Storage.DirectServeBaseURL)
		}
	}

	// Validate metadata TTL if specified
	if c.MetadataTTL != "" && c.MetadataTTL != "0" {
		if _, err := time.ParseDuration(c.MetadataTTL); err != nil {
			return fmt.Errorf("invalid metadata_ttl %q: %w", c.MetadataTTL, err)
		}
	}

	if err := c.Gradle.BuildCache.Validate(); err != nil {
		return err
	}

	return nil
}

// Validate checks Gradle build cache settings, applying the default upload
// size if unset.
func (g *GradleBuildCacheConfig) Validate() error {
	if g.MaxUploadSize == "" {
		g.MaxUploadSize = defaultGradleMaxUploadSizeStr
	}
	uploadSize, err := ParseSize(g.MaxUploadSize)
	if err != nil {
		return fmt.Errorf("invalid gradle.build_cache.max_upload_size: %w", err)
	}
	if uploadSize <= 0 {
		return fmt.Errorf("invalid gradle.build_cache.max_upload_size %q: must be > 0", g.MaxUploadSize)
	}

	if g.MaxAge != "" && g.MaxAge != "0" {
		if _, err := time.ParseDuration(g.MaxAge); err != nil {
			return fmt.Errorf("invalid gradle.build_cache.max_age %q: %w", g.MaxAge, err)
		}
	}

	if g.MaxSize != "" {
		if _, err := ParseSize(g.MaxSize); err != nil {
			return fmt.Errorf("invalid gradle.build_cache.max_size: %w", err)
		}
	}

	if g.SweepInterval != "" {
		d, err := time.ParseDuration(g.SweepInterval)
		if err != nil {
			return fmt.Errorf("invalid gradle.build_cache.sweep_interval %q: %w", g.SweepInterval, err)
		}
		if d <= 0 {
			return fmt.Errorf("invalid gradle.build_cache.sweep_interval %q: must be > 0", g.SweepInterval)
		}
	}

	return nil
}

const (
	defaultMetadataTTL                   = 5 * time.Minute  //nolint:mnd // sensible default
	defaultDirectServeTTL                = 15 * time.Minute //nolint:mnd // sensible default
	defaultGradleBuildCacheMaxUploadSize = 100 << 20
	defaultGradleBuildCacheSweepInterval = 10 * time.Minute
	defaultGradleMaxUploadSizeStr        = "100MB"
	defaultGradleSweepIntervalStr        = "10m"
)

// ParseMaxSize returns the maximum cache size in bytes.
// Returns 0 if unset or explicitly disabled (meaning unlimited).
func (c *Config) ParseMaxSize() int64 {
	if c.Storage.MaxSize == "" || c.Storage.MaxSize == "0" {
		return 0
	}
	size, err := ParseSize(c.Storage.MaxSize)
	if err != nil {
		return 0
	}
	return size
}

// ParseMetadataTTL returns the metadata TTL duration.
// Returns 5 minutes if unset, 0 if explicitly disabled.
func (c *Config) ParseMetadataTTL() time.Duration {
	if c.MetadataTTL == "" {
		return defaultMetadataTTL
	}
	if c.MetadataTTL == "0" {
		return 0
	}
	d, err := time.ParseDuration(c.MetadataTTL)
	if err != nil {
		return defaultMetadataTTL
	}
	return d
}

// ParseGradleBuildCacheMaxUploadSize returns the max accepted PUT body size.
// Defaults to 100MB if unset or invalid.
func (c *Config) ParseGradleBuildCacheMaxUploadSize() int64 {
	if c.Gradle.BuildCache.MaxUploadSize == "" {
		return defaultGradleBuildCacheMaxUploadSize
	}
	size, err := ParseSize(c.Gradle.BuildCache.MaxUploadSize)
	if err != nil || size <= 0 {
		return defaultGradleBuildCacheMaxUploadSize
	}
	return size
}

// ParseGradleBuildCacheMaxAge returns age-based eviction threshold.
// Returns 0 when disabled or invalid.
func (c *Config) ParseGradleBuildCacheMaxAge() time.Duration {
	if c.Gradle.BuildCache.MaxAge == "" || c.Gradle.BuildCache.MaxAge == "0" {
		return 0
	}
	d, err := time.ParseDuration(c.Gradle.BuildCache.MaxAge)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// ParseGradleBuildCacheMaxSize returns total-size cap in bytes.
// Returns 0 when disabled or invalid.
func (c *Config) ParseGradleBuildCacheMaxSize() int64 {
	if c.Gradle.BuildCache.MaxSize == "" || c.Gradle.BuildCache.MaxSize == "0" {
		return 0
	}
	size, err := ParseSize(c.Gradle.BuildCache.MaxSize)
	if err != nil || size <= 0 {
		return 0
	}
	return size
}

// ParseGradleBuildCacheSweepInterval returns eviction sweep cadence.
// Defaults to 10m if unset or invalid.
func (c *Config) ParseGradleBuildCacheSweepInterval() time.Duration {
	if c.Gradle.BuildCache.SweepInterval == "" {
		return defaultGradleBuildCacheSweepInterval
	}
	d, err := time.ParseDuration(c.Gradle.BuildCache.SweepInterval)
	if err != nil || d <= 0 {
		return defaultGradleBuildCacheSweepInterval
	}
	return d
}

// ParseDirectServeTTL returns the presigned URL expiry duration.
// Returns 15 minutes if unset.
func (c *Config) ParseDirectServeTTL() time.Duration {
	if c.Storage.DirectServeTTL == "" {
		return defaultDirectServeTTL
	}
	d, err := time.ParseDuration(c.Storage.DirectServeTTL)
	if err != nil {
		return defaultDirectServeTTL
	}
	return d
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

// Header returns the HTTP header name and value for this auth config.
// Returns empty strings if the config is invalid or incomplete.
func (a *AuthConfig) Header() (name, value string) {
	switch strings.ToLower(a.Type) {
	case "bearer":
		token := expandEnv(a.Token)
		if token == "" {
			return "", ""
		}
		return "Authorization", "Bearer " + token

	case "basic":
		username := expandEnv(a.Username)
		password := expandEnv(a.Password)
		if username == "" {
			return "", ""
		}
		encoded := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
		return "Authorization", "Basic " + encoded

	case "header":
		name := a.HeaderName
		value := expandEnv(a.HeaderValue)
		if name == "" {
			return "", ""
		}
		return name, value

	default:
		return "", ""
	}
}

// expandEnv expands ${VAR_NAME} references in a string.
func expandEnv(s string) string {
	return os.Expand(s, os.Getenv)
}

func envBool(v string) bool {
	return v == "true" || v == "1"
}
