package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testDriverPostgres = "postgres"
	testInvalid        = "invalid"
	testLevelDebug     = "debug"
)

func TestDefault(t *testing.T) {
	cfg := Default()

	if cfg.Listen != ":8080" {
		t.Errorf("Listen = %q, want %q", cfg.Listen, ":8080")
	}
	if cfg.Storage.Path == "" {
		t.Error("Storage.Path should not be empty")
	}
	if cfg.Database.Path == "" {
		t.Error("Database.Path should not be empty")
	}
	if cfg.Gradle.BuildCache.MaxUploadSize != "100MB" {
		t.Errorf("Gradle.BuildCache.MaxUploadSize = %q, want %q", cfg.Gradle.BuildCache.MaxUploadSize, "100MB")
	}
	if cfg.Gradle.BuildCache.MaxAge != "168h" {
		t.Errorf("Gradle.BuildCache.MaxAge = %q, want %q", cfg.Gradle.BuildCache.MaxAge, "168h")
	}
	if cfg.Upstream.Maven != "https://repo1.maven.org/maven2" {
		t.Errorf("Upstream.Maven = %q, want %q", cfg.Upstream.Maven, "https://repo1.maven.org/maven2")
	}
	if cfg.Upstream.GradlePluginPortal != "https://plugins.gradle.org/m2" {
		t.Errorf("Upstream.GradlePluginPortal = %q, want %q", cfg.Upstream.GradlePluginPortal, "https://plugins.gradle.org/m2")
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*Config)
		wantErr bool
	}{
		{
			name:    "valid default",
			modify:  func(c *Config) {},
			wantErr: false,
		},
		{
			name:    "empty listen",
			modify:  func(c *Config) { c.Listen = "" },
			wantErr: true,
		},
		{
			name:    "empty base_url",
			modify:  func(c *Config) { c.BaseURL = "" },
			wantErr: true,
		},
		{
			name:    "empty storage path and url",
			modify:  func(c *Config) { c.Storage.Path = ""; c.Storage.URL = "" },
			wantErr: true,
		},
		{
			name:    "storage url set",
			modify:  func(c *Config) { c.Storage.Path = ""; c.Storage.URL = "s3://bucket" },
			wantErr: false,
		},
		{
			name:    "empty database path for sqlite",
			modify:  func(c *Config) { c.Database.Path = "" },
			wantErr: true,
		},
		{
			name:    "invalid database driver",
			modify:  func(c *Config) { c.Database.Driver = "mysql" },
			wantErr: true,
		},
		{
			name:    "postgres without url",
			modify:  func(c *Config) { c.Database.Driver = testDriverPostgres; c.Database.URL = "" },
			wantErr: true,
		},
		{
			name:    "postgres with url",
			modify:  func(c *Config) { c.Database.Driver = testDriverPostgres; c.Database.URL = "postgres://localhost/test" },
			wantErr: false,
		},
		{
			name:    "invalid log level",
			modify:  func(c *Config) { c.Log.Level = testInvalid },
			wantErr: true,
		},
		{
			name:    "invalid log format",
			modify:  func(c *Config) { c.Log.Format = testInvalid },
			wantErr: true,
		},
		{
			name:    "invalid max size",
			modify:  func(c *Config) { c.Storage.MaxSize = testInvalid },
			wantErr: true,
		},
		{
			name:    "valid max size",
			modify:  func(c *Config) { c.Storage.MaxSize = "10GB" },
			wantErr: false,
		},
		{
			name:    "invalid gradle upload size",
			modify:  func(c *Config) { c.Gradle.BuildCache.MaxUploadSize = testInvalid },
			wantErr: true,
		},
		{
			name:    "zero gradle upload size",
			modify:  func(c *Config) { c.Gradle.BuildCache.MaxUploadSize = "0" },
			wantErr: true,
		},
		{
			name:    "invalid gradle max age",
			modify:  func(c *Config) { c.Gradle.BuildCache.MaxAge = testInvalid },
			wantErr: true,
		},
		{
			name:    "valid gradle max age",
			modify:  func(c *Config) { c.Gradle.BuildCache.MaxAge = "24h" },
			wantErr: false,
		},
		{
			name:    "invalid gradle max size",
			modify:  func(c *Config) { c.Gradle.BuildCache.MaxSize = testInvalid },
			wantErr: true,
		},
		{
			name:    "invalid gradle sweep interval",
			modify:  func(c *Config) { c.Gradle.BuildCache.SweepInterval = "0" },
			wantErr: true,
		},
		{
			name:    "valid gradle sweep interval",
			modify:  func(c *Config) { c.Gradle.BuildCache.SweepInterval = "30m" },
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			tt.modify(cfg)
			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestParseSize(t *testing.T) {
	tests := []struct {
		input   string
		want    int64
		wantErr bool
	}{
		{"", 0, false},
		{"0", 0, false},
		{"100", 100, false},
		{"1KB", 1024, false},
		{"1K", 1024, false},
		{"1MB", 1024 * 1024, false},
		{"1M", 1024 * 1024, false},
		{"1GB", 1024 * 1024 * 1024, false},
		{"1G", 1024 * 1024 * 1024, false},
		{"10GB", 10 * 1024 * 1024 * 1024, false},
		{"1.5GB", int64(1.5 * 1024 * 1024 * 1024), false},
		{"1TB", 1024 * 1024 * 1024 * 1024, false},
		{"invalid", 0, true},
		{"10XB", 0, true},
	}

	for _, tt := range tests {
		got, err := ParseSize(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseSize(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseSize(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestLoadYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	content := `
listen: ":3000"
base_url: "https://example.com"
storage:
  path: "/data/cache"
  max_size: "5GB"
database:
  path: "/data/proxy.db"
log:
  level: "debug"
  format: "json"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writing config file: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.Listen != ":3000" {
		t.Errorf("Listen = %q, want %q", cfg.Listen, ":3000")
	}
	if cfg.BaseURL != "https://example.com" {
		t.Errorf("BaseURL = %q, want %q", cfg.BaseURL, "https://example.com")
	}
	if cfg.Storage.Path != "/data/cache" {
		t.Errorf("Storage.Path = %q, want %q", cfg.Storage.Path, "/data/cache")
	}
	if cfg.Storage.MaxSize != "5GB" {
		t.Errorf("Storage.MaxSize = %q, want %q", cfg.Storage.MaxSize, "5GB")
	}
	if cfg.Log.Level != testLevelDebug {
		t.Errorf("Log.Level = %q, want %q", cfg.Log.Level, testLevelDebug)
	}
	if cfg.Log.Format != "json" {
		t.Errorf("Log.Format = %q, want %q", cfg.Log.Format, "json")
	}
}

func TestLoadJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	content := `{
		"listen": ":4000",
		"base_url": "https://json.example.com"
	}`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writing config file: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.Listen != ":4000" {
		t.Errorf("Listen = %q, want %q", cfg.Listen, ":4000")
	}
	if cfg.BaseURL != "https://json.example.com" {
		t.Errorf("BaseURL = %q, want %q", cfg.BaseURL, "https://json.example.com")
	}
}

func TestLoadFromEnv(t *testing.T) {
	cfg := Default()

	t.Setenv("PROXY_LISTEN", ":9000")
	t.Setenv("PROXY_BASE_URL", "https://env.example.com")
	t.Setenv("PROXY_UI_URL", "https://ui.env.example.com/ui")
	t.Setenv("PROXY_STORAGE_PATH", "/env/cache")
	t.Setenv("PROXY_LOG_LEVEL", testLevelDebug)
	t.Setenv("PROXY_UPSTREAM_MAVEN", "https://maven.example.com/repository/maven-public")
	t.Setenv("PROXY_UPSTREAM_GRADLE_PLUGIN_PORTAL", "https://plugins.example.com/m2")
	t.Setenv("PROXY_GRADLE_BUILD_CACHE_READ_ONLY", "true")
	t.Setenv("PROXY_GRADLE_BUILD_CACHE_MAX_UPLOAD_SIZE", "32MB")
	t.Setenv("PROXY_GRADLE_BUILD_CACHE_MAX_AGE", "12h")
	t.Setenv("PROXY_GRADLE_BUILD_CACHE_MAX_SIZE", "10GB")
	t.Setenv("PROXY_GRADLE_BUILD_CACHE_SWEEP_INTERVAL", "15m")

	cfg.LoadFromEnv()

	if cfg.Listen != ":9000" {
		t.Errorf("Listen = %q, want %q", cfg.Listen, ":9000")
	}
	if cfg.BaseURL != "https://env.example.com" {
		t.Errorf("BaseURL = %q, want %q", cfg.BaseURL, "https://env.example.com")
	}
	if cfg.UIBaseURL != "https://ui.env.example.com/ui" {
		t.Errorf("UIBaseURL = %q, want %q", cfg.UIBaseURL, "https://ui.env.example.com/ui")
	}
	if cfg.Storage.Path != "/env/cache" {
		t.Errorf("Storage.Path = %q, want %q", cfg.Storage.Path, "/env/cache")
	}
	if cfg.Log.Level != testLevelDebug {
		t.Errorf("Log.Level = %q, want %q", cfg.Log.Level, testLevelDebug)
	}
	if cfg.Upstream.Maven != "https://maven.example.com/repository/maven-public" {
		t.Errorf("Upstream.Maven = %q, want %q", cfg.Upstream.Maven, "https://maven.example.com/repository/maven-public")
	}
	if cfg.Upstream.GradlePluginPortal != "https://plugins.example.com/m2" {
		t.Errorf("Upstream.GradlePluginPortal = %q, want %q", cfg.Upstream.GradlePluginPortal, "https://plugins.example.com/m2")
	}
	if !cfg.Gradle.BuildCache.ReadOnly {
		t.Error("Gradle.BuildCache.ReadOnly = false, want true")
	}
	if cfg.Gradle.BuildCache.MaxUploadSize != "32MB" {
		t.Errorf("Gradle.BuildCache.MaxUploadSize = %q, want %q", cfg.Gradle.BuildCache.MaxUploadSize, "32MB")
	}
	if cfg.Gradle.BuildCache.MaxAge != "12h" {
		t.Errorf("Gradle.BuildCache.MaxAge = %q, want %q", cfg.Gradle.BuildCache.MaxAge, "12h")
	}
	if cfg.Gradle.BuildCache.MaxSize != "10GB" {
		t.Errorf("Gradle.BuildCache.MaxSize = %q, want %q", cfg.Gradle.BuildCache.MaxSize, "10GB")
	}
	if cfg.Gradle.BuildCache.SweepInterval != "15m" {
		t.Errorf("Gradle.BuildCache.SweepInterval = %q, want %q", cfg.Gradle.BuildCache.SweepInterval, "15m")
	}
}

func TestLoadCooldownConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	content := `
listen: ":8080"
base_url: "http://localhost:8080"
storage:
  path: "/data/cache"
database:
  path: "/data/proxy.db"
cooldown:
  default: "3d"
  ecosystems:
    npm: "7d"
    cargo: "0"
  packages:
    "pkg:npm/lodash": "0"
    "pkg:npm/@babel/core": "14d"
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writing config file: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.Cooldown.Default != "3d" {
		t.Errorf("Cooldown.Default = %q, want %q", cfg.Cooldown.Default, "3d")
	}
	if cfg.Cooldown.Ecosystems["npm"] != "7d" {
		t.Errorf("Cooldown.Ecosystems[npm] = %q, want %q", cfg.Cooldown.Ecosystems["npm"], "7d")
	}
	if cfg.Cooldown.Ecosystems["cargo"] != "0" {
		t.Errorf("Cooldown.Ecosystems[cargo] = %q, want %q", cfg.Cooldown.Ecosystems["cargo"], "0")
	}
	if cfg.Cooldown.Packages["pkg:npm/lodash"] != "0" {
		t.Errorf("Cooldown.Packages[lodash] = %q, want %q", cfg.Cooldown.Packages["pkg:npm/lodash"], "0")
	}
	if cfg.Cooldown.Packages["pkg:npm/@babel/core"] != "14d" {
		t.Errorf("Cooldown.Packages[@babel/core] = %q, want %q", cfg.Cooldown.Packages["pkg:npm/@babel/core"], "14d")
	}
	if got := cfg.Cooldown.NormalizedPackages()["pkg:npm/%40babel/core"]; got != "14d" {
		t.Errorf("normalized Cooldown.Packages[@babel/core] = %q, want %q", got, "14d")
	}
}

func TestCooldownConfigNormalizedPackages(t *testing.T) {
	rawScoped := "pkg:npm/@typescript/typescript-darwin-arm64"
	canonicalScoped := "pkg:npm/%40typescript/typescript-darwin-arm64"
	cfg := CooldownConfig{Packages: map[string]string{
		rawScoped:       "2d",
		canonicalScoped: "3d",
		"not-a-purl":    "4d",
	}}

	got := cfg.NormalizedPackages()

	if got[canonicalScoped] != "3d" {
		t.Errorf("canonical scoped package duration = %q, want %q", got[canonicalScoped], "3d")
	}
	if _, exists := got[rawScoped]; exists {
		t.Errorf("raw scoped package key %q was not canonicalized", rawScoped)
	}
	if got["not-a-purl"] != "4d" {
		t.Errorf("invalid PURL duration = %q, want preserved value %q", got["not-a-purl"], "4d")
	}
	if cfg.Packages[rawScoped] != "2d" {
		t.Error("NormalizedPackages mutated the source map")
	}
}

func TestLoadCooldownFromEnv(t *testing.T) {
	cfg := Default()
	t.Setenv("PROXY_COOLDOWN_DEFAULT", "5d")
	cfg.LoadFromEnv()

	if cfg.Cooldown.Default != "5d" {
		t.Errorf("Cooldown.Default = %q, want %q", cfg.Cooldown.Default, "5d")
	}
}

func TestLoadFileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/config.yaml")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestParseMaxSize(t *testing.T) {
	tests := []struct {
		name    string
		maxSize string
		want    int64
	}{
		{"empty means unlimited", "", 0},
		{"zero means unlimited", "0", 0},
		{"10GB", "10GB", 10 * 1024 * 1024 * 1024},
		{"500MB", "500MB", 500 * 1024 * 1024},
		{"invalid returns 0", "invalid", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			cfg.Storage.MaxSize = tt.maxSize
			got := cfg.ParseMaxSize()
			if got != tt.want {
				t.Errorf("ParseMaxSize() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestParseMetadataTTL(t *testing.T) {
	tests := []struct {
		name string
		ttl  string
		want time.Duration
	}{
		{"empty defaults to 5m", "", 5 * time.Minute},
		{"explicit zero", "0", 0},
		{"10 minutes", "10m", 10 * time.Minute},
		{"1 hour", "1h", 1 * time.Hour},
		{"invalid defaults to 5m", "not-a-duration", 5 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			cfg.MetadataTTL = tt.ttl
			got := cfg.ParseMetadataTTL()
			if got != tt.want {
				t.Errorf("ParseMetadataTTL() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseMetadataMaxSize(t *testing.T) {
	tests := []struct {
		name string
		size string
		want int64
	}{
		{"unset uses default", "", defaultMetadataMaxSize},
		{"explicit value", "250MB", 250 << 20},
		{"bytes", "1024", 1024},
		{"invalid uses default", "lots", defaultMetadataMaxSize},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			cfg.MetadataMaxSize = tt.size
			got := cfg.ParseMetadataMaxSize()
			if got != tt.want {
				t.Errorf("ParseMetadataMaxSize() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestValidateMetadataMaxSize(t *testing.T) {
	cfg := Default()
	cfg.MetadataMaxSize = "not-a-size"
	if err := cfg.Validate(); err == nil {
		t.Error("expected validation error for invalid metadata_max_size")
	}

	cfg.MetadataMaxSize = "0"
	if err := cfg.Validate(); err == nil {
		t.Error("expected validation error for zero metadata_max_size")
	}

	cfg.MetadataMaxSize = "250MB"
	if err := cfg.Validate(); err != nil {
		t.Errorf("unexpected error for valid metadata_max_size: %v", err)
	}

	cfg.MetadataMaxSize = ""
	if err := cfg.Validate(); err != nil {
		t.Errorf("unexpected error for unset metadata_max_size: %v", err)
	}
}

func TestValidateMetadataTTL(t *testing.T) {
	cfg := Default()
	cfg.MetadataTTL = "invalid"
	if err := cfg.Validate(); err == nil {
		t.Error("expected validation error for invalid metadata_ttl")
	}

	cfg.MetadataTTL = "5m"
	if err := cfg.Validate(); err != nil {
		t.Errorf("unexpected error for valid metadata_ttl: %v", err)
	}

	cfg.MetadataTTL = "0"
	if err := cfg.Validate(); err != nil {
		t.Errorf("unexpected error for zero metadata_ttl: %v", err)
	}
}

func TestValidateHealthStorageProbeInterval(t *testing.T) {
	cfg := Default()
	cfg.Health.StorageProbeInterval = "not-a-duration"
	if err := cfg.Validate(); err == nil {
		t.Error("expected validation error for invalid health.storage_probe_interval")
	}

	cfg.Health.StorageProbeInterval = "30s"
	if err := cfg.Validate(); err != nil {
		t.Errorf("unexpected error for valid health.storage_probe_interval: %v", err)
	}

	cfg.Health.StorageProbeInterval = "0"
	if err := cfg.Validate(); err != nil {
		t.Errorf("unexpected error for zero health.storage_probe_interval: %v", err)
	}

	cfg.Health.StorageProbeInterval = ""
	if err := cfg.Validate(); err != nil {
		t.Errorf("unexpected error for empty health.storage_probe_interval: %v", err)
	}

	cfg.Health.StorageProbeInterval = "-5s"
	if err := cfg.Validate(); err == nil {
		t.Error("expected validation error for negative health.storage_probe_interval")
	}
}

func TestParseHTTPTimeout(t *testing.T) {
	tests := []struct {
		name    string
		timeout string
		want    time.Duration
	}{
		{"empty defaults to 30s", "", 30 * time.Second},
		{"explicit zero disables", "0", 0},
		{"2 minutes", "2m", 2 * time.Minute},
		{"90 seconds", "90s", 90 * time.Second},
		{"invalid defaults to 30s", "not-a-duration", 30 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			cfg.HTTPTimeout = tt.timeout
			got := cfg.ParseHTTPTimeout()
			if got != tt.want {
				t.Errorf("ParseHTTPTimeout() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestValidateHTTPTimeout(t *testing.T) {
	cfg := Default()
	cfg.HTTPTimeout = "not-a-duration"
	if err := cfg.Validate(); err == nil {
		t.Error("expected validation error for invalid http_timeout")
	}

	cfg.HTTPTimeout = "-5s"
	if err := cfg.Validate(); err == nil {
		t.Error("expected validation error for negative http_timeout")
	}

	cfg.HTTPTimeout = "2m"
	if err := cfg.Validate(); err != nil {
		t.Errorf("unexpected error for valid http_timeout: %v", err)
	}

	cfg.HTTPTimeout = "0"
	if err := cfg.Validate(); err != nil {
		t.Errorf("unexpected error for zero http_timeout: %v", err)
	}

	cfg.HTTPTimeout = ""
	if err := cfg.Validate(); err != nil {
		t.Errorf("unexpected error for empty http_timeout: %v", err)
	}
}

func TestLoadHTTPTimeoutFromEnv(t *testing.T) {
	cfg := Default()
	t.Setenv("PROXY_HTTP_TIMEOUT", "90s")
	cfg.LoadFromEnv()

	if cfg.HTTPTimeout != "90s" {
		t.Errorf("HTTPTimeout = %q, want %q", cfg.HTTPTimeout, "90s")
	}
}

func TestLoadMetadataTTLFromEnv(t *testing.T) {
	cfg := Default()
	t.Setenv("PROXY_METADATA_TTL", "10m")
	cfg.LoadFromEnv()

	if cfg.MetadataTTL != "10m" {
		t.Errorf("MetadataTTL = %q, want %q", cfg.MetadataTTL, "10m")
	}
}

func TestParseGradleBuildCacheConfig(t *testing.T) {
	cfg := Default()

	if got := cfg.ParseGradleBuildCacheMaxUploadSize(); got != 100*1024*1024 {
		t.Errorf("ParseGradleBuildCacheMaxUploadSize() = %d, want %d", got, 100*1024*1024)
	}
	if got := cfg.ParseGradleBuildCacheMaxAge(); got != 168*time.Hour {
		t.Errorf("ParseGradleBuildCacheMaxAge() = %v, want %v", got, 168*time.Hour)
	}
	if got := cfg.ParseGradleBuildCacheMaxSize(); got != 0 {
		t.Errorf("ParseGradleBuildCacheMaxSize() = %d, want 0", got)
	}
	if got := cfg.ParseGradleBuildCacheSweepInterval(); got != 10*time.Minute {
		t.Errorf("ParseGradleBuildCacheSweepInterval() = %v, want %v", got, 10*time.Minute)
	}

	cfg.Gradle.BuildCache.MaxUploadSize = "64MB"
	cfg.Gradle.BuildCache.MaxAge = "48h"
	cfg.Gradle.BuildCache.MaxSize = "2GB"
	cfg.Gradle.BuildCache.SweepInterval = "20m"

	if got := cfg.ParseGradleBuildCacheMaxUploadSize(); got != 64*1024*1024 {
		t.Errorf("ParseGradleBuildCacheMaxUploadSize() = %d, want %d", got, 64*1024*1024)
	}
	if got := cfg.ParseGradleBuildCacheMaxAge(); got != 48*time.Hour {
		t.Errorf("ParseGradleBuildCacheMaxAge() = %v, want %v", got, 48*time.Hour)
	}
	if got := cfg.ParseGradleBuildCacheMaxSize(); got != 2*1024*1024*1024 {
		t.Errorf("ParseGradleBuildCacheMaxSize() = %d, want %d", got, 2*1024*1024*1024)
	}
	if got := cfg.ParseGradleBuildCacheSweepInterval(); got != 20*time.Minute {
		t.Errorf("ParseGradleBuildCacheSweepInterval() = %v, want %v", got, 20*time.Minute)
	}
}

func TestParseDirectServeTTL(t *testing.T) {
	tests := []struct {
		name string
		ttl  string
		want time.Duration
	}{
		{"empty defaults to 15m", "", 15 * time.Minute},
		{"5 minutes", "5m", 5 * time.Minute},
		{"1 hour", "1h", 1 * time.Hour},
		{"invalid defaults to 15m", "not-a-duration", 15 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			cfg.Storage.DirectServeTTL = tt.ttl
			got := cfg.ParseDirectServeTTL()
			if got != tt.want {
				t.Errorf("ParseDirectServeTTL() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestValidateDirectServeTTL(t *testing.T) {
	cfg := Default()
	cfg.Storage.DirectServeTTL = "invalid"
	if err := cfg.Validate(); err == nil {
		t.Error("expected validation error for invalid storage.direct_serve_ttl")
	}

	cfg.Storage.DirectServeTTL = "5m"
	if err := cfg.Validate(); err != nil {
		t.Errorf("unexpected error for valid storage.direct_serve_ttl: %v", err)
	}
}

func TestLoadDirectServeFromEnv(t *testing.T) {
	cfg := Default()
	t.Setenv("PROXY_STORAGE_DIRECT_SERVE", "true")
	t.Setenv("PROXY_STORAGE_DIRECT_SERVE_TTL", "30m")
	t.Setenv("PROXY_STORAGE_DIRECT_SERVE_BASE_URL", "https://cdn.example.com")
	cfg.LoadFromEnv()

	if !cfg.Storage.DirectServe {
		t.Error("Storage.DirectServe should be true")
	}
	if cfg.Storage.DirectServeTTL != "30m" {
		t.Errorf("Storage.DirectServeTTL = %q, want %q", cfg.Storage.DirectServeTTL, "30m")
	}
	if cfg.Storage.DirectServeBaseURL != "https://cdn.example.com" {
		t.Errorf("Storage.DirectServeBaseURL = %q, want %q", cfg.Storage.DirectServeBaseURL, "https://cdn.example.com")
	}
}

func TestValidateUIBaseURLDefaultsToBaseURL(t *testing.T) {
	cfg := Default()
	cfg.BaseURL = "https://proxy.example.com"
	cfg.UIBaseURL = ""

	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	if cfg.UIBaseURL != "https://proxy.example.com" {
		t.Errorf("UIBaseURL = %q, want it to default to BaseURL %q", cfg.UIBaseURL, "https://proxy.example.com")
	}
}

func TestValidateUIBaseURL(t *testing.T) {
	cfg := Default()

	cfg.UIBaseURL = "not a url"
	if err := cfg.Validate(); err == nil {
		t.Error("expected validation error for relative ui_base_url")
	}

	cfg = Default()
	cfg.UIBaseURL = "://bad"
	if err := cfg.Validate(); err == nil {
		t.Error("expected validation error for unparseable ui_base_url")
	}

	cfg = Default()
	cfg.UIBaseURL = "https://ui.example.com/ui"
	if err := cfg.Validate(); err != nil {
		t.Errorf("unexpected error for valid ui_base_url: %v", err)
	}
}

func TestValidateDirectServeBaseURL(t *testing.T) {
	cfg := Default()

	cfg.Storage.DirectServeBaseURL = "not a url"
	if err := cfg.Validate(); err == nil {
		t.Error("expected validation error for relative direct_serve_base_url")
	}

	cfg.Storage.DirectServeBaseURL = "://bad"
	if err := cfg.Validate(); err == nil {
		t.Error("expected validation error for unparseable direct_serve_base_url")
	}

	cfg.Storage.DirectServeBaseURL = "https://cdn.example.com"
	if err := cfg.Validate(); err != nil {
		t.Errorf("unexpected error for valid direct_serve_base_url: %v", err)
	}
}

func TestDatabaseConfigString(t *testing.T) {
	tests := []struct {
		name string
		cfg  DatabaseConfig
		want string
	}{
		{"sqlite", DatabaseConfig{Driver: "sqlite", Path: "./cache/proxy.db"}, "./cache/proxy.db"},
		{"default driver", DatabaseConfig{Path: "/var/lib/proxy.db"}, "/var/lib/proxy.db"},
		{"postgres no password", DatabaseConfig{Driver: "postgres", URL: "postgres://user@localhost:5432/proxy"}, "postgres://user@localhost:5432/proxy"},
		{"postgres redacts password", DatabaseConfig{Driver: "postgres", URL: "postgres://user:secret@localhost:5432/proxy?sslmode=disable"}, "postgres://user:xxxxx@localhost:5432/proxy?sslmode=disable"},
		{"postgres unparseable url", DatabaseConfig{Driver: "postgres", URL: "host=localhost user=foo password=bar"}, "postgres"},
		{"postgres ignores sqlite path", DatabaseConfig{Driver: "postgres", URL: "postgres://localhost/db", Path: "./cache/proxy.db"}, "postgres://localhost/db"},
	}

	for _, tt := range tests {
		if got := tt.cfg.String(); got != tt.want {
			t.Errorf("%s: String() = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestUpstreamAuthForURLMatchesURLComponents(t *testing.T) {
	registryAuth := AuthConfig{Type: "bearer", Token: "registry-token"}
	privateAuth := AuthConfig{Type: "bearer", Token: "private-token"}
	config := UpstreamConfig{Auth: map[string]AuthConfig{
		"https://registry.example.com":         registryAuth,
		"https://registry.example.com/private": privateAuth,
	}}

	tests := []struct {
		name      string
		url       string
		wantToken string
	}{
		{name: "registry root", url: "https://registry.example.com/package", wantToken: "registry-token"},
		{name: "host is case insensitive", url: "https://REGISTRY.EXAMPLE.COM/package", wantToken: "registry-token"},
		{name: "longest path match", url: "https://registry.example.com/private/package", wantToken: "private-token"},
		{name: "exact path match", url: "https://registry.example.com/private", wantToken: "private-token"},
		{name: "path segment boundary", url: "https://registry.example.com/private-other/package", wantToken: "registry-token"},
		{name: "lookalike host rejected", url: "https://registry.example.com.evil.test/package"},
		{name: "different scheme rejected", url: "http://registry.example.com/package"},
		{name: "different port rejected", url: "https://registry.example.com:8443/package"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auth := config.AuthForURL(tt.url)
			if tt.wantToken == "" {
				if auth != nil {
					t.Fatalf("AuthForURL() = %+v, want nil", auth)
				}
				return
			}
			if auth == nil {
				t.Fatal("AuthForURL() = nil, want authentication")
			}
			if auth.Token != tt.wantToken {
				t.Errorf("token = %q, want %q", auth.Token, tt.wantToken)
			}
		})
	}
}

func TestValidateUpstreamAuthURLs(t *testing.T) {
	t.Run("valid absolute URL", func(t *testing.T) {
		cfg := Default()
		cfg.Upstream.Auth = map[string]AuthConfig{
			"https://registry.example.com/private": {Type: "bearer", Token: "token"},
		}

		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate() error = %v", err)
		}
	})

	t.Run("invalid URL", func(t *testing.T) {
		cfg := Default()
		cfg.Upstream.Auth = map[string]AuthConfig{
			"registry.example.com": {Type: "bearer", Token: "token"},
		}

		err := cfg.Validate()
		if err == nil {
			t.Fatal("Validate() error = nil, want invalid upstream.auth URL error")
		}
		if !strings.Contains(err.Error(), "upstream.auth") || !strings.Contains(err.Error(), "registry.example.com") {
			t.Errorf("Validate() error = %q, want field and URL", err)
		}
	})
}
