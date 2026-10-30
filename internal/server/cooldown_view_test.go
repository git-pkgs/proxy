package server

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"slices"
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

func TestCooldownViewUnsupportedEcosystem(t *testing.T) {
	v := testCooldownView(t, config.CooldownConfig{
		Default:    "3d",
		Ecosystems: map[string]string{"deb": "0", "npm": "7d"},
	})
	for _, eco := range []string{"deb", "alpine", "oci", "generic"} {
		if got := v.forPackage(eco, "pkg"); got != nil {
			t.Errorf("forPackage(%s) = %+v, want nil", eco, got)
		}
	}
	want := []CooldownOverride{{"deb", "off", true}, {"npm", "7d", false}}
	if got := v.summary().Ecosystems; !slices.Equal(got, want) {
		t.Errorf("summary ecosystems = %+v, want %+v", got, want)
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

func TestPackagePageWithheldVersions(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()
	ts.server.cooldown = testCooldownView(t, config.CooldownConfig{Default: "3d"})

	const pkg = "pkg:npm/lodash"
	if err := ts.db.UpsertPackage(&database.Package{PURL: pkg, Ecosystem: "npm", Name: "lodash"}); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}
	now := time.Now()
	held := now.Add(-time.Hour)
	versions := map[string]time.Time{
		"1.0.0": now.Add(-30 * 24 * time.Hour), // fetched
		"1.1.0": now.Add(-time.Hour),           // fetched while held back
		"2.0.0": now.Add(-20 * 24 * time.Hour), // only seen, left cooldown
		"2.1.0": held,                          // only seen, held back
	}
	for v, at := range versions {
		if err := ts.db.SetVersionPublishedAt(pkg+"@"+v, pkg, at); err != nil {
			t.Fatalf("SetVersionPublishedAt: %v", err)
		}
	}
	// No publish time: the download checks never stored one.
	if err := ts.db.UpsertVersion(&database.Version{PURL: pkg + "@0.9.0", PackagePURL: pkg}); err != nil {
		t.Fatalf("UpsertVersion: %v", err)
	}
	for _, v := range []string{"1.0.0", "1.1.0"} {
		if err := ts.db.UpsertArtifact(&database.Artifact{
			VersionPURL: pkg + "@" + v, Filename: "lodash-" + v + ".tgz", UpstreamURL: "https://example.com/" + v,
		}); err != nil {
			t.Fatalf("UpsertArtifact: %v", err)
		}
	}

	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ui/package/npm/lodash", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	page := w.Body.String()

	for _, v := range []string{"1.0.0", "1.1.0", "2.1.0"} {
		if !strings.Contains(page, "lodash@"+v) {
			t.Errorf("page is missing %s", v)
		}
	}
	for _, v := range []string{"2.0.0", "0.9.0"} {
		if strings.Contains(page, "lodash@"+v) {
			t.Errorf("page lists %s, which nobody fetched and cooldown no longer holds back", v)
		}
	}
	until := held.Add(3 * 24 * time.Hour).UTC().Format("2006-01-02 15:04")
	if got := strings.Count(page, "held back until"); got != 2 {
		t.Errorf("page marks %d versions as held back, want 2 (1.1.0 and 2.1.0)", got)
	}
	if !strings.Contains(page, "held back until "+until+" UTC") {
		t.Errorf("page is missing the availability of 2.1.0, %s", until)
	}
}

func TestPackageVersionsCooldownOff(t *testing.T) {
	var off *cooldownView
	pkg := &database.Package{PURL: "pkg:npm/lodash", Ecosystem: "npm", Name: "lodash"}
	recent := sql.NullTime{Time: time.Now().Add(-time.Hour), Valid: true}
	versions := []database.Version{
		{PURL: "pkg:npm/lodash@1.0.0", PublishedAt: recent},
		{PURL: "pkg:npm/lodash@2.0.0", PublishedAt: recent},
	}
	got := off.packageVersions(pkg, versions, map[string]bool{"pkg:npm/lodash@1.0.0": true}, time.Now())
	if len(got) != 1 || got[0].PURL != "pkg:npm/lodash@1.0.0" || !got[0].AvailableAt.IsZero() {
		t.Errorf("packageVersions with cooldown off = %+v, want only the fetched 1.0.0", got)
	}
}

func TestAPIPackageCooldown(t *testing.T) {
	h := &APIHandler{cooldown: testCooldownView(t, config.CooldownConfig{
		Default:  "3d",
		Packages: map[string]string{"pkg:npm/leftpad": "0"},
	})}
	tests := []struct{ ecosystem, name, want string }{
		{"npm", "lodash", "3d"},
		{"npm", "leftpad", "0"},
		{"deb", "curl", ""},
	}
	for _, tt := range tests {
		if got := h.packageCooldown(tt.ecosystem, tt.name); got != tt.want {
			t.Errorf("packageCooldown(%s, %s) = %q, want %q", tt.ecosystem, tt.name, got, tt.want)
		}
	}
	if got := (&APIHandler{}).packageCooldown("npm", "lodash"); got != "" {
		t.Errorf("packageCooldown with cooldown off = %q, want empty", got)
	}

	now := time.Now()
	published := now.Add(-time.Hour)
	if got, want := h.cooldown.availableAt("npm", "lodash", published, now), published.Add(72*time.Hour); !got.Equal(want) {
		t.Errorf("availableAt = %v, want %v", got, want)
	}
	if got := h.cooldown.availableAt("npm", "lodash", now.Add(-96*time.Hour), now); !got.IsZero() {
		t.Errorf("availableAt after cooldown = %v, want zero", got)
	}
}
