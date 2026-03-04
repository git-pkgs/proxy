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
