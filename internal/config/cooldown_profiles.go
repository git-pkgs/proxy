package config

import (
	"fmt"
	"maps"
	"regexp"
	"slices"

	"github.com/git-pkgs/cooldown"
)

var cooldownProfileName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Policy builds the policy for this URL's cooldown settings.
func (c *CooldownConfig) Policy() (*cooldown.Policy, error) {
	return cooldown.NewPolicy(&cooldown.Config{
		Default: c.Default, Ecosystems: c.Ecosystems, Packages: c.NormalizedPackages(),
	}, c.PackagePatterns)
}

// ValidateProfiles checks names and policies before they become routes.
func (c *CooldownConfig) ValidateProfiles() error {
	for _, name := range slices.Sorted(maps.Keys(c.Profiles)) {
		profile := c.Profiles[name]
		if !cooldownProfileName.MatchString(name) {
			return fmt.Errorf("invalid cooldown profile name %q: use lowercase letters, digits, hyphens or underscores", name)
		}
		if len(profile.Profiles) > 0 {
			return fmt.Errorf("cooldown.profiles.%s cannot contain profiles", name)
		}
		if err := profile.validateProfileDurations(); err != nil {
			return fmt.Errorf("cooldown.profiles.%s: %w", name, err)
		}
		if _, err := profile.Policy(); err != nil {
			return fmt.Errorf("cooldown.profiles.%s: %w", name, err)
		}
	}
	return nil
}

func (c *CooldownConfig) validateProfileDurations() error {
	values := map[string]string{"default": c.Default}
	for group, overrides := range map[string]map[string]string{
		"ecosystems": c.Ecosystems, "packages": c.Packages, "package_patterns": c.PackagePatterns,
	} {
		for key, value := range overrides {
			values[group+"."+key] = value
		}
	}
	for _, key := range slices.Sorted(maps.Keys(values)) {
		value := values[key]
		if key == "default" && value == "" {
			continue
		}
		duration, err := cooldown.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("invalid %s: %w", key, err)
		}
		if duration < 0 {
			return fmt.Errorf("invalid %s: duration must be non-negative", key)
		}
	}
	return nil
}
