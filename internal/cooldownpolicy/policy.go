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
}

type pattern struct {
	glob     string
	duration time.Duration
}

// New creates a Policy using the supplied exact and pattern overrides.
func New(base *cooldown.Config, packagePatterns map[string]string) (*Policy, error) {
	if base == nil {
		base = &cooldown.Config{}
	}

	patterns := make([]pattern, 0, len(packagePatterns))
	for glob, value := range packagePatterns {
		canonicalGlob := strings.ReplaceAll(glob, "@", "%40")
		if _, err := path.Match(canonicalGlob, ""); err != nil {
			return nil, fmt.Errorf("invalid cooldown package pattern %q: %w", glob, err)
		}
		duration, err := cooldown.ParseDuration(value)
		if err != nil {
			return nil, fmt.Errorf("invalid cooldown duration for package pattern %q: %w", glob, err)
		}
		patterns = append(patterns, pattern{glob: canonicalGlob, duration: duration})
	}
	sort.Slice(patterns, func(i, j int) bool {
		left, right := literalLength(patterns[i].glob), literalLength(patterns[j].glob)
		if left != right {
			return left > right
		}
		return patterns[i].glob < patterns[j].glob
	})

	return &Policy{base: base, patterns: patterns}, nil
}

func literalLength(glob string) int {
	return len(glob) - strings.Count(glob, "*") - strings.Count(glob, "?")
}

// IsAllowed reports whether a version published at publishedAt has completed its
// cooldown. Exact package overrides take precedence over package patterns.
func (p *Policy) IsAllowed(ecosystem, packagePURL string, publishedAt time.Time) bool {
	if _, exact := p.base.Packages[packagePURL]; exact {
		return p.base.IsAllowed(ecosystem, packagePURL, publishedAt)
	}

	for _, candidate := range p.patterns {
		matched, _ := path.Match(candidate.glob, packagePURL)
		if !matched {
			continue
		}
		return candidate.duration == 0 || publishedAt.IsZero() || time.Since(publishedAt) >= candidate.duration
	}

	return p.base.IsAllowed(ecosystem, packagePURL, publishedAt)
}

// Enabled reports whether any configured cooldown can filter a package version.
func (p *Policy) Enabled() bool {
	if p.base.Enabled() {
		return true
	}
	for _, candidate := range p.patterns {
		if candidate.duration > 0 {
			return true
		}
	}
	return false
}
