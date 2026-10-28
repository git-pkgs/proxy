package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/database"
)

func testCooldownView(t *testing.T, cfg config.CooldownConfig) *cooldownView {
	t.Helper()
	policy, err := cooldown.NewPolicy(&cooldown.Config{
		Default:    cfg.Default,
		Ecosystems: cfg.Ecosystems,
		Packages:   cfg.NormalizedPackages(),
	}, cfg.PackagePatterns)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	v, err := newCooldownView(cfg, policy)
	if err != nil {
		t.Fatalf("newCooldownView: %v", err)
	}
	return v
}

func TestCooldownViewForPackage(t *testing.T) {
	v := testCooldownView(t, config.CooldownConfig{
		Default:         "3d",
		Ecosystems:      map[string]string{"npm": "7d", "composer": "48h"},
		Packages:        map[string]string{"pkg:composer/phpunit/phpunit": "0"},
		PackagePatterns: map[string]string{"pkg:npm/@corp/*": "0", "pkg:composer/acme/*": "12h"},
	})
	tests := []struct {
		ecosystem, name string
		want            PackageCooldown
	}{
		{"cargo", "serde", PackageCooldown{"3d", "default"}},
		{"npm", "lodash", PackageCooldown{"7d", "ecosystem override"}},
		{"composer", "monolog/monolog", PackageCooldown{"2d", "ecosystem override"}},
		{"composer", "phpunit/phpunit", PackageCooldown{"off", "package override"}},
		{"composer", "acme/tool", PackageCooldown{"12h", "package pattern"}},
		{"npm", "@corp/ui", PackageCooldown{"off", "package pattern"}},
	}
	for _, tt := range tests {
		got := v.forPackage(tt.ecosystem, tt.name)
		if got == nil || *got != tt.want {
			t.Errorf("forPackage(%s, %s) = %+v, want %+v", tt.ecosystem, tt.name, got, tt.want)
		}
	}
}

func TestCooldownViewOff(t *testing.T) {
	var unset *cooldownView
	for _, v := range []*cooldownView{unset, testCooldownView(t, config.CooldownConfig{})} {
		if got := v.forPackage("npm", "lodash"); got != nil {
			t.Errorf("forPackage with cooldown off = %+v, want nil", got)
		}
		if got := v.summary(); got.Enabled {
			t.Errorf("summary with cooldown off = %+v, want disabled", got)
		}
	}
}

// TestCooldownViewOnlyPattern checks that a pattern alone turns cooldown on.
func TestCooldownViewOnlyPattern(t *testing.T) {
	v := testCooldownView(t, config.CooldownConfig{PackagePatterns: map[string]string{"pkg:npm/*": "1d"}})
	if got := v.forPackage("npm", "lodash"); got == nil || *got != (PackageCooldown{"1d", "package pattern"}) {
		t.Errorf("forPackage = %+v, want 1d from the pattern", got)
	}
	if got := v.forPackage("cargo", "serde"); got == nil || *got != (PackageCooldown{"off", "default"}) {
		t.Errorf("forPackage outside the pattern = %+v, want off by default", got)
	}
}

func TestFormatCooldown(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                               "off",
		72 * time.Hour:                  "3d",
		36 * time.Hour:                  "36h",
		90 * time.Minute:                "90m",
		90*time.Minute + 30*time.Second: "1h30m30s",
	} {
		if got := formatCooldown(d); got != want {
			t.Errorf("formatCooldown(%v) = %q, want %q", d, got, want)
		}
	}
}

// TestCooldownPages checks that the dashboard shows the configuration
// without the names of package overrides and patterns, and the package page
// the cooldown that applies to the package.
func TestCooldownPages(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()
	ts.server.cooldown = testCooldownView(t, config.CooldownConfig{
		Default:         "3d",
		Ecosystems:      map[string]string{"npm": "7d"},
		Packages:        map[string]string{"pkg:npm/internal-secret": "0"},
		PackagePatterns: map[string]string{"pkg:npm/@corp-secret/*": "0"},
	})
	if err := ts.db.UpsertPackage(&database.Package{PURL: "pkg:npm/lodash", Ecosystem: "npm", Name: "lodash"}); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}

	get := func(path string) string {
		t.Helper()
		w := httptest.NewRecorder()
		ts.handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d", path, w.Code)
		}
		return w.Body.String()
	}

	dashboard := get("/ui/")
	for _, want := range []string{"Cooldown", "3d", "npm:", "7d", "Package overrides:", "Package patterns:"} {
		if !strings.Contains(dashboard, want) {
			t.Errorf("dashboard is missing %q", want)
		}
	}
	for _, secret := range []string{"internal-secret", "corp-secret"} {
		if strings.Contains(dashboard, secret) {
			t.Errorf("dashboard shows %q", secret)
		}
	}

	page := get("/ui/package/npm/lodash")
	if !strings.Contains(page, "7d") || !strings.Contains(page, "(ecosystem override)") {
		t.Error("package page doesn't show the 7d ecosystem override")
	}
}
