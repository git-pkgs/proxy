package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/cooldown"
)

func TestCondaParseFilename(t *testing.T) {
	h := &CondaHandler{proxy: &Proxy{Logger: slog.Default()}}

	tests := []struct {
		filename    string
		wantName    string
		wantVersion string
	}{
		{"numpy-1.24.0-py311h64a7726_0.conda", "numpy", "1.24.0"},
		{"scipy-1.11.4-py310h64a7726_0.tar.bz2", "scipy", "1.11.4"},
		{"python-dateutil-2.8.2-pyhd8ed1ab_0.conda", "python-dateutil", "2.8.2"},
		{"ca-certificates-2023.11.17-hbcca054_0.conda", "ca-certificates", "2023.11.17"},
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

func TestCondaIsPackageFile(t *testing.T) {
	h := &CondaHandler{}

	tests := []struct {
		filename string
		want     bool
	}{
		{"numpy-1.24.0-py311h64a7726_0.conda", true},
		{"scipy-1.11.4-py310h64a7726_0.tar.bz2", true},
		{"repodata.json", false},
		{"repodata.json.bz2", false},
	}

	for _, tt := range tests {
		got := h.isPackageFile(tt.filename)
		if got != tt.want {
			t.Errorf("isPackageFile(%q) = %v, want %v", tt.filename, got, tt.want)
		}
	}
}

func TestCondaCooldownFiltering(t *testing.T) {
	now := time.Now()
	oldTimestamp := float64(now.Add(-7 * 24 * time.Hour).UnixMilli())
	recentTimestamp := float64(now.Add(-1 * time.Hour).UnixMilli())

	repodata := map[string]any{
		"info": map[string]any{},
		"packages": map[string]any{
			"numpy-1.24.0-old.tar.bz2": map[string]any{
				"name":      "numpy",
				"version":   "1.24.0",
				"timestamp": oldTimestamp,
			},
			"numpy-1.25.0-new.tar.bz2": map[string]any{
				"name":      "numpy",
				"version":   "1.25.0",
				"timestamp": recentTimestamp,
			},
		},
		"packages.conda": map[string]any{
			"scipy-1.11.0-old.conda": map[string]any{
				"name":      "scipy",
				"version":   "1.11.0",
				"timestamp": oldTimestamp,
			},
			"scipy-1.12.0-new.conda": map[string]any{
				"name":      "scipy",
				"version":   "1.12.0",
				"timestamp": recentTimestamp,
			},
		},
	}

	body, err := json.Marshal(repodata)
	if err != nil {
		t.Fatal(err)
	}

	proxy := testProxy()
	proxy.Cooldown = &cooldown.Config{
		Default: "3d",
	}

	h := &CondaHandler{
		proxy:    proxy,
		proxyURL: "http://localhost:8080",
	}

	filtered, err := h.applyCooldownFiltering(body)
	if err != nil {
		t.Fatal(err)
	}

	var result map[string]any
	if err := json.Unmarshal(filtered, &result); err != nil {
		t.Fatal(err)
	}

	packages := result["packages"].(map[string]any)
	if len(packages) != 1 {
		t.Fatalf("expected 1 package in packages, got %d", len(packages))
	}
	if _, ok := packages["numpy-1.24.0-old.tar.bz2"]; !ok {
		t.Error("expected old numpy to survive filtering")
	}

	condaPkgs := result["packages.conda"].(map[string]any)
	if len(condaPkgs) != 1 {
		t.Fatalf("expected 1 package in packages.conda, got %d", len(condaPkgs))
	}
	if _, ok := condaPkgs["scipy-1.11.0-old.conda"]; !ok {
		t.Error("expected old scipy to survive filtering")
	}
}

func TestCondaCooldownFilteringWithPackageOverride(t *testing.T) {
	now := time.Now()
	recentTimestamp := float64(now.Add(-2 * time.Hour).UnixMilli())

	repodata := map[string]any{
		"info": map[string]any{},
		"packages": map[string]any{
			"special-1.0.0-build.tar.bz2": map[string]any{
				"name":      "special",
				"version":   "1.0.0",
				"timestamp": recentTimestamp,
			},
		},
		"packages.conda": map[string]any{},
	}

	body, err := json.Marshal(repodata)
	if err != nil {
		t.Fatal(err)
	}

	proxy := testProxy()
	proxy.Cooldown = &cooldown.Config{
		Default:  "3d",
		Packages: map[string]string{"pkg:conda/special": "1h"},
	}

	h := &CondaHandler{
		proxy:    proxy,
		proxyURL: "http://localhost:8080",
	}

	filtered, err := h.applyCooldownFiltering(body)
	if err != nil {
		t.Fatal(err)
	}

	var result map[string]any
	if err := json.Unmarshal(filtered, &result); err != nil {
		t.Fatal(err)
	}

	packages := result["packages"].(map[string]any)
	if len(packages) != 1 {
		t.Fatalf("expected 1 package (override allows it), got %d", len(packages))
	}
}

func TestCondaCooldownFilteringNoTimestamp(t *testing.T) {
	repodata := map[string]any{
		"info": map[string]any{},
		"packages": map[string]any{
			"old-pkg-1.0.0-build.tar.bz2": map[string]any{
				"name":    "old-pkg",
				"version": "1.0.0",
				// no timestamp field
			},
		},
		"packages.conda": map[string]any{},
	}

	body, err := json.Marshal(repodata)
	if err != nil {
		t.Fatal(err)
	}

	proxy := testProxy()
	proxy.Cooldown = &cooldown.Config{
		Default: "3d",
	}

	h := &CondaHandler{
		proxy:    proxy,
		proxyURL: "http://localhost:8080",
	}

	filtered, err := h.applyCooldownFiltering(body)
	if err != nil {
		t.Fatal(err)
	}

	var result map[string]any
	if err := json.Unmarshal(filtered, &result); err != nil {
		t.Fatal(err)
	}

	packages := result["packages"].(map[string]any)
	if len(packages) != 1 {
		t.Fatalf("entries without timestamp should pass through, got %d", len(packages))
	}
}

func TestCondaHandleRepodataWithCooldown(t *testing.T) {
	now := time.Now()
	oldTimestamp := float64(now.Add(-7 * 24 * time.Hour).UnixMilli())
	recentTimestamp := float64(now.Add(-1 * time.Hour).UnixMilli())

	repodataJSON, _ := json.Marshal(map[string]any{
		"info": map[string]any{},
		"packages": map[string]any{
			"old-1.0.0-build.tar.bz2": map[string]any{
				"name": "testpkg", "version": "1.0.0", "timestamp": oldTimestamp,
			},
			"new-2.0.0-build.tar.bz2": map[string]any{
				"name": "testpkg", "version": "2.0.0", "timestamp": recentTimestamp,
			},
		},
		"packages.conda": map[string]any{},
	})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(repodataJSON)
	}))
	defer upstream.Close()

	proxy := testProxy()
	proxy.Cooldown = &cooldown.Config{
		Default: "3d",
	}

	h := &CondaHandler{
		proxy:       proxy,
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/conda-forge/noarch/repodata.json", nil)
	req.SetPathValue("channel", "conda-forge")
	req.SetPathValue("arch", "noarch")
	w := httptest.NewRecorder()
	h.handleRepodata(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}

	packages := result["packages"].(map[string]any)
	if len(packages) != 1 {
		t.Fatalf("expected 1 package after filtering, got %d", len(packages))
	}
}

func TestCondaHandleRepodataWithoutCooldown(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"info":{},"packages":{},"packages.conda":{}}`))
	}))
	defer upstream.Close()

	h := &CondaHandler{
		proxy:       &Proxy{Logger: slog.Default(), HTTPClient: http.DefaultClient},
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/conda-forge/noarch/repodata.json", nil)
	req.SetPathValue("channel", "conda-forge")
	req.SetPathValue("arch", "noarch")
	w := httptest.NewRecorder()
	h.handleRepodata(w, req)

	// Without cooldown, should proxy directly (response comes from upstream)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
}
