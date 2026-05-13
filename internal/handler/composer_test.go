package handler

import (
	"encoding/json"
	"log/slog"
	"strings"
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

func TestComposerRewriteMetadataUnset(t *testing.T) {
	h := &ComposerHandler{
		proxy:    &Proxy{Logger: slog.Default()},
		proxyURL: "http://localhost:8080",
	}

	// In the minified format, "__unset" removes a field from the inherited
	// state. v1.29.0 has require-dev, v1.28.0 unsets it, v1.27.0 inherits the
	// unset state. Composer rejects metadata where require-dev (or any link
	// field) is the literal string "__unset" rather than an object.
	input := `{
		"minified": "composer/2.0",
		"packages": {
			"venturecraft/revisionable": [
				{
					"name": "venturecraft/revisionable",
					"version": "1.29.0",
					"require": {"php": ">=5.4"},
					"require-dev": {"orchestra/testbench": "~3.0"},
					"dist": {"url": "https://example.com/a.zip", "type": "zip"}
				},
				{
					"version": "1.28.0",
					"require-dev": "__unset"
				},
				{
					"version": "1.27.0"
				},
				{
					"version": "1.26.0",
					"require-dev": {"foo/bar": "1.0"}
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

	versions := result["packages"].(map[string]any)["venturecraft/revisionable"].([]any)
	if len(versions) != 4 {
		t.Fatalf("expected 4 versions, got %d", len(versions))
	}

	byVersion := map[string]map[string]any{}
	for _, v := range versions {
		vmap := v.(map[string]any)
		byVersion[vmap["version"].(string)] = vmap
	}

	if _, ok := byVersion["1.29.0"]["require-dev"].(map[string]any); !ok {
		t.Errorf("1.29.0 require-dev should be an object, got %T", byVersion["1.29.0"]["require-dev"])
	}
	if rd, ok := byVersion["1.28.0"]["require-dev"]; ok {
		t.Errorf("1.28.0 require-dev should be absent, got %v", rd)
	}
	if rd, ok := byVersion["1.27.0"]["require-dev"]; ok {
		t.Errorf("1.27.0 require-dev should be absent (inherited unset), got %v", rd)
	}
	if _, ok := byVersion["1.26.0"]["require-dev"].(map[string]any); !ok {
		t.Errorf("1.26.0 require-dev should be an object, got %T", byVersion["1.26.0"]["require-dev"])
	}
	if _, ok := byVersion["1.27.0"]["require"].(map[string]any); !ok {
		t.Error("1.27.0 should still inherit require from 1.29.0")
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

func TestComposerRewriteDistURLGitHubZipball(t *testing.T) {
	// GitHub zipball URLs end with a bare commit hash, no file extension.
	// The proxy must produce a filename with .zip extension so that the
	// archives library can detect the format when browsing source.
	h := &ComposerHandler{
		proxy:    testProxy(),
		proxyURL: "http://localhost:8080",
	}

	vmap := map[string]any{
		"version": "v7.4.8",
		"dist": map[string]any{
			"url":       "https://api.github.com/repos/symfony/asset/zipball/d2e2f014ccd6ec9fae8dbe6336a4164346a2a856",
			"type":      "zip",
			"shasum":    "",
			"reference": "d2e2f014ccd6ec9fae8dbe6336a4164346a2a856",
		},
	}

	h.rewriteDistURL(vmap, "symfony/asset", "v7.4.8")

	dist := vmap["dist"].(map[string]any)
	url := dist["url"].(string)

	// The rewritten URL's filename must have a .zip extension
	if !strings.HasSuffix(url, ".zip") {
		t.Errorf("rewritten dist URL filename has no .zip extension: %s", url)
	}
}

func TestComposerRewriteMetadataGitHubZipballFilenames(t *testing.T) {
	// End-to-end: metadata with GitHub zipball URLs should produce
	// download URLs that end in .zip so browse source can open them.
	h := &ComposerHandler{
		proxy:    testProxy(),
		proxyURL: "http://localhost:8080",
	}

	input := `{
		"packages": {
			"symfony/config": [
				{
					"version": "v7.4.8",
					"dist": {
						"url": "https://api.github.com/repos/symfony/config/zipball/c7369cc1da250fcbfe0c5a9d109e419661549c39",
						"type": "zip",
						"reference": "c7369cc1da250fcbfe0c5a9d109e419661549c39"
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
	versions := packages["symfony/config"].([]any)
	v := versions[0].(map[string]any)
	dist := v["dist"].(map[string]any)
	url := dist["url"].(string)

	if !strings.HasSuffix(url, ".zip") {
		t.Errorf("rewritten URL should end in .zip, got %s", url)
	}
}

func TestComposerExpandMinifiedSharedDistReferences(t *testing.T) {
	// When a minified version inherits the dist field from a previous version
	// (i.e. it doesn't include its own dist), expanding + rewriting must not
	// corrupt the dist URLs via shared map references.
	h := &ComposerHandler{
		proxy:    testProxy(),
		proxyURL: "http://localhost:8080",
	}

	// In this minified payload, v5.3.0 does NOT include a dist field,
	// so it inherits v5.4.0's dist. After expansion and URL rewriting,
	// each version must have its own correct dist URL.
	input := `{
		"minified": "composer/2.0",
		"packages": {
			"vendor/pkg": [
				{
					"name": "vendor/pkg",
					"version": "5.4.0",
					"dist": {
						"url": "https://api.github.com/repos/vendor/pkg/zipball/aaa111",
						"type": "zip",
						"reference": "aaa111"
					}
				},
				{
					"version": "5.3.0"
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
	versions := packages["vendor/pkg"].([]any)
	if len(versions) != 2 {
		t.Fatalf("expected 2 versions, got %d", len(versions))
	}

	v1 := versions[0].(map[string]any)
	v2 := versions[1].(map[string]any)

	dist1 := v1["dist"].(map[string]any)
	dist2 := v2["dist"].(map[string]any)

	url1 := dist1["url"].(string)
	url2 := dist2["url"].(string)

	// Each version must have its own URL with its own version in the path
	if !strings.Contains(url1, "/5.4.0/") {
		t.Errorf("v5.4.0 dist URL should contain /5.4.0/, got %s", url1)
	}
	if !strings.Contains(url2, "/5.3.0/") {
		t.Errorf("v5.3.0 dist URL should contain /5.3.0/, got %s", url2)
	}

	// The two URLs must be different
	if url1 == url2 {
		t.Errorf("both versions have the same dist URL (shared reference bug): %s", url1)
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
