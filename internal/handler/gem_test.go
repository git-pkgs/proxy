package handler

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/cooldown"
)

func TestGemParseFilename(t *testing.T) {
	h := &GemHandler{proxy: &Proxy{Logger: slog.Default()}}

	tests := []struct {
		filename    string
		wantName    string
		wantVersion string
	}{
		{"rails-7.1.0.gem", "rails", "7.1.0"},
		{"aws-sdk-s3-1.142.0.gem", "aws-sdk-s3", "1.142.0"},
		{"nokogiri-1.15.4-x86_64-linux.gem", "nokogiri", "1.15.4-x86_64-linux"},
		{"activerecord-7.0.8.gem", "activerecord", "7.0.8"},
		{"invalid", "", ""},
	}

	for _, tt := range tests {
		name, version := h.parseGemFilename(tt.filename)
		if name != tt.wantName || version != tt.wantVersion {
			t.Errorf("parseGemFilename(%q) = (%q, %q), want (%q, %q)",
				tt.filename, name, version, tt.wantName, tt.wantVersion)
		}
	}
}

func TestGemCompactIndexCooldown(t *testing.T) {
	now := time.Now()
	oldTime := now.Add(-7 * 24 * time.Hour).Format(time.RFC3339)
	recentTime := now.Add(-1 * time.Hour).Format(time.RFC3339)

	compactIndex := "---\n1.0.0 dep1:>= 1.0|checksum:abc123\n2.0.0 dep1:>= 1.0|checksum:def456\n"

	versionsJSON, _ := json.Marshal([]gemVersion{
		{Number: "1.0.0", Platform: "ruby", CreatedAt: oldTime},
		{Number: "2.0.0", Platform: "ruby", CreatedAt: recentTime},
	})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/info/"):
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(compactIndex))
		case strings.HasPrefix(r.URL.Path, "/api/v1/versions/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(versionsJSON)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	proxy := testProxy()
	proxy.Cooldown = &cooldown.Config{
		Default: "3d",
	}

	h := &GemHandler{
		proxy:       proxy,
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/info/testgem", nil)
	req.SetPathValue("name", "testgem")
	w := httptest.NewRecorder()
	h.handleCompactIndex(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	body := w.Body.String()
	if !strings.Contains(body, "1.0.0") {
		t.Error("expected version 1.0.0 to survive filtering")
	}
	if strings.Contains(body, "2.0.0") {
		t.Error("expected version 2.0.0 to be filtered out")
	}
	if !strings.HasPrefix(body, "---\n") {
		t.Error("expected compact index header to be preserved")
	}
}

func TestGemCompactIndexCooldownWithPlatformVersion(t *testing.T) {
	now := time.Now()
	recentTime := now.Add(-1 * time.Hour).Format(time.RFC3339)

	compactIndex := "---\n1.0.0 dep:>= 1.0|checksum:abc\n1.0.0-java dep:>= 1.0|checksum:def\n"

	versionsJSON, _ := json.Marshal([]gemVersion{
		{Number: "1.0.0", Platform: "ruby", CreatedAt: recentTime},
		{Number: "1.0.0", Platform: "java", CreatedAt: recentTime},
	})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/info/"):
			_, _ = w.Write([]byte(compactIndex))
		case strings.HasPrefix(r.URL.Path, "/api/v1/versions/"):
			_, _ = w.Write(versionsJSON)
		}
	}))
	defer upstream.Close()

	proxy := testProxy()
	proxy.Cooldown = &cooldown.Config{
		Default: "3d",
	}

	h := &GemHandler{
		proxy:       proxy,
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/info/testgem", nil)
	req.SetPathValue("name", "testgem")
	w := httptest.NewRecorder()
	h.handleCompactIndex(w, req)

	body := w.Body.String()
	// Both ruby and java platform versions should be filtered
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) != 1 { // only "---"
		t.Errorf("expected only header line, got %d lines: %v", len(lines), lines)
	}
}

func TestGemCompactIndexNoCooldown(t *testing.T) {
	compactIndex := "---\n1.0.0 dep:>= 1.0|checksum:abc\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(compactIndex))
	}))
	defer upstream.Close()

	h := &GemHandler{
		proxy:       testProxy(), // no cooldown
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/info/testgem", nil)
	req.SetPathValue("name", "testgem")
	w := httptest.NewRecorder()
	h.handleCompactIndex(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestGemCompactIndexVersionsAPIFails(t *testing.T) {
	compactIndex := "---\n1.0.0 dep:>= 1.0|checksum:abc\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/info/"):
			_, _ = w.Write([]byte(compactIndex))
		case strings.HasPrefix(r.URL.Path, "/api/v1/versions/"):
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer upstream.Close()

	proxy := testProxy()
	proxy.Cooldown = &cooldown.Config{
		Default: "3d",
	}

	h := &GemHandler{
		proxy:       proxy,
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/info/testgem", nil)
	req.SetPathValue("name", "testgem")
	w := httptest.NewRecorder()
	h.handleCompactIndex(w, req)

	// Should still return OK with unfiltered content
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	body := w.Body.String()
	if !strings.Contains(body, "1.0.0") {
		t.Error("expected unfiltered content when versions API fails")
	}
}

func TestGemFetchFilteredVersions(t *testing.T) {
	now := time.Now()
	oldTime := now.Add(-7 * 24 * time.Hour).Format(time.RFC3339)
	recentTime := now.Add(-1 * time.Hour).Format(time.RFC3339)

	versionsJSON, _ := json.Marshal([]gemVersion{
		{Number: "1.0.0", Platform: "ruby", CreatedAt: oldTime},
		{Number: "2.0.0", Platform: "ruby", CreatedAt: recentTime},
		{Number: "2.0.0", Platform: "java", CreatedAt: recentTime},
	})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(versionsJSON)
	}))
	defer upstream.Close()

	proxy := testProxy()
	proxy.Cooldown = &cooldown.Config{
		Default: "3d",
	}

	h := &GemHandler{
		proxy:       proxy,
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/info/testgem", nil)
	filtered, err := h.fetchFilteredVersions(req, "testgem")
	if err != nil {
		t.Fatal(err)
	}

	if filtered["1.0.0"] {
		t.Error("version 1.0.0 should not be filtered (old enough)")
	}
	if !filtered["2.0.0"] {
		t.Error("version 2.0.0 (ruby) should be filtered")
	}
	if !filtered["2.0.0-java"] {
		t.Error("version 2.0.0-java should be filtered")
	}

	_ = fmt.Sprintf // silence unused import
}
