package handler

import "testing"

func TestPackageRoutesMatch(t *testing.T) {
	routes := newPackageRoutes(map[string]string{
		"@example/*":      "https://npm.example.com/scoped/",
		"@example/tool-*": "https://npm.example.com/tools",
		"example/*":       "https://composer.example.com/packages",
		"single-package":  "https://registry.example.com/single",
	})

	tests := []struct {
		name    string
		pkg     string
		wantURL string
		wantOK  bool
	}{
		{"scoped npm package", "@example/widgets", "https://npm.example.com/scoped", true},
		{"longest pattern wins", "@example/tool-kit", "https://npm.example.com/tools", true},
		{"composer vendor", "example/library", "https://composer.example.com/packages", true},
		{"exact name", "single-package", "https://registry.example.com/single", true},
		{"names are case-insensitive", "Example/Library", "https://composer.example.com/packages", true},
		{"wildcard does not cross slash", "example/library/extra", "", false},
		{"similar vendor does not match", "example-fork/library", "", false},
		{"unrouted package", "lodash", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			route, ok := routes.match(tt.pkg)
			if ok != tt.wantOK {
				t.Fatalf("match(%q) ok = %t, want %t", tt.pkg, ok, tt.wantOK)
			}
			if ok && route.url != tt.wantURL {
				t.Errorf("match(%q) url = %q, want %q", tt.pkg, route.url, tt.wantURL)
			}
		})
	}
}

func TestPackageRoutesCacheKeysAreDistinct(t *testing.T) {
	routes := newPackageRoutes(map[string]string{
		"example/*":       "https://one.example.com",
		"other-example/*": "https://two.example.com",
	})

	first, _ := routes.match("example/library")
	second, _ := routes.match("other-example/library")

	if first.cacheKey("example/library") == "example/library" {
		t.Error("routed cache key must differ from the unrouted package name")
	}
	if first.cacheKey("example/library") == second.cacheKey("example/library") {
		t.Error("routes with different upstreams must not share cache keys")
	}
	if containsPathTraversal(first.cacheKey("example/library")) {
		t.Error("routed cache key must be usable as a storage path")
	}
}
