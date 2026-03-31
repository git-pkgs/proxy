package handler

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/cooldown"
)

func cargoTestProxy() *Proxy {
	return &Proxy{
		Logger:     slog.Default(),
		HTTPClient: http.DefaultClient,
	}
}

func TestCargoBuildIndexPath(t *testing.T) {
	h := &CargoHandler{}

	tests := []struct {
		name string
		want string
	}{
		{"a", "1/a"},
		{"ab", "2/ab"},
		{"abc", "3/a/abc"},
		{"abcd", "ab/cd/abcd"},
		{"serde", "se/rd/serde"},
		{"tokio", "to/ki/tokio"},
		{"A", "1/a"},             // lowercase
		{"SERDE", "se/rd/serde"}, // lowercase
		{"rand_core", "ra/nd/rand_core"},
	}

	for _, tt := range tests {
		got := h.buildIndexPath(tt.name)
		if got != tt.want {
			t.Errorf("buildIndexPath(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestCargoConfigEndpoint(t *testing.T) {
	h := &CargoHandler{
		proxyURL: "http://localhost:8080",
	}

	req := httptest.NewRequest(http.MethodGet, "/config.json", nil)
	w := httptest.NewRecorder()

	h.handleConfig(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var config CargoConfig
	if err := json.Unmarshal(w.Body.Bytes(), &config); err != nil {
		t.Fatalf("failed to parse config: %v", err)
	}

	expectedDL := "http://localhost:8080/cargo/crates/{crate}/{version}/download"
	if config.DL != expectedDL {
		t.Errorf("DL = %q, want %q", config.DL, expectedDL)
	}
}

func TestCargoIndexProxy(t *testing.T) {
	// Create a mock upstream index server
	indexContent := `{"name":"serde","vers":"1.0.0","deps":[],"cksum":"abc123"}
{"name":"serde","vers":"1.0.1","deps":[],"cksum":"def456"}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/se/rd/serde" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(indexContent))
	}))
	defer upstream.Close()

	h := &CargoHandler{
		proxy:    cargoTestProxy(),
		indexURL: upstream.URL,
		proxyURL: "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/se/rd/serde", nil)
	req.SetPathValue("a", "se")
	req.SetPathValue("b", "rd")
	req.SetPathValue("name", "serde")

	w := httptest.NewRecorder()
	h.handleIndex(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	if w.Body.String() != indexContent {
		t.Errorf("body = %q, want %q", w.Body.String(), indexContent)
	}
}

func TestCargoIndexNotFound(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	h := &CargoHandler{
		proxy:    cargoTestProxy(),
		indexURL: upstream.URL,
		proxyURL: "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/no/ne/nonexistent", nil)
	req.SetPathValue("a", "no")
	req.SetPathValue("b", "ne")
	req.SetPathValue("name", "nonexistent")

	w := httptest.NewRecorder()
	h.handleIndex(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestCargoRoutes(t *testing.T) {
	h := &CargoHandler{
		proxy:    cargoTestProxy(),
		proxyURL: "http://proxy.local",
	}

	routes := h.Routes()

	// Test that config.json route exists
	req := httptest.NewRequest(http.MethodGet, "/config.json", nil)
	w := httptest.NewRecorder()
	routes.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("config.json status = %d, want %d", w.Code, http.StatusOK)
	}
}

type filterTestCase struct {
	line     string
	expected bool
}

func TestCargoCooldown(t *testing.T) {
	now := time.Now()

	createCase := func(name string, version string, age time.Duration, expected bool) filterTestCase {
		return filterTestCase{line: `{"name":"` + name + `","vers":"` + version + `","cksum":"abcd","features":{},"yanked":false,"pubtime":"` + now.Add(-1*age).Format(time.RFC3339) + `"}`, expected: expected}
	}

	testCases := []filterTestCase{
		// one week ago
		createCase("serde", "1.0.0", 168*time.Hour, true),
		// one hour ago
		createCase("serde", "1.0.1", 1*time.Hour, false),
		// two hours ago with custom filter (1h)
		createCase("tokio", "1.0.0", 2*time.Hour, true),
		// one hour ago with custom filter (1h)
		createCase("tokio", "1.0.0", 1*time.Minute, false),
	}

	var testInput strings.Builder
	var expectedOutput strings.Builder

	for _, testCase := range testCases {
		testInput.WriteString(testCase.line + "\n")
		if testCase.expected {
			expectedOutput.WriteString(testCase.line + "\n")
		}
	}

	proxy := testProxy()
	proxy.Cooldown = &cooldown.Config{
		Default:  "3d",
		Packages: map[string]string{"pkg:cargo/tokio": "1h"},
	}

	h := &CargoHandler{
		proxy:    proxy,
		proxyURL: "http://localhost:8080",
	}

	var outputBuffer bytes.Buffer
	h.applyCooldownFiltering(&outputBuffer, strings.NewReader(testInput.String()))
	output := outputBuffer.String()

	if output != expectedOutput.String() {
		t.Errorf("output = %q, want %q", output, expectedOutput.String())
	}

}
