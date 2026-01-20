package config

import (
	"os"
	"path/filepath"
	"testing"
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
			name:    "empty storage path",
			modify:  func(c *Config) { c.Storage.Path = "" },
			wantErr: true,
		},
		{
			name:    "empty database path",
			modify:  func(c *Config) { c.Database.Path = "" },
			wantErr: true,
		},
		{
			name:    "invalid log level",
			modify:  func(c *Config) { c.Log.Level = "invalid" },
			wantErr: true,
		},
		{
			name:    "invalid log format",
			modify:  func(c *Config) { c.Log.Format = "invalid" },
			wantErr: true,
		},
		{
			name:    "invalid max size",
			modify:  func(c *Config) { c.Storage.MaxSize = "invalid" },
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
	if cfg.Log.Level != "debug" {
		t.Errorf("Log.Level = %q, want %q", cfg.Log.Level, "debug")
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
	t.Setenv("PROXY_LOG_LEVEL", "debug")

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
	if cfg.Log.Level != "debug" {
		t.Errorf("Log.Level = %q, want %q", cfg.Log.Level, "debug")
	}
}

func TestLoadFileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/config.yaml")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}
