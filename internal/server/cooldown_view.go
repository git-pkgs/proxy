package server

import (
	"fmt"
	"sort"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/packageurl"
)

// CooldownSummary describes the cooldown configuration on the dashboard. The
// UI has no auth, so package overrides and patterns are only counted: their
// names often list internal packages.
type CooldownSummary struct {
	Enabled          bool
	Default          string
	Ecosystems       []CooldownOverride
	PackageOverrides int
	PackagePatterns  int
}

// CooldownOverride is one ecosystem's cooldown.
type CooldownOverride struct {
	Ecosystem string
	Duration  string
}

// PackageCooldown is the cooldown that applies to one package, and the kind
// of rule it comes from.
type PackageCooldown struct {
	Duration string
	Rule     string
}

// cooldownView answers the UI's cooldown questions from the configuration.
type cooldownView struct {
	cfg      config.CooldownConfig
	policy   *cooldown.Policy
	packages map[string]string
	// patterns sets every package pattern to the same non-zero duration over
	// a zero default, so a non-zero result means a pattern matched, with the
	// policy's own matching.
	patterns *cooldown.Policy
}

func newCooldownView(cfg config.CooldownConfig, policy *cooldown.Policy) (*cooldownView, error) {
	probe := make(map[string]string, len(cfg.PackagePatterns))
	for glob := range cfg.PackagePatterns {
		probe[glob] = "1h"
	}
	patterns, err := cooldown.NewPolicy(nil, probe)
	if err != nil {
		return nil, fmt.Errorf("configuring cooldown package patterns: %w", err)
	}
	return &cooldownView{cfg: cfg, policy: policy, packages: cfg.NormalizedPackages(), patterns: patterns}, nil
}

func (v *cooldownView) enabled() bool {
	return v != nil && v.policy != nil && v.policy.Enabled()
}

// summary returns the dashboard's view of the configuration.
func (v *cooldownView) summary() CooldownSummary {
	if !v.enabled() {
		return CooldownSummary{}
	}
	s := CooldownSummary{
		Enabled:          true,
		Default:          formatConfigDuration(v.cfg.Default),
		PackageOverrides: len(v.cfg.Packages),
		PackagePatterns:  len(v.cfg.PackagePatterns),
	}
	for eco, d := range v.cfg.Ecosystems {
		s.Ecosystems = append(s.Ecosystems, CooldownOverride{Ecosystem: eco, Duration: formatConfigDuration(d)})
	}
	sort.Slice(s.Ecosystems, func(i, j int) bool { return s.Ecosystems[i].Ecosystem < s.Ecosystems[j].Ecosystem })
	return s
}

// forPackage returns the cooldown for a package, or nil with cooldown off.
func (v *cooldownView) forPackage(ecosystem, name string) *PackageCooldown {
	if !v.enabled() {
		return nil
	}
	purl := packageurl.MakeString(ecosystem, name, "")
	rule := "default"
	if _, ok := v.packages[purl]; ok {
		rule = "package override"
	} else if v.patterns.For(ecosystem, purl) != 0 {
		rule = "package pattern"
	} else if _, ok := v.cfg.Ecosystems[ecosystem]; ok {
		rule = "ecosystem override"
	}
	return &PackageCooldown{Duration: formatCooldown(v.policy.For(ecosystem, purl)), Rule: rule}
}

func formatConfigDuration(s string) string {
	d, err := cooldown.ParseDuration(s)
	if err != nil {
		return s
	}
	return formatCooldown(d)
}

// formatCooldown writes d as the configuration does: "3d", "12h", "30m",
// and "off" for zero.
func formatCooldown(d time.Duration) string {
	const day = 24 * time.Hour
	switch {
	case d <= 0:
		return "off"
	case d%day == 0:
		return fmt.Sprintf("%dd", d/day)
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return d.String()
}
