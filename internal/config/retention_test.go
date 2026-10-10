package config

import (
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/retention"
)

const retentionDay = 24 * time.Hour

func retentionTestRegistry() *retention.Registry {
	reg := retention.NewRegistry()
	reg.Register(retention.Spec{Key: "npm", CanonicalPackage: retention.DefaultCanonical("npm")})
	reg.Register(retention.Spec{Key: "julia"})
	return reg
}

func TestRetentionRulesUnsetIsDisabled(t *testing.T) {
	rules, err := Default().RetentionRules(retentionTestRegistry())
	if err != nil {
		t.Fatalf("RetentionRules() error = %v", err)
	}
	if got := rules.MinPositive(); got != 0 {
		t.Errorf("MinPositive() = %v, want 0 for an unset retention block", got)
	}
}

func TestRetentionRulesResolves(t *testing.T) {
	cfg := Default()
	cfg.Storage.Retention = RetentionConfig{
		Default:    "30d",
		Ecosystems: map[string]string{"npm": "14d"},
		Packages: map[string]string{
			"pkg:npm/lodash":      "0",
			"pkg:npm/@babel/core": "90d",
		},
		SweepInterval: "5m",
	}

	rules, err := cfg.RetentionRules(retentionTestRegistry())
	if err != nil {
		t.Fatalf("RetentionRules() error = %v", err)
	}
	if rules.Default != 30*retentionDay {
		t.Errorf("Default = %v, want 30d", rules.Default)
	}
	if got := rules.For("npm", "pkg:npm/left-pad"); got != 14*retentionDay {
		t.Errorf("npm rule = %v, want 14d", got)
	}
	if got, ok := rules.Packages["pkg:npm/lodash"]; !ok || got != 0 {
		t.Errorf("lodash rule = %v, %v, want 0, true", got, ok)
	}
	if got := rules.For("npm", "pkg:npm/%40babel/core"); got != 90*retentionDay {
		t.Errorf("babel rule = %v, want 90d under its canonical key", got)
	}
	if got := cfg.ParseRetentionSweepInterval(); got != 5*time.Minute {
		t.Errorf("ParseRetentionSweepInterval() = %v, want 5m", got)
	}
}

func TestRetentionRulesErrors(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*RetentionConfig)
		wantErr string
	}{
		{"bad default", func(r *RetentionConfig) { r.Default = "7x" }, "storage.retention.default"},
		{"negative default", func(r *RetentionConfig) { r.Default = "-1d" }, "must not be negative"},
		{"NaN days", func(r *RetentionConfig) { r.Default = "NaNd" }, "out of range"},
		{"infinite days", func(r *RetentionConfig) { r.Default = "Infd" }, "out of range"},
		{"overflowing days", func(r *RetentionConfig) { r.Default = "1e9d" }, "out of range"},
		{"zero sweep interval", func(r *RetentionConfig) { r.SweepInterval = "0s" }, "must be > 0"},
		{"bad sweep interval", func(r *RetentionConfig) { r.SweepInterval = "often" }, "sweep_interval"},
		{"unknown ecosystem", func(r *RetentionConfig) { r.Ecosystems = map[string]string{"nmp": "1d"} }, "unknown ecosystem"},
		{"generic is not an ecosystem key", func(r *RetentionConfig) { r.Ecosystems = map[string]string{"generic": "1d"} }, "unknown ecosystem"},
		{"ecosystem not registered", func(r *RetentionConfig) { r.Ecosystems = map[string]string{"oci": "14d"} },
			`storage.retention.ecosystems.oci: retention is not supported for "oci"`},
		{"bad ecosystem duration", func(r *RetentionConfig) { r.Ecosystems = map[string]string{"npm": "soon"} }, "storage.retention.ecosystems.npm"},
		{"package of unregistered ecosystem", func(r *RetentionConfig) { r.Packages = map[string]string{"pkg:cargo/serde": "1d"} },
			"not supported for cargo packages"},
		{"julia package", func(r *RetentionConfig) { r.Packages = map[string]string{"pkg:julia/Example": "1d"} },
			"not supported for julia"},
		{"invalid package PURL", func(r *RetentionConfig) { r.Packages = map[string]string{"lodash": "1d"} }, "not a valid package PURL"},
		{"versioned package PURL", func(r *RetentionConfig) { r.Packages = map[string]string{"pkg:npm/lodash@4.17.21": "1d"} },
			"must not carry a version"},
		{"conflicting package keys", func(r *RetentionConfig) {
			r.Packages = map[string]string{"pkg:npm/%40babel/core": "1d", "pkg:npm/@babel/core": "2d"}
		}, "conflicts with another key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			tt.modify(&cfg.Storage.Retention)
			_, err := cfg.RetentionRules(retentionTestRegistry())
			if err == nil {
				t.Fatalf("RetentionRules() succeeded, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("RetentionRules() error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// Validate checks retention against the handlers' registry, which this
// package's tests leave empty, so any ecosystem rule fails while a bare
// default passes.
func TestValidateRetention(t *testing.T) {
	cfg := Default()
	cfg.Storage.Retention.Default = "30d"
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() with only a default = %v, want nil", err)
	}

	cfg.Storage.Retention.Ecosystems = map[string]string{"npm": "14d"}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("Validate() with an unsupported ecosystem = %v, want a not supported error", err)
	}
}

func TestRetentionEnvOverrides(t *testing.T) {
	t.Setenv("PROXY_STORAGE_RETENTION_DEFAULT", "21d")
	t.Setenv("PROXY_STORAGE_RETENTION_SWEEP_INTERVAL", "1h")

	cfg := Default()
	cfg.LoadFromEnv()

	if cfg.Storage.Retention.Default != "21d" {
		t.Errorf("Retention.Default = %q, want 21d", cfg.Storage.Retention.Default)
	}
	if got := cfg.ParseRetentionSweepInterval(); got != time.Hour {
		t.Errorf("ParseRetentionSweepInterval() = %v, want 1h", got)
	}
}

func TestRetentionSweepIntervalDefault(t *testing.T) {
	cfg := Default()
	if got := cfg.ParseRetentionSweepInterval(); got != 10*time.Minute {
		t.Errorf("ParseRetentionSweepInterval() = %v, want 10m", got)
	}
}

func TestGradleBuildCacheMaxAgeAcceptsDays(t *testing.T) {
	cfg := Default()
	cfg.Gradle.BuildCache.MaxAge = "7d"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() with max_age 7d = %v, want nil", err)
	}
	if got := cfg.ParseGradleBuildCacheMaxAge(); got != 7*retentionDay {
		t.Errorf("ParseGradleBuildCacheMaxAge() = %v, want 168h", got)
	}
}
