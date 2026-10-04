// Package cooldownpolicy applies package-pattern overrides to cooldown checks.
package cooldownpolicy

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/git-pkgs/cooldown"
)

// Policy applies exact PURL overrides before package-pattern overrides.
type Policy struct {
	base     *cooldown.Config
	patterns []pattern
	enabled  bool
}

type pattern struct {
	glob     string
	duration time.Duration
	config   *cooldown.Config
}

// New creates a Policy using the supplied exact and pattern overrides.
func New(base *cooldown.Config, packagePatterns map[string]string) (*Policy, error) {
	if base == nil {
		base = &cooldown.Config{}
	}

	keys := make([]string, 0, len(packagePatterns))
	for glob := range packagePatterns {
		keys = append(keys, glob)
	}
	sort.Strings(keys)
	patterns := make([]pattern, 0, len(packagePatterns))
	seen := make(map[string]pattern)
	enabled := base.Enabled()
	for _, glob := range keys {
		value := packagePatterns[glob]
		canonicalGlob := strings.ReplaceAll(glob, "@", "%40")
		if _, err := path.Match(canonicalGlob, ""); err != nil {
			return nil, fmt.Errorf("invalid cooldown package pattern %q: %w", glob, err)
		}
		duration, err := cooldown.ParseDuration(value)
		if err != nil {
			return nil, fmt.Errorf("invalid cooldown duration for package pattern %q: %w", glob, err)
		}
		if previous, exists := seen[canonicalGlob]; exists {
			if previous.duration != duration {
				return nil, fmt.Errorf("conflicting cooldown package patterns %q and %q", previous.glob, glob)
			}
			continue
		}
		seen[canonicalGlob] = pattern{glob: glob, duration: duration}
		config := &cooldown.Config{Default: value}
		enabled = config.Enabled() || enabled
		patterns = append(patterns, pattern{glob: canonicalGlob, duration: duration, config: config})
	}
	sort.Slice(patterns, func(i, j int) bool {
		left, right := literalLength(patterns[i].glob), literalLength(patterns[j].glob)
		if left != right {
			return left > right
		}
		return patterns[i].glob < patterns[j].glob
	})

	return &Policy{base: base, patterns: patterns, enabled: enabled}, nil
}

func literalLength(glob string) int {
	return len(glob) - strings.Count(glob, "*") - strings.Count(glob, "?")
}

// For returns the duration, with exact overrides taking precedence over patterns.
func (p *Policy) For(ecosystem, packagePURL string) time.Duration {
	return p.configFor(packagePURL).For(ecosystem, packagePURL)
}

func (p *Policy) configFor(packagePURL string) *cooldown.Config {
	if _, exact := p.base.Packages[packagePURL]; exact {
		return p.base
	}

	for _, candidate := range p.patterns {
		matched, _ := path.Match(candidate.glob, packagePURL)
		if !matched {
			continue
		}
		return candidate.config
	}

	return p.base
}

// IsAllowed reports whether the package version has completed its cooldown.
func (p *Policy) IsAllowed(ecosystem, packagePURL string, publishedAt time.Time) bool {
	return p.Evaluate(ecosystem, packagePURL, publishedAt, time.Now()).Allowed
}

// Evaluate returns the cooldown decision at the supplied evaluation time.
func (p *Policy) Evaluate(ecosystem, packagePURL string, publishedAt, evaluatedAt time.Time) cooldown.Decision {
	return p.configFor(packagePURL).Evaluate(ecosystem, packagePURL, publishedAt, evaluatedAt)
}

// Enabled reports whether any configured cooldown can filter a package version.
func (p *Policy) Enabled() bool {
	return p.enabled
}
