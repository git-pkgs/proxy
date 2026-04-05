package handler

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/cooldown"
)

func TestComposerRewriteMetadata(t *testing.T) {
	h := &ComposerHandler{
		proxy:    testProxy(),
		proxyURL: "http://localhost:8080",
	}

	input := `{
		"packages": {
			"symfony/console": [
				{
					"version": "6.0.0",
					"dist": {
						"url": "https://repo.packagist.org/files/symfony/console/6.0.0/abc123.zip",
						"type": "zip"
					}
				}
			]
		}
	}`

	output, err := h.rewriteMetadata([]byte(input))
	if err != nil {
		t.Fatalf("rewriteMetadata failed: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	packages := result["packages"].(map[string]any)
	versions := packages["symfony/console"].([]any)
	v := versions[0].(map[string]any)
	dist := v["dist"].(map[string]any)

	expected := "http://localhost:8080/composer/files/symfony/console/6.0.0/abc123.zip"
	if dist["url"] != expected {
		t.Errorf("dist url = %q, want %q", dist["url"], expected)
	}
}

func TestComposerRewriteMetadataExpandsMinified(t *testing.T) {
	h := &ComposerHandler{
		proxy:    testProxy(),
		proxyURL: "http://localhost:8080",
	}

	// Minified format: first version has all fields, subsequent versions
	// only include fields that changed. The proxy must expand this so every
	// version has all fields (including "name").
	input := `{
		"minified": "composer/2.0",
		"packages": {
			"symfony/console": [
				{
					"name": "symfony/console",
					"description": "Symfony Console Component",
					"version": "6.0.0",
					"dist": {
						"url": "https://repo.packagist.org/files/symfony/console/6.0.0/abc123.zip",
						"type": "zip"
					}
				},
				{
					"version": "5.4.0",
					"dist": {
						"url": "https://repo.packagist.org/files/symfony/console/5.4.0/def456.zip",
						"type": "zip"
					}
				}
			]
		}
	}`

	output, err := h.rewriteMetadata([]byte(input))
	if err != nil {
		t.Fatalf("rewriteMetadata failed: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	// The minified key should be removed from output
	if _, ok := result["minified"]; ok {
		t.Error("expected minified key to be removed from output")
	}

	packages := result["packages"].(map[string]any)
	versions := packages["symfony/console"].([]any)

	// Second version should have inherited the "name" and "description" fields
	v1 := versions[1].(map[string]any)
	if v1["name"] != "symfony/console" {
		t.Errorf("second version name = %v, want %q", v1["name"], "symfony/console")
	}
	if v1["description"] != "Symfony Console Component" {
		t.Errorf("second version description = %v, want %q", v1["description"], "Symfony Console Component")
	}
}

func TestComposerRewriteMetadataMinifiedDevReset(t *testing.T) {
	h := &ComposerHandler{
		proxy:    testProxy(),
		proxyURL: "http://localhost:8080",
	}

	// The ~dev sentinel resets the inheritance chain for dev versions.
	input := `{
		"minified": "composer/2.0",
		"packages": {
			"symfony/console": [
				{
					"name": "symfony/console",
					"description": "Symfony Console Component",
					"license": ["MIT"],
					"version": "6.0.0",
					"dist": {
						"url": "https://repo.packagist.org/files/symfony/console/6.0.0/abc123.zip",
						"type": "zip"
					}
				},
				"~dev",
				{
					"name": "symfony/console",
					"version": "dev-main",
					"dist": {
						"url": "https://repo.packagist.org/files/symfony/console/dev-main/xyz789.zip",
						"type": "zip"
					}
				}
			]
		}
	}`

	output, err := h.rewriteMetadata([]byte(input))
	if err != nil {
		t.Fatalf("rewriteMetadata failed: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	packages := result["packages"].(map[string]any)
	versions := packages["symfony/console"].([]any)

	if len(versions) != 2 {
		t.Fatalf("expected 2 versions, got %d", len(versions))
	}

	// Dev version should NOT have inherited "license" or "description"
	// from the tagged version (the ~dev sentinel resets inheritance).
	devVersion := versions[1].(map[string]any)
	if devVersion["version"] != "dev-main" {
		t.Errorf("dev version = %v, want %q", devVersion["version"], "dev-main")
	}
	if _, ok := devVersion["license"]; ok {
		t.Error("dev version should not have inherited license field after ~dev reset")
	}
	if _, ok := devVersion["description"]; ok {
		t.Error("dev version should not have inherited description field after ~dev reset")
	}
}

func TestComposerRewriteMetadataCooldownPreservesNames(t *testing.T) {
	now := time.Now()
	old := now.Add(-10 * 24 * time.Hour).Format(time.RFC3339)
	veryOld := now.Add(-20 * 24 * time.Hour).Format(time.RFC3339)
	recent := now.Add(-1 * time.Hour).Format(time.RFC3339)

	proxy := &Proxy{Logger: slog.Default()}
	proxy.Cooldown = &cooldown.Config{Default: "3d"}

	h := &ComposerHandler{
		proxy:    proxy,
		proxyURL: "http://localhost:8080",
	}

	// Minified format where "name" only appears in first version.
	// When cooldown filters the first version, remaining versions must
	// still have the "name" field after expansion.
	input := `{
		"minified": "composer/2.0",
		"packages": {
			"symfony/console": [
				{
					"name": "symfony/console",
					"description": "Symfony Console Component",
					"version": "7.0.0",
					"time": "` + recent + `",
					"dist": {"url": "https://repo.packagist.org/7.0.0.zip", "type": "zip"}
				},
				{
					"version": "6.0.0",
					"time": "` + old + `",
					"dist": {"url": "https://repo.packagist.org/6.0.0.zip", "type": "zip"}
				},
				{
					"version": "5.0.0",
					"time": "` + veryOld + `",
					"dist": {"url": "https://repo.packagist.org/5.0.0.zip", "type": "zip"}
				}
			]
		}
	}`

	output, err := h.rewriteMetadata([]byte(input))
	if err != nil {
		t.Fatalf("rewriteMetadata failed: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	packages := result["packages"].(map[string]any)
	versions := packages["symfony/console"].([]any)

	// v7.0.0 should be filtered by cooldown, leaving v6.0.0 and v5.0.0
	if len(versions) != 2 {
		t.Fatalf("expected 2 versions after cooldown, got %d", len(versions))
	}

	// Both remaining versions must have the "name" field
	for _, v := range versions {
		vmap := v.(map[string]any)
		if vmap["name"] != "symfony/console" {
			t.Errorf("version %v missing name field, got %v", vmap["version"], vmap["name"])
		}
	}
}

func TestComposerRewriteMetadataCooldown(t *testing.T) {
	now := time.Now()
	old := now.Add(-10 * 24 * time.Hour).Format(time.RFC3339)
	recent := now.Add(-1 * time.Hour).Format(time.RFC3339)

	proxy := &Proxy{Logger: slog.Default()}
	proxy.Cooldown = &cooldown.Config{Default: "3d"}

	h := &ComposerHandler{
		proxy:    proxy,
		proxyURL: "http://localhost:8080",
	}

	input := `{
		"packages": {
			"symfony/console": [
				{
					"version": "5.0.0",
					"time": "` + old + `",
					"dist": {"url": "https://repo.packagist.org/5.0.0.zip", "type": "zip"}
				},
				{
					"version": "6.0.0",
					"time": "` + recent + `",
					"dist": {"url": "https://repo.packagist.org/6.0.0.zip", "type": "zip"}
				}
			]
		}
	}`

	output, err := h.rewriteMetadata([]byte(input))
	if err != nil {
		t.Fatalf("rewriteMetadata failed: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	packages := result["packages"].(map[string]any)
	versions := packages["symfony/console"].([]any)

	if len(versions) != 1 {
		t.Fatalf("expected 1 version after cooldown, got %d", len(versions))
	}

	v := versions[0].(map[string]any)
	if v["version"] != "5.0.0" {
		t.Errorf("expected version 5.0.0, got %v", v["version"])
	}
}
