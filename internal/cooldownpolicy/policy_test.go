package cooldownpolicy

import (
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

func TestPatternOverride(t *testing.T) {
	policy, err := New(&cooldown.Config{
		Default:    "7d",
		Ecosystems: map[string]string{"npm": "7d"},
	}, map[string]string{
		"pkg:npm/@example/*": "0",
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	if !policy.IsAllowed("npm", "pkg:npm/%40example/widget", time.Now()) {
		t.Fatal("matching package pattern should disable cooldown")
	}
	if policy.IsAllowed("npm", "pkg:npm/public-package", time.Now()) {
		t.Fatal("non-matching package should use ecosystem cooldown")
	}
}

func TestExactOverrideTakesPrecedenceOverPattern(t *testing.T) {
	purl := "pkg:npm/%40example/widget"
	policy, err := New(&cooldown.Config{
		Default:  "7d",
		Packages: map[string]string{purl: "2d"},
	}, map[string]string{
		"pkg:npm/@example/*": "0",
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	if policy.IsAllowed("npm", purl, time.Now()) {
		t.Fatal("exact package override should take precedence over pattern")
	}
}

func TestMoreSpecificPatternTakesPrecedence(t *testing.T) {
	policy, err := New(&cooldown.Config{Default: "7d"}, map[string]string{
		"pkg:npm/@example/*":        "0",
		"pkg:npm/@example/critical": "2d",
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	if policy.IsAllowed("npm", "pkg:npm/%40example/critical", time.Now()) {
		t.Fatal("more specific pattern should take precedence")
	}
}

func TestNewRejectsInvalidPattern(t *testing.T) {
	if _, err := New(&cooldown.Config{}, map[string]string{"pkg:npm/[": "0"}); err == nil {
		t.Fatal("New should reject an invalid package pattern")
	}
}

func TestNormalizedPatternCollisions(t *testing.T) {
	for _, duration := range []string{"0", "24h"} {
		t.Run(duration, func(t *testing.T) {
			for range 20 {
				policy, err := New(nil, map[string]string{
					"pkg:npm/@example/*":   "1d",
					"pkg:npm/%40example/*": duration,
				})
				if duration == "0" {
					if err == nil || !strings.Contains(err.Error(), `conflicting cooldown package patterns "pkg:npm/%40example/*" and "pkg:npm/@example/*"`) {
						t.Fatalf("conflict error = %v", err)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				if got := policy.For("npm", "pkg:npm/%40example/widget"); got != 24*time.Hour {
					t.Fatalf("duration = %s", got)
				}
			}
		})
	}
}

func TestForPrecedence(t *testing.T) {
	policy, err := New(&cooldown.Config{
		Default:    "1h",
		Ecosystems: map[string]string{"npm": "2h"},
		Packages:   map[string]string{"pkg:npm/%40example/exact": "0"},
	}, map[string]string{
		"pkg:npm/@example/*":         "3h",
		"pkg:npm/@example/specific*": "4h",
		"pkg:npm/@example/ab*":       "5h",
		"pkg:npm/@example/a*c":       "6h",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		ecosystem, purl string
		want            time.Duration
	}{
		{"npm", "pkg:npm/%40example/exact", 0},
		{"npm", "pkg:npm/%40example/widget", 3 * time.Hour},
		{"npm", "pkg:npm/%40example/specific-widget", 4 * time.Hour},
		{"npm", "pkg:npm/%40example/abc", 6 * time.Hour},
		{"npm", "pkg:npm/other", 2 * time.Hour},
		{"npm", "pkg:npm/%40example/nested/package", 2 * time.Hour},
		{"cargo", "pkg:cargo/serde", time.Hour},
	} {
		t.Run(tc.purl, func(t *testing.T) {
			if got := policy.For(tc.ecosystem, tc.purl); got != tc.want {
				t.Errorf("For = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestPatternOnlyPolicy(t *testing.T) {
	policy, err := New(nil, map[string]string{"pkg:nuget/example.*": "24h"})
	if err != nil {
		t.Fatal(err)
	}
	if !policy.Enabled() {
		t.Fatal("pattern-only policy should be enabled")
	}
	for _, tc := range []struct {
		published time.Time
		allowed   bool
	}{
		{time.Now().Add(-time.Hour), false},
		{time.Now().Add(-48 * time.Hour), true},
		{time.Time{}, true},
	} {
		if got := policy.IsAllowed("nuget", "pkg:nuget/example.widget", tc.published); got != tc.allowed {
			t.Errorf("published=%s: allowed=%t, want %t", tc.published, got, tc.allowed)
		}
	}
	if !policy.IsAllowed("nuget", "pkg:nuget/other", time.Now()) {
		t.Error("unmatched package should have no cooldown")
	}
}

func TestDisabledPolicy(t *testing.T) {
	for _, patterns := range []map[string]string{nil, {"pkg:npm/@example/*": "0"}} {
		policy, err := New(nil, patterns)
		if err != nil {
			t.Fatal(err)
		}
		if policy.Enabled() {
			t.Error("zero cooldown should be disabled")
		}
	}
}

func TestNewRejectsInvalidDuration(t *testing.T) {
	if _, err := New(nil, map[string]string{"pkg:npm/*": "invalid"}); err == nil {
		t.Fatal("invalid duration accepted")
	}
}

func TestEvaluate(t *testing.T) {
	policy, err := New(&cooldown.Config{
		Default:    "48h",
		Ecosystems: map[string]string{"npm": "72h"},
		Packages:   map[string]string{"pkg:npm/%40example/exact": "0"},
	}, map[string]string{"pkg:npm/@example/*": "7d"})
	if err != nil {
		t.Fatal(err)
	}
	published := time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC)
	eligible := published.Add(7 * 24 * time.Hour)
	for _, tc := range []struct {
		name, ecosystem, purl string
		published, at         time.Time
		want                  cooldown.Decision
	}{
		{"waiting", "npm", "pkg:npm/%40example/widget", published, eligible.Add(-time.Nanosecond), cooldown.Decision{Cooldown: 7 * 24 * time.Hour, AvailableAt: eligible, Reason: cooldown.ReasonWaiting}},
		{"boundary", "npm", "pkg:npm/%40example/widget", published, eligible, cooldown.Decision{Allowed: true, Cooldown: 7 * 24 * time.Hour, AvailableAt: eligible, Reason: cooldown.ReasonElapsed}},
		{"elapsed", "npm", "pkg:npm/%40example/widget", published, eligible.Add(time.Hour), cooldown.Decision{Allowed: true, Cooldown: 7 * 24 * time.Hour, AvailableAt: eligible, Reason: cooldown.ReasonElapsed}},
		{"unknown publication", "npm", "pkg:npm/%40example/widget", time.Time{}, eligible, cooldown.Decision{Allowed: true, Cooldown: 7 * 24 * time.Hour, Reason: cooldown.ReasonUnknownPublicationTime}},
		{"exact exemption", "npm", "pkg:npm/%40example/exact", published, published, cooldown.Decision{Allowed: true, AvailableAt: published, Reason: cooldown.ReasonDisabled}},
		{"exemption without publication", "npm", "pkg:npm/%40example/exact", time.Time{}, eligible, cooldown.Decision{Allowed: true, Reason: cooldown.ReasonUnknownPublicationTime}},
		{"ecosystem fallback", "npm", "pkg:npm/other", published, published, cooldown.Decision{Cooldown: 72 * time.Hour, AvailableAt: published.Add(72 * time.Hour), Reason: cooldown.ReasonWaiting}},
		{"global fallback", "cargo", "pkg:cargo/serde", published, published, cooldown.Decision{Cooldown: 48 * time.Hour, AvailableAt: published.Add(48 * time.Hour), Reason: cooldown.ReasonWaiting}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := policy.Evaluate(tc.ecosystem, tc.purl, tc.published, tc.at); got != tc.want {
				t.Errorf("Evaluate = %+v, want %+v", got, tc.want)
			}
		})
	}
}
