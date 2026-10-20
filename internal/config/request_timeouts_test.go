package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRequestTimeouts(t *testing.T) {
	for _, value := range []string{"", "0", "0s", "18m", "invalid", "-1s", "999999999999999999h"} {
		t.Run(value, func(t *testing.T) {
			cfg := Default()
			cfg.WriteTimeout, cfg.ArtifactTimeout = value, value
			valid := value == "" || value == "0" || value == "0s" || value == "18m"
			want := 5 * time.Minute
			if value == "0" || value == "0s" {
				want = 0
			}
			if value == "18m" {
				want = 18 * time.Minute
			}
			if got := cfg.ParseWriteTimeout(); got != want {
				t.Errorf("write timeout = %v, want %v", got, want)
			}
			if got := cfg.ParseArtifactTimeout(); got != want {
				t.Errorf("artifact timeout = %v, want %v", got, want)
			}
			err := cfg.Validate()
			if (err == nil) != valid {
				t.Fatalf("Validate() = %v, valid = %v", err, valid)
			}
			if !valid {
				if !strings.Contains(err.Error(), "write_timeout") {
					t.Fatalf("error does not identify write_timeout: %v", err)
				}
				cfg.WriteTimeout = ""
				if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "artifact_timeout") {
					t.Fatalf("artifact validation = %v", err)
				}
			}
		})
	}
}

func TestLoadRequestTimeouts(t *testing.T) {
	for _, format := range []struct{ extension, data string }{
		{"yaml", "write_timeout: 18m\nartifact_timeout: 10m\nhttp_timeout: 90s\n"},
		{"json", `{"write_timeout":"18m","artifact_timeout":"10m","http_timeout":"90s"}`},
	} {
		t.Run(format.extension, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config."+format.extension)
			if err := os.WriteFile(path, []byte(format.data), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.ParseWriteTimeout() != 18*time.Minute || cfg.ParseArtifactTimeout() != 10*time.Minute || cfg.ParseHTTPTimeout() != 90*time.Second {
				t.Fatalf("file timeouts = %v, %v, %v", cfg.WriteTimeout, cfg.ArtifactTimeout, cfg.HTTPTimeout)
			}
			t.Setenv("PROXY_WRITE_TIMEOUT", "20m")
			t.Setenv("PROXY_ARTIFACT_TIMEOUT", "0")
			t.Setenv("PROXY_HTTP_TIMEOUT", "15s")
			cfg.LoadFromEnv()
			if cfg.ParseWriteTimeout() != 20*time.Minute || cfg.ParseArtifactTimeout() != 0 || cfg.ParseHTTPTimeout() != 15*time.Second {
				t.Fatalf("environment timeouts = %v, %v, %v", cfg.WriteTimeout, cfg.ArtifactTimeout, cfg.HTTPTimeout)
			}
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
