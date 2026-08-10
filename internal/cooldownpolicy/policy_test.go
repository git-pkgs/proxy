package cooldownpolicy

import (
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
