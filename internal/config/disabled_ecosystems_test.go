package config

import (
	"slices"
	"strings"
	"testing"
)

func TestDisabledEcosystemsDefaultToNone(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	for _, key := range DisableableEcosystems {
		if cfg.EcosystemDisabled(key) {
			t.Errorf("%s is disabled by default", key)
		}
	}
}

func TestDisabledEcosystemsAreNormalized(t *testing.T) {
	cfg := Default()
	cfg.DisabledEcosystems = []string{"PyPI", " apk ", "go", "docker", "debian", "brew", "rubygems"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	want := []string{"pypi", "alpine", "golang", "oci", "deb", "homebrew", "gem"}
	if !slices.Equal(cfg.DisabledEcosystems, want) {
		t.Errorf("DisabledEcosystems = %v, want %v", cfg.DisabledEcosystems, want)
	}
	for _, name := range []string{"pypi", "PyPI", "alpine", "apk", "golang", "go", "oci", "docker", "deb", "homebrew", "brew", "gem"} {
		if !cfg.EcosystemDisabled(name) {
			t.Errorf("EcosystemDisabled(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"npm", "cargo", "maven"} {
		if cfg.EcosystemDisabled(name) {
			t.Errorf("EcosystemDisabled(%q) = true, want false", name)
		}
	}
}

func TestDisabledEcosystemsRejectUnknownNames(t *testing.T) {
	for _, name := range []string{"pipy", "git", "generic", ""} {
		cfg := Default()
		cfg.DisabledEcosystems = []string{"npm", name}
		err := cfg.Validate()
		if err == nil {
			t.Errorf("%q: Validate accepted it", name)
			continue
		}
		if !strings.Contains(err.Error(), "disabled_ecosystems") {
			t.Errorf("%q: error %q doesn't name the setting", name, err)
		}
	}
}

func TestDisabledEcosystemsFromEnv(t *testing.T) {
	t.Setenv("PROXY_DISABLED_ECOSYSTEMS", "pypi, cargo")
	cfg := Default()
	cfg.LoadFromEnv()
	if !slices.Equal(cfg.DisabledEcosystems, []string{"pypi", "cargo"}) {
		t.Errorf("DisabledEcosystems = %v, want [pypi cargo]", cfg.DisabledEcosystems)
	}
}

// Entries are matched by key before Validate has normalized them too.
func TestEcosystemDisabledBeforeValidate(t *testing.T) {
	cfg := Default()
	cfg.DisabledEcosystems = []string{"PyPI", "apk"}
	if !cfg.EcosystemDisabled("pypi") || !cfg.EcosystemDisabled("alpine") || cfg.EcosystemDisabled("npm") {
		t.Errorf("EcosystemDisabled doesn't match unnormalized entries")
	}
}
