package handler

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/cooldown"
)

func TestPubRewriteMetadata(t *testing.T) {
	h := &PubHandler{
		proxy:    testProxy(),
		proxyURL: "http://localhost:8080",
	}

	input := `{
		"name": "flutter_bloc",
		"versions": [
			{"version": "1.0.0", "archive_url": "https://pub.dev/packages/flutter_bloc/versions/1.0.0.tar.gz"},
			{"version": "2.0.0", "archive_url": "https://pub.dev/packages/flutter_bloc/versions/2.0.0.tar.gz"}
		]
	}`

	output, err := h.rewriteMetadata("flutter_bloc", []byte(input))
	if err != nil {
		t.Fatalf("rewriteMetadata failed: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	versions := result["versions"].([]any)
	if len(versions) != 2 {
		t.Fatalf("expected 2 versions, got %d", len(versions))
	}

	v1 := versions[0].(map[string]any)
	if v1["archive_url"] != "http://localhost:8080/pub/packages/flutter_bloc/versions/1.0.0.tar.gz" {
		t.Errorf("unexpected archive_url: %s", v1["archive_url"])
	}
}

func TestPubRewriteMetadataCooldown(t *testing.T) {
	now := time.Now()
	old := now.Add(-10 * 24 * time.Hour).Format(time.RFC3339)
	recent := now.Add(-1 * time.Hour).Format(time.RFC3339)

	proxy := &Proxy{Logger: slog.Default()}
	proxy.Cooldown = &cooldown.Config{Default: "3d"}

	h := &PubHandler{
		proxy:    proxy,
		proxyURL: "http://localhost:8080",
	}

	input := `{
		"name": "flutter_bloc",
		"latest": {"version": "2.0.0"},
		"versions": [
			{"version": "1.0.0", "published": "` + old + `", "archive_url": "https://pub.dev/1.0.0.tar.gz"},
			{"version": "2.0.0", "published": "` + recent + `", "archive_url": "https://pub.dev/2.0.0.tar.gz"}
		]
	}`

	output, err := h.rewriteMetadata("flutter_bloc", []byte(input))
	if err != nil {
		t.Fatalf("rewriteMetadata failed: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	versions := result["versions"].([]any)
	if len(versions) != 1 {
		t.Fatalf("expected 1 version after cooldown, got %d", len(versions))
	}

	v := versions[0].(map[string]any)
	if v["version"] != "1.0.0" {
		t.Errorf("expected version 1.0.0, got %v", v["version"])
	}

	// latest should be updated
	latest := result["latest"].(map[string]any)
	if latest["version"] != "1.0.0" {
		t.Errorf("latest version = %v, want 1.0.0", latest["version"])
	}
}
