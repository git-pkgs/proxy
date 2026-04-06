package config

import (
	"os"
	"path/filepath"
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
	t.Setenv("PROXY_STORAGE_PATH", "/env/cache")
	t.Setenv("PROXY_LOG_LEVEL", testLevelDebug)

	cfg.LoadFromEnv()

	if cfg.Listen != ":9000" {
		t.Errorf("Listen = %q, want %q", cfg.Listen, ":9000")
	}
	if cfg.BaseURL != "https://env.example.com" {
		t.Errorf("BaseURL = %q, want %q", cfg.BaseURL, "https://env.example.com")
	}
	if cfg.Storage.Path != "/env/cache" {
		t.Errorf("Storage.Path = %q, want %q", cfg.Storage.Path, "/env/cache")
	}
	if cfg.Log.Level != testLevelDebug {
		t.Errorf("Log.Level = %q, want %q", cfg.Log.Level, testLevelDebug)
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

func TestLoadMetadataTTLFromEnv(t *testing.T) {
	cfg := Default()
	t.Setenv("PROXY_METADATA_TTL", "10m")
	cfg.LoadFromEnv()

	if cfg.MetadataTTL != "10m" {
		t.Errorf("MetadataTTL = %q, want %q", cfg.MetadataTTL, "10m")
	}
}
