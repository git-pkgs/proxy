package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func cargoTestProxy() *Proxy {
	return &Proxy{
		Logger: slog.Default(),
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
		{"A", "1/a"},                      // lowercase
		{"SERDE", "se/rd/serde"},          // lowercase
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
