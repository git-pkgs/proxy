package handler

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/cooldown"
)

func TestPyPIParseFilename(t *testing.T) {
	h := &PyPIHandler{proxy: &Proxy{Logger: slog.Default()}}

	tests := []struct {
		filename    string
		wantName    string
		wantVersion string
	}{
		// Sdist formats
		{"requests-2.31.0.tar.gz", "requests", "2.31.0"},
		{"Django-4.2.7.tar.gz", "Django", "4.2.7"},
		{"aws-sdk-1.0.0.tar.gz", "aws-sdk", "1.0.0"},
		{"zipp-3.17.0.zip", "zipp", "3.17.0"},

		// Wheel formats
		{"requests-2.31.0-py3-none-any.whl", "requests", "2.31.0"},
		{"numpy-1.26.2-cp311-cp311-manylinux_2_17_x86_64.whl", "numpy", "1.26.2"},
		{"cryptography-41.0.5-cp37-abi3-manylinux_2_28_x86_64.whl", "cryptography", "41.0.5"},

		// Invalid
		{"invalid", "", ""},
	}

	for _, tt := range tests {
		name, version := h.parseFilename(tt.filename)
		if name != tt.wantName || version != tt.wantVersion {
			t.Errorf("parseFilename(%q) = (%q, %q), want (%q, %q)",
				tt.filename, name, version, tt.wantName, tt.wantVersion)
		}
	}
}

func TestPyPIRewriteJSONMetadataCooldown(t *testing.T) {
	now := time.Now()
	old := now.Add(-10 * 24 * time.Hour).Format(time.RFC3339)
	recent := now.Add(-1 * time.Hour).Format(time.RFC3339)

	proxy := &Proxy{Logger: slog.Default()}
	proxy.Cooldown = &cooldown.Config{Default: "3d"}

	h := &PyPIHandler{
		proxy:    proxy,
		proxyURL: "http://localhost:8080",
	}

	input := `{
		"info": {"name": "requests"},
		"releases": {
			"2.30.0": [{"url": "https://files.pythonhosted.org/packages/ab/cd/requests-2.30.0.tar.gz", "upload_time_iso_8601": "` + old + `"}],
			"2.31.0": [{"url": "https://files.pythonhosted.org/packages/ab/cd/requests-2.31.0.tar.gz", "upload_time_iso_8601": "` + recent + `"}]
		},
		"urls": [{"url": "https://files.pythonhosted.org/packages/ab/cd/requests-2.31.0.tar.gz", "upload_time_iso_8601": "` + recent + `"}]
	}`

	output, err := h.rewriteJSONMetadata([]byte(input))
	if err != nil {
		t.Fatalf("rewriteJSONMetadata failed: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	releases := result["releases"].(map[string]any)

	if _, ok := releases["2.30.0"]; !ok {
		t.Error("version 2.30.0 should not be filtered")
	}
	if _, ok := releases["2.31.0"]; ok {
		t.Error("version 2.31.0 should be filtered by cooldown")
	}

	// urls array should be empty since the current version is filtered
	urls := result["urls"].([]any)
	if len(urls) != 0 {
		t.Errorf("urls should be empty, got %d entries", len(urls))
	}
}

func TestIsPythonTag(t *testing.T) {
	tests := []struct {
		tag  string
		want bool
	}{
		{"py3", true},
		{"py2", true},
		{"cp311", true},
		{"cp37", true},
		{"pp39", true},
		{"none", false},
		{"any", false},
		{"manylinux", false},
	}

	for _, tt := range tests {
		got := isPythonTag(tt.tag)
		if got != tt.want {
			t.Errorf("isPythonTag(%q) = %v, want %v", tt.tag, got, tt.want)
		}
	}
}
