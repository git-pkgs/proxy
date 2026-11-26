package config

import (
	"fmt"
	"slices"
	"strings"
)

// DisableableEcosystems are the ecosystem keys disabled_ecosystems accepts:
// the ones served by default. git and generic serve only what upstream.git
// and upstream.generic configure, so they are turned off by leaving those
// out.
var DisableableEcosystems = []string{
	"alpine", "arch", "cargo", "chef", "composer", "conan", "conda", "cran", "deb",
	"freebsd", "gem", "golang", "gradle", "helm", "hex", "homebrew", "julia", "maven",
	"npm", "nuget", "oci", "pub", "pypi", "rpm", "swift", "terraform",
}

// normalizeEcosystem resolves case and the aliases scanning accepts, plus
// brew for homebrew.
func normalizeEcosystem(name string) string {
	key := NormalizeScannerEcosystem(name)
	if key == "brew" {
		return "homebrew"
	}
	return key
}

// validateDisabledEcosystems normalizes disabled_ecosystems in place and
// rejects names it doesn't accept.
func (c *Config) validateDisabledEcosystems() error {
	for i, name := range c.DisabledEcosystems {
		key := normalizeEcosystem(name)
		if !slices.Contains(DisableableEcosystems, key) {
			return fmt.Errorf("invalid disabled_ecosystems entry %q (valid: %s)", name, strings.Join(DisableableEcosystems, ", "))
		}
		c.DisabledEcosystems[i] = key
	}
	return nil
}

// EcosystemDisabled reports whether disabled_ecosystems turns off the
// ecosystem, given by its key or an alias such as apk, go or docker.
func (c *Config) EcosystemDisabled(name string) bool {
	key := normalizeEcosystem(name)
	return slices.ContainsFunc(c.DisabledEcosystems, func(entry string) bool {
		return normalizeEcosystem(entry) == key
	})
}
