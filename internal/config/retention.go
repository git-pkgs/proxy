package config

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/proxy/internal/retention"
	"github.com/git-pkgs/purl"
)

// parseDayDuration parses a duration that may use a "d" suffix for days, as
// cooldown values do. Day counts that are not finite or exceed
// maxDurationDays are rejected, since converting them to a time.Duration
// would overflow.
func parseDayDuration(s string) (time.Duration, error) {
	if num, ok := strings.CutSuffix(strings.TrimSpace(s), "d"); ok {
		days, err := strconv.ParseFloat(num, 64)
		if err == nil && (math.IsNaN(days) || math.IsInf(days, 0) || math.Abs(days) > maxDurationDays) {
			return 0, fmt.Errorf("invalid duration %q: day count out of range", s)
		}
	}
	return cooldown.ParseDuration(s)
}

// parseRetentionDuration parses one retention value. field names the
// setting in error messages.
func parseRetentionDuration(field, value string) (time.Duration, error) {
	d, err := parseDayDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", field, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("invalid %s %q: must not be negative", field, value)
	}
	return d, nil
}

// RetentionRules validates storage.retention against the ecosystems
// registered with reg and resolves it. An ecosystem or package that reg
// does not support is an error, so a rule is never silently ignored.
func (c *Config) RetentionRules(reg *retention.Registry) (retention.Rules, error) {
	r := c.Storage.Retention
	rules := retention.Rules{
		Ecosystems: map[string]time.Duration{},
		Packages:   map[string]time.Duration{},
	}

	var err error
	if rules.Default, err = parseRetentionDuration("storage.retention.default", r.Default); err != nil {
		return retention.Rules{}, err
	}

	if r.SweepInterval != "" {
		d, err := parseDayDuration(r.SweepInterval)
		if err != nil {
			return retention.Rules{}, fmt.Errorf("invalid storage.retention.sweep_interval: %w", err)
		}
		if d < minRetentionSweepInterval {
			return retention.Rules{}, fmt.Errorf("invalid storage.retention.sweep_interval %q: must be at least %s",
				r.SweepInterval, minRetentionSweepInterval)
		}
	}

	for _, key := range sortedKeys(r.Ecosystems) {
		field := "storage.retention.ecosystems." + key
		if !retention.IsKnown(key) {
			return retention.Rules{}, fmt.Errorf("invalid %s: unknown ecosystem %q (valid: %s)",
				field, key, strings.Join(retention.KnownKeys, ", "))
		}
		if _, ok := reg.Lookup(key); !ok {
			return retention.Rules{}, fmt.Errorf("invalid %s: retention is not supported for %q in this version", field, key)
		}
		d, err := parseRetentionDuration(field, r.Ecosystems[key])
		if err != nil {
			return retention.Rules{}, err
		}
		rules.Ecosystems[key] = d
	}

	for _, key := range sortedKeys(r.Packages) {
		field := fmt.Sprintf("storage.retention.packages[%q]", key)
		canonical, err := retentionPackageKey(reg, key)
		if err != nil {
			return retention.Rules{}, fmt.Errorf("invalid %s: %w", field, err)
		}
		d, err := parseRetentionDuration(field, r.Packages[key])
		if err != nil {
			return retention.Rules{}, err
		}
		if prev, ok := rules.Packages[canonical]; ok && prev != d {
			return retention.Rules{}, fmt.Errorf("invalid %s: conflicts with another key for package %s", field, canonical)
		}
		rules.Packages[canonical] = d
	}

	return rules, nil
}

// retentionPackageKey returns the stored package PURL a configured package
// key stands for.
func retentionPackageKey(reg *retention.Registry, key string) (string, error) {
	// A julia PURL needs a uuid qualifier to parse at all, and the julia
	// handler stores none, so no key could ever match one of its packages.
	if strings.HasPrefix(strings.ToLower(key), "pkg:julia/") {
		return "", fmt.Errorf("package rules are not supported for julia")
	}
	p, err := purl.Parse(key)
	if err != nil {
		return "", fmt.Errorf("not a valid package PURL: %w", err)
	}
	if p.Version != "" {
		return "", fmt.Errorf("package PURL must not carry a version")
	}
	canonical, ok := reg.Canonical(p)
	if !ok {
		return "", fmt.Errorf("retention is not supported for %s packages in this version", p.Type)
	}
	return canonical, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ParseRetentionSweepInterval returns how often the retention sweep runs.
func (c *Config) ParseRetentionSweepInterval() time.Duration {
	if c.Storage.Retention.SweepInterval == "" {
		return defaultRetentionSweepInterval
	}
	d, err := parseDayDuration(c.Storage.Retention.SweepInterval)
	if err != nil || d < minRetentionSweepInterval {
		return defaultRetentionSweepInterval
	}
	return d
}
