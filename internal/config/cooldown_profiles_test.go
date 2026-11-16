package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadCooldownProfiles(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "config.yaml")
	data := `cooldown:
  default: 7d
  ecosystems:
    npm: "0"
  profiles:
    3d:
      default: 3d
      packages:
        "pkg:pypi/Django": "0"
    strict:
      default: 14d
      package_patterns:
        "pkg:npm/@example/*": 1d
`
	if err := os.WriteFile(filename, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(filename)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	profile := cfg.Cooldown.Profiles["3d"]
	policy, err := profile.Policy()
	if err != nil {
		t.Fatal(err)
	}
	if got := policy.For("npm", "pkg:npm/demo"); got != 72*time.Hour {
		t.Fatalf("profile inherited global ecosystem override: %v", got)
	}
	if got := policy.For("pypi", "pkg:pypi/django"); got != 0 {
		t.Fatalf("canonical package override = %v", got)
	}
	strict := cfg.Cooldown.Profiles["strict"]
	policy, err = strict.Policy()
	if err != nil {
		t.Fatal(err)
	}
	if got := policy.For("npm", "pkg:npm/%40example/tool"); got != 24*time.Hour {
		t.Fatalf("profile pattern = %v", got)
	}
}

func TestLoadRejectsInvalidCooldownProfiles(t *testing.T) {
	for _, tc := range []struct{ name, profile, want string }{
		{"path", `"a/b": {default: 3d}`, "profile name"},
		{"uppercase", `Strict: {default: 3d}`, "profile name"},
		{"empty", `"": {default: 3d}`, "profile name"},
		{"duration", `strict: {default: nope}`, "cooldown.profiles.strict"},
		{"pattern", `strict: {package_patterns: {"pkg:npm/[": 3d}}`, "cooldown.profiles.strict"},
		{"nested", `strict: {profiles: {nested: {default: 3d}}}`, "cannot contain profiles"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(filename, []byte("cooldown:\n  profiles:\n    "+tc.profile+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(filename)
			if err == nil {
				err = cfg.Validate()
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRejectsInvalidTopLevelCooldown(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*CooldownConfig)
		want string
	}{
		{"default weeks", func(c *CooldownConfig) { c.Default = "1w" }, "cooldown: invalid default"},
		{"default words", func(c *CooldownConfig) { c.Default = "7 days" }, "cooldown: invalid default"},
		{"default negative", func(c *CooldownConfig) { c.Default = "-3d" }, "cooldown: invalid default"},
		{"ecosystem", func(c *CooldownConfig) { c.Ecosystems = map[string]string{"npm": "3days"} }, "cooldown: invalid ecosystems.npm"},
		{"package", func(c *CooldownConfig) { c.Packages = map[string]string{"pkg:npm/lodash": "soon"} }, "cooldown: invalid packages.pkg:npm/lodash"},
		{"pattern weeks", func(c *CooldownConfig) { c.PackagePatterns = map[string]string{"pkg:npm/*": "1w"} }, "cooldown: invalid package_patterns.pkg:npm/*"},
		{"pattern negative", func(c *CooldownConfig) { c.PackagePatterns = map[string]string{"pkg:npm/*": "-1d"} }, "cooldown: invalid package_patterns.pkg:npm/*"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.set(&cfg.Cooldown)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestRejectsEveryInvalidTopLevelCooldown(t *testing.T) {
	cfg := Default()
	cfg.Cooldown.Default = "1w"
	cfg.Cooldown.Ecosystems = map[string]string{"npm": "3days", "pypi": "2d"}
	cfg.Cooldown.Packages = map[string]string{"pkg:npm/lodash": "soon"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate = nil, want the invalid durations rejected")
	}
	for _, want := range []string{"invalid default", "invalid ecosystems.npm", "invalid packages.pkg:npm/lodash"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate = %q, want it to contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "ecosystems.pypi") {
		t.Errorf("Validate = %q, want the valid pypi value left out", err)
	}
}

func TestAcceptsValidTopLevelCooldown(t *testing.T) {
	for _, value := range []string{"", "0", "3d", "1.5d", "48h", "90m"} {
		cfg := Default()
		cfg.Cooldown.Default = value
		cfg.Cooldown.Ecosystems = map[string]string{"npm": value}
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", value, err)
		}
	}
}

func TestRejectsInvalidCooldownFromEnv(t *testing.T) {
	t.Setenv("PROXY_COOLDOWN_DEFAULT", "1w")
	cfg := Default()
	cfg.LoadFromEnv()
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "cooldown: invalid default") {
		t.Fatalf("Validate = %v, want invalid cooldown default", err)
	}
}
