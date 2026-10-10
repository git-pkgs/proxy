package retention

import (
	"slices"
	"testing"
	"time"

	"github.com/git-pkgs/purl"
)

func TestSpecDBEcosystems(t *testing.T) {
	tests := []struct {
		key  string
		want []string
	}{
		{"npm", []string{"npm"}},
		{"gem", []string{"gem", "rubygems"}},
		{"composer", []string{"composer", "packagist"}},
		{"alpine", []string{"alpine", "apk"}},
		{"golang", []string{"golang"}},
		{"deb", []string{"deb"}},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			got := Spec{Key: tt.key}.dbEcosystems()
			if !slices.Equal(got, tt.want) {
				t.Errorf("dbEcosystems() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRegistryRegister(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Spec{Key: "gem"})
	reg.Register(Spec{Key: "npm"})

	if got := reg.Keys(); !slices.Equal(got, []string{"gem", "npm"}) {
		t.Errorf("Keys() = %v, want [gem npm]", got)
	}
	for _, value := range []string{"gem", "rubygems"} {
		if key, ok := reg.KeyForDBEcosystem(value); !ok || key != "gem" {
			t.Errorf("KeyForDBEcosystem(%q) = %q, %v, want gem, true", value, key, ok)
		}
	}
	if _, ok := reg.KeyForDBEcosystem("pypi"); ok {
		t.Error("KeyForDBEcosystem(pypi) found a key for an unregistered ecosystem")
	}
	if _, ok := reg.Lookup("pypi"); ok {
		t.Error("Lookup(pypi) found an unregistered ecosystem")
	}
}

func TestRegistryRegisterPanics(t *testing.T) {
	tests := []struct {
		name  string
		specs []Spec
	}{
		{"unknown key", []Spec{{Key: "generic"}}},
		{"duplicate key", []Spec{{Key: "npm"}, {Key: "npm"}}},
		{"overlapping ecosystem value", []Spec{{Key: "gem"}, {Key: "npm", ExtraDBEcosystems: []string{"rubygems"}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := NewRegistry()
			defer func() {
				if recover() == nil {
					t.Error("Register did not panic")
				}
			}()
			for _, s := range tt.specs {
				reg.Register(s)
			}
		})
	}
}

func mustParse(t *testing.T, s string) *purl.PURL {
	t.Helper()
	p, err := purl.Parse(s)
	if err != nil {
		t.Fatalf("parsing %q: %v", s, err)
	}
	return p
}

func TestDefaultCanonical(t *testing.T) {
	canonical := DefaultCanonical("npm")

	got, ok := canonical(mustParse(t, "pkg:npm/%40babel/core"))
	if !ok || got != "pkg:npm/%40babel/core" {
		t.Errorf("scoped package = %q, %v, want pkg:npm/%%40babel/core, true", got, ok)
	}
	if got, ok := canonical(mustParse(t, "pkg:pypi/requests")); ok {
		t.Errorf("other type accepted as %q", got)
	}
	if _, ok := canonical(nil); ok {
		t.Error("nil PURL accepted")
	}

	// alpine stores pkg:apk/alpine/{name} and deb stores pkg:deb/{name}, so
	// building their keys back from the full name gives a different package.
	// The default refuses them rather than produce keys that never match.
	for _, tt := range []struct{ key, purl string }{
		{"alpine", "pkg:apk/alpine/curl"},
		{"alpine", "pkg:apk/curl"},
		{"deb", "pkg:deb/debian/curl"},
	} {
		if got, ok := DefaultCanonical(tt.key)(mustParse(t, tt.purl)); ok {
			t.Errorf("DefaultCanonical(%q) accepted %s as %q", tt.key, tt.purl, got)
		}
	}
}

func TestRegistryCanonical(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Spec{Key: "julia"})
	reg.Register(Spec{Key: "npm", CanonicalPackage: DefaultCanonical("npm")})

	if got, ok := reg.Canonical(mustParse(t, "pkg:npm/lodash")); !ok || got != "pkg:npm/lodash" {
		t.Errorf("Canonical(npm) = %q, %v, want pkg:npm/lodash, true", got, ok)
	}
	if got, ok := reg.Canonical(mustParse(t, "pkg:cargo/serde")); ok {
		t.Errorf("Canonical(cargo) = %q for an unregistered ecosystem", got)
	}
}

func TestRulesFor(t *testing.T) {
	rules := Rules{
		Default:    30 * 24 * time.Hour,
		Ecosystems: map[string]time.Duration{"npm": 14 * 24 * time.Hour, "maven": 0},
		Packages:   map[string]time.Duration{"pkg:npm/lodash": 0, "pkg:maven/a/b": 90 * 24 * time.Hour},
	}
	tests := []struct {
		key, pkg string
		want     time.Duration
	}{
		{"npm", "pkg:npm/lodash", 0},
		{"npm", "pkg:npm/left-pad", 14 * 24 * time.Hour},
		{"maven", "pkg:maven/a/b", 90 * 24 * time.Hour},
		{"maven", "pkg:maven/c/d", 0},
		{"cargo", "pkg:cargo/serde", 30 * 24 * time.Hour},
	}
	for _, tt := range tests {
		if got := rules.For(tt.key, tt.pkg); got != tt.want {
			t.Errorf("For(%q, %q) = %v, want %v", tt.key, tt.pkg, got, tt.want)
		}
	}
}

func TestRulesMinPositive(t *testing.T) {
	if got := (Rules{}).MinPositive(); got != 0 {
		t.Errorf("empty rules MinPositive() = %v, want 0", got)
	}
	rules := Rules{
		Default:    0,
		Ecosystems: map[string]time.Duration{"npm": 14 * time.Hour, "maven": 0},
		Packages:   map[string]time.Duration{"pkg:npm/lodash": 2 * time.Hour},
	}
	if got := rules.MinPositive(); got != 2*time.Hour {
		t.Errorf("MinPositive() = %v, want 2h", got)
	}
}
