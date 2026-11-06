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
