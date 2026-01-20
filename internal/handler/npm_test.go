package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func testProxy() *Proxy {
	return &Proxy{
		Logger: slog.Default(),
	}
}

func TestNPMExtractVersionFromFilename(t *testing.T) {
	h := &NPMHandler{}

	tests := []struct {
		packageName string
		filename    string
		want        string
	}{
		{"lodash", "lodash-4.17.21.tgz", "4.17.21"},
		{"@babel/core", "core-7.23.0.tgz", "7.23.0"},
		{"@types/node", "node-20.10.0.tgz", "20.10.0"},
		{"express", "express-4.18.2.tgz", "4.18.2"},
		{"lodash", "lodash.tgz", ""},           // no version
		{"lodash", "lodash-4.17.21.zip", ""},   // wrong extension
		{"lodash", "other-4.17.21.tgz", ""},    // wrong package name
	}

	for _, tt := range tests {
		got := h.extractVersionFromFilename(tt.packageName, tt.filename)
		if got != tt.want {
			t.Errorf("extractVersionFromFilename(%q, %q) = %q, want %q",
				tt.packageName, tt.filename, got, tt.want)
		}
	}
}

func TestNPMRewriteMetadata(t *testing.T) {
	h := &NPMHandler{
		proxy:    testProxy(),
		proxyURL: "http://localhost:8080",
	}

	input := `{
		"name": "lodash",
		"versions": {
			"4.17.21": {
				"name": "lodash",
				"version": "4.17.21",
				"dist": {
					"tarball": "https://registry.npmjs.org/lodash/-/lodash-4.17.21.tgz",
					"shasum": "abc123"
				}
			}
		}
	}`

	output, err := h.rewriteMetadata("lodash", []byte(input))
	if err != nil {
		t.Fatalf("rewriteMetadata failed: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	versions := result["versions"].(map[string]any)
	v := versions["4.17.21"].(map[string]any)
	dist := v["dist"].(map[string]any)
	tarball := dist["tarball"].(string)

	expected := "http://localhost:8080/npm/lodash/-/lodash-4.17.21.tgz"
	if tarball != expected {
		t.Errorf("tarball = %q, want %q", tarball, expected)
	}
}

func TestNPMRewriteMetadataScopedPackage(t *testing.T) {
	h := &NPMHandler{
		proxy:    testProxy(),
		proxyURL: "http://localhost:8080",
	}

	input := `{
		"name": "@babel/core",
		"versions": {
			"7.23.0": {
				"name": "@babel/core",
				"version": "7.23.0",
				"dist": {
					"tarball": "https://registry.npmjs.org/@babel/core/-/core-7.23.0.tgz"
				}
			}
		}
	}`

	output, err := h.rewriteMetadata("@babel/core", []byte(input))
	if err != nil {
		t.Fatalf("rewriteMetadata failed: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	versions := result["versions"].(map[string]any)
	v := versions["7.23.0"].(map[string]any)
	dist := v["dist"].(map[string]any)
	tarball := dist["tarball"].(string)

	expected := "http://localhost:8080/npm/@babel%2Fcore/-/core-7.23.0.tgz"
	if tarball != expected {
		t.Errorf("tarball = %q, want %q", tarball, expected)
	}
}

func TestNPMHandlerMetadataProxy(t *testing.T) {
	// Create a mock upstream server
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/testpkg" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"name": "testpkg",
			"versions": {
				"1.0.0": {
					"name": "testpkg",
					"version": "1.0.0",
					"dist": {
						"tarball": "https://registry.npmjs.org/testpkg/-/testpkg-1.0.0.tgz"
					}
				}
			}
		}`))
	}))
	defer upstream.Close()

	h := &NPMHandler{
		proxy:       testProxy(),
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	// Test metadata request
	req := httptest.NewRequest(http.MethodGet, "/testpkg", nil)
	req.SetPathValue("name", "testpkg")

	w := httptest.NewRecorder()
	h.handlePackageMetadata(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	// Check that tarball URL was rewritten
	versions := result["versions"].(map[string]any)
	v := versions["1.0.0"].(map[string]any)
	dist := v["dist"].(map[string]any)
	tarball := dist["tarball"].(string)

	if tarball != "http://proxy.local/npm/testpkg/-/testpkg-1.0.0.tgz" {
		t.Errorf("tarball URL not rewritten correctly: %s", tarball)
	}
}

func TestNPMHandlerMetadataNotFound(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	h := &NPMHandler{
		proxy:       testProxy(),
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/nonexistent", nil)
	req.SetPathValue("name", "nonexistent")

	w := httptest.NewRecorder()
	h.handlePackageMetadata(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}
