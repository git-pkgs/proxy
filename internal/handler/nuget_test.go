package handler

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/cooldown"
)

func nugetTestProxy() *Proxy {
	return &Proxy{
		Logger:     slog.Default(),
		HTTPClient: http.DefaultClient,
	}
}

func TestNuGetRewriteServiceIndex(t *testing.T) {
	h := &NuGetHandler{
		proxy:       nugetTestProxy(),
		upstreamURL: nugetUpstream,
		proxyURL:    "http://localhost:8080",
	}

	input := `{
		"version": "3.0.0",
		"resources": [
			{
				"@id": "https://api.nuget.org/v3-flatcontainer/",
				"@type": "PackageBaseAddress/3.0.0"
			},
			{
				"@id": "https://api.nuget.org/v3/registration5-gz-semver2/",
				"@type": "RegistrationsBaseUrl/3.6.0"
			},
			{
				"@id": "https://azuresearch-usnc.nuget.org/query",
				"@type": "SearchQueryService/3.5.0"
			},
			{
				"@id": "https://azuresearch-usnc.nuget.org/autocomplete",
				"@type": "SearchAutocompleteService/3.5.0"
			},
			{
				"@id": "https://example.com/other-service",
				"@type": "SomeOtherService/1.0.0"
			}
		]
	}`

	output, err := h.rewriteServiceIndex([]byte(input))
	if err != nil {
		t.Fatalf("rewriteServiceIndex failed: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	resources := result["resources"].([]any)
	if len(resources) != 5 {
		t.Fatalf("expected 5 resources, got %d", len(resources))
	}

	expectations := map[string]string{
		"PackageBaseAddress/3.0.0":        "http://localhost:8080/nuget/v3-flatcontainer/",
		"RegistrationsBaseUrl/3.6.0":      "http://localhost:8080/nuget/v3/registration5-gz-semver2/",
		"SearchQueryService/3.5.0":        "http://localhost:8080/nuget/query",
		"SearchAutocompleteService/3.5.0": "http://localhost:8080/nuget/autocomplete",
		"SomeOtherService/1.0.0":          "https://example.com/other-service",
	}

	for _, res := range resources {
		rmap := res.(map[string]any)
		rtype := rmap["@type"].(string)
		id := rmap["@id"].(string)
		expected, ok := expectations[rtype]
		if !ok {
			t.Errorf("unexpected resource type: %s", rtype)
			continue
		}
		if id != expected {
			t.Errorf("resource %s: @id = %q, want %q", rtype, id, expected)
		}
	}
}

func TestNuGetShouldRewriteService(t *testing.T) {
	h := &NuGetHandler{}

	rewriteTypes := []string{
		"PackageBaseAddress/3.0.0",
		"RegistrationsBaseUrl/3.6.0",
		"RegistrationsBaseUrl/Versioned",
		"SearchQueryService",
		"SearchQueryService/3.0.0-rc",
		"SearchQueryService/3.5.0",
		"SearchAutocompleteService",
		"SearchAutocompleteService/3.5.0",
	}

	for _, stype := range rewriteTypes {
		if !h.shouldRewriteService(stype) {
			t.Errorf("shouldRewriteService(%q) = false, want true", stype)
		}
	}

	noRewriteTypes := []string{
		"SomeOtherService/1.0.0",
		"PackagePublish/2.0.0",
		"",
		"SearchQueryService/99.0.0",
	}

	for _, stype := range noRewriteTypes {
		if h.shouldRewriteService(stype) {
			t.Errorf("shouldRewriteService(%q) = true, want false", stype)
		}
	}
}

func TestNuGetRewriteURL(t *testing.T) {
	h := &NuGetHandler{
		proxyURL: "http://localhost:8080",
	}

	tests := []struct {
		input string
		want  string
	}{
		{
			"https://api.nuget.org/v3-flatcontainer/",
			"http://localhost:8080/nuget/v3-flatcontainer/",
		},
		{
			"https://api.nuget.org/v3/registration5-gz-semver2/",
			"http://localhost:8080/nuget/v3/registration5-gz-semver2/",
		},
		{
			"https://azuresearch-usnc.nuget.org/query",
			"http://localhost:8080/nuget/query",
		},
		{
			"https://azuresearch-usnc.nuget.org/autocomplete",
			"http://localhost:8080/nuget/autocomplete",
		},
		{
			"https://example.com/unknown",
			"https://example.com/unknown",
		},
	}

	for _, tt := range tests {
		got := h.rewriteNuGetURL(tt.input)
		if got != tt.want {
			t.Errorf("rewriteNuGetURL(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestNuGetHandleServiceIndex(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/index.json" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"version": "3.0.0",
			"resources": [
				{
					"@id": "https://api.nuget.org/v3-flatcontainer/",
					"@type": "PackageBaseAddress/3.0.0"
				}
			]
		}`))
	}))
	defer upstream.Close()

	h := &NuGetHandler{
		proxy:       nugetTestProxy(),
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/v3/index.json", nil)
	w := httptest.NewRecorder()
	h.handleServiceIndex(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}

	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	resources := result["resources"].([]any)
	r0 := resources[0].(map[string]any)
	if r0["@id"] != "http://proxy.local/nuget/v3-flatcontainer/" {
		t.Errorf("resource @id = %q, want rewritten URL", r0["@id"])
	}
}

func TestNuGetHandleServiceIndexUpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal error"))
	}))
	defer upstream.Close()

	h := &NuGetHandler{
		proxy:       nugetTestProxy(),
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/v3/index.json", nil)
	w := httptest.NewRecorder()
	h.handleServiceIndex(w, req)

	// With metadata caching, upstream 500 is reported as 502 (bad gateway)
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadGateway)
	}
}

func TestNuGetHandleServiceIndexUpstreamUnreachable(t *testing.T) {
	h := &NuGetHandler{
		proxy:       nugetTestProxy(),
		upstreamURL: "http://127.0.0.1:1", // unreachable
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/v3/index.json", nil)
	w := httptest.NewRecorder()
	h.handleServiceIndex(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadGateway)
	}
}

func TestNuGetHandleServiceIndexInvalidJSON(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not valid json"))
	}))
	defer upstream.Close()

	h := &NuGetHandler{
		proxy:       nugetTestProxy(),
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/v3/index.json", nil)
	w := httptest.NewRecorder()
	h.handleServiceIndex(w, req)

	// When rewrite fails, the handler falls back to proxying the original body
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (should fall back to original)", w.Code, http.StatusOK)
	}

	body := w.Body.String()
	if body != "not valid json" {
		t.Errorf("body = %q, want original body passed through", body)
	}
}

func TestNuGetHandleDownloadEmptyParams(t *testing.T) {
	h := &NuGetHandler{
		proxy:       nugetTestProxy(),
		upstreamURL: "http://localhost:1",
		proxyURL:    "http://proxy.local",
	}

	// Missing path values
	req := httptest.NewRequest(http.MethodGet, "/v3-flatcontainer///", nil)
	req.SetPathValue("id", "")
	req.SetPathValue("version", "")
	req.SetPathValue("filename", "")

	w := httptest.NewRecorder()
	h.handleDownload(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestNuGetHandleDownloadNonNupkg(t *testing.T) {
	// Non-.nupkg files should be proxied upstream
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<nuspec>test</nuspec>"))
	}))
	defer upstream.Close()

	h := &NuGetHandler{
		proxy:       nugetTestProxy(),
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/v3-flatcontainer/newtonsoft.json/13.0.1/newtonsoft.json.nuspec", nil)
	req.SetPathValue("id", "newtonsoft.json")
	req.SetPathValue("version", "13.0.1")
	req.SetPathValue("filename", "newtonsoft.json.nuspec")

	w := httptest.NewRecorder()
	h.handleDownload(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	body := w.Body.String()
	if body != "<nuspec>test</nuspec>" {
		t.Errorf("body = %q, want nuspec content", body)
	}
}

func TestNuGetProxyUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3-flatcontainer/newtonsoft.json/index.json" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"versions":["13.0.1","13.0.2"]}`))
	}))
	defer upstream.Close()

	h := &NuGetHandler{
		proxy:       nugetTestProxy(),
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/v3-flatcontainer/newtonsoft.json/index.json", nil)
	w := httptest.NewRecorder()
	h.proxyUpstream(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	body := w.Body.String()
	if !strings.Contains(body, "13.0.1") {
		t.Errorf("response body does not contain expected version: %s", body)
	}
}

func TestNuGetProxyUpstreamNotFound(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	h := &NuGetHandler{
		proxy:       nugetTestProxy(),
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/v3-flatcontainer/nonexistent/index.json", nil)
	w := httptest.NewRecorder()
	h.proxyUpstream(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestNuGetProxyUpstreamBadUpstream(t *testing.T) {
	h := &NuGetHandler{
		proxy:       nugetTestProxy(),
		upstreamURL: "http://127.0.0.1:1",
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/v3-flatcontainer/test/index.json", nil)
	w := httptest.NewRecorder()
	h.proxyUpstream(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadGateway)
	}
}

func TestNuGetProxyUpstreamCopiesHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom", "value")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	h := &NuGetHandler{
		proxy:       nugetTestProxy(),
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/v3-flatcontainer/test/index.json", nil)
	w := httptest.NewRecorder()
	h.proxyUpstream(w, req)

	if w.Header().Get("X-Custom") != "value" {
		t.Errorf("X-Custom = %q, want %q", w.Header().Get("X-Custom"), "value")
	}
}

func TestNuGetProxyUpstreamForwardsAcceptEncoding(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ae := r.Header.Get("Accept-Encoding")
		if ae != "gzip" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("expected Accept-Encoding: gzip"))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	h := &NuGetHandler{
		proxy:       nugetTestProxy(),
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/v3-flatcontainer/test/index.json", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	h.proxyUpstream(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestNuGetBuildUpstreamURL(t *testing.T) {
	h := &NuGetHandler{
		upstreamURL: "https://api.nuget.org",
	}

	tests := []struct {
		path  string
		query string
		want  string
	}{
		{
			"/v3-flatcontainer/newtonsoft.json/index.json",
			"",
			"https://api.nuget.org/v3-flatcontainer/newtonsoft.json/index.json",
		},
		{
			"/v3/registration5-gz-semver2/newtonsoft.json/index.json",
			"",
			"https://api.nuget.org/v3/registration5-gz-semver2/newtonsoft.json/index.json",
		},
		{
			"/query",
			"q=json&take=20",
			"https://azuresearch-usnc.nuget.org/query?q=json&take=20",
		},
		{
			"/autocomplete",
			"q=new&take=10",
			"https://azuresearch-usnc.nuget.org/autocomplete?q=new&take=10",
		},
	}

	for _, tt := range tests {
		req := httptest.NewRequest(http.MethodGet, tt.path+"?"+tt.query, nil)
		got := h.buildUpstreamURL(req)
		if got != tt.want {
			t.Errorf("buildUpstreamURL(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestNuGetRoutes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v3/index.json" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":"3.0.0","resources":[]}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	h := &NuGetHandler{
		proxy:       nugetTestProxy(),
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	routes := h.Routes()

	req := httptest.NewRequest(http.MethodGet, "/v3/index.json", nil)
	w := httptest.NewRecorder()
	routes.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /v3/index.json: status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestNewNuGetHandler(t *testing.T) {
	proxy := nugetTestProxy()
	h := NewNuGetHandler(proxy, "http://localhost:8080/")

	if h.proxy != proxy {
		t.Error("proxy not set correctly")
	}
	if h.upstreamURL != nugetUpstream {
		t.Errorf("upstreamURL = %q, want %q", h.upstreamURL, nugetUpstream)
	}
	if h.proxyURL != "http://localhost:8080" {
		t.Errorf("proxyURL = %q, want %q (trailing slash should be trimmed)", h.proxyURL, "http://localhost:8080")
	}
}

func TestNewNuGetHandlerNoTrailingSlash(t *testing.T) {
	proxy := nugetTestProxy()
	h := NewNuGetHandler(proxy, "http://localhost:8080")

	if h.proxyURL != "http://localhost:8080" {
		t.Errorf("proxyURL = %q, want %q", h.proxyURL, "http://localhost:8080")
	}
}

func TestNuGetRewriteServiceIndexNoResources(t *testing.T) {
	h := &NuGetHandler{
		proxyURL: "http://localhost:8080",
	}

	input := `{"version":"3.0.0"}`
	output, err := h.rewriteServiceIndex([]byte(input))
	if err != nil {
		t.Fatalf("rewriteServiceIndex failed: %v", err)
	}

	// Should return the body unchanged when no resources key
	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}
	if result["version"] != "3.0.0" {
		t.Errorf("version = %v, want 3.0.0", result["version"])
	}
}

func TestNuGetRewriteServiceIndexAllTypes(t *testing.T) {
	h := &NuGetHandler{
		proxyURL: "http://localhost:8080",
	}

	// Test every rewritable service type
	resources := []map[string]string{
		{"@id": "https://api.nuget.org/v3-flatcontainer/", "@type": "PackageBaseAddress/3.0.0"},
		{"@id": "https://api.nuget.org/v3/registration5-gz-semver2/", "@type": "RegistrationsBaseUrl/3.6.0"},
		{"@id": "https://api.nuget.org/v3/registration5-gz-semver2/", "@type": "RegistrationsBaseUrl/Versioned"},
		{"@id": "https://azuresearch-usnc.nuget.org/query", "@type": "SearchQueryService"},
		{"@id": "https://azuresearch-usnc.nuget.org/query", "@type": "SearchQueryService/3.0.0-rc"},
		{"@id": "https://azuresearch-usnc.nuget.org/query", "@type": "SearchQueryService/3.5.0"},
		{"@id": "https://azuresearch-usnc.nuget.org/autocomplete", "@type": "SearchAutocompleteService"},
		{"@id": "https://azuresearch-usnc.nuget.org/autocomplete", "@type": "SearchAutocompleteService/3.5.0"},
	}

	inputResources := make([]any, len(resources))
	for i, r := range resources {
		inputResources[i] = r
	}

	input, _ := json.Marshal(map[string]any{
		"version":   "3.0.0",
		"resources": inputResources,
	})

	output, err := h.rewriteServiceIndex(input)
	if err != nil {
		t.Fatalf("rewriteServiceIndex failed: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	outputResources := result["resources"].([]any)
	for _, res := range outputResources {
		rmap := res.(map[string]any)
		id := rmap["@id"].(string)
		// All should be rewritten to proxy URL
		if strings.HasPrefix(id, "https://api.nuget.org") || strings.HasPrefix(id, "https://azuresearch-usnc.nuget.org") {
			t.Errorf("resource %s was not rewritten: %s", rmap["@type"], id)
		}
	}
}

func TestNuGetProxyUpstreamPreservesStatusCodes(t *testing.T) {
	codes := []int{
		http.StatusOK,
		http.StatusNotFound,
		http.StatusForbidden,
		http.StatusInternalServerError,
	}

	for _, code := range codes {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))

		h := &NuGetHandler{
			proxy:       nugetTestProxy(),
			upstreamURL: upstream.URL,
			proxyURL:    "http://proxy.local",
		}

		req := httptest.NewRequest(http.MethodGet, "/v3-flatcontainer/test/index.json", nil)
		w := httptest.NewRecorder()
		h.proxyUpstream(w, req)

		if w.Code != code {
			t.Errorf("status = %d, want %d", w.Code, code)
		}

		upstream.Close()
	}
}

func TestNuGetProxyUpstreamCopiesBody(t *testing.T) {
	expected := `{"versions":["1.0.0","2.0.0"]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(expected))
	}))
	defer upstream.Close()

	h := &NuGetHandler{
		proxy:       nugetTestProxy(),
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/v3-flatcontainer/test/index.json", nil)
	w := httptest.NewRecorder()
	h.proxyUpstream(w, req)

	got, _ := io.ReadAll(w.Body)
	if string(got) != expected {
		t.Errorf("body = %q, want %q", string(got), expected)
	}
}

func TestNuGetHandleDownloadMissingID(t *testing.T) {
	h := &NuGetHandler{
		proxy:       nugetTestProxy(),
		upstreamURL: "http://localhost:1",
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/v3-flatcontainer//1.0.0/test.nupkg", nil)
	req.SetPathValue("id", "")
	req.SetPathValue("version", "1.0.0")
	req.SetPathValue("filename", "test.nupkg")

	w := httptest.NewRecorder()
	h.handleDownload(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestNuGetHandleDownloadMissingVersion(t *testing.T) {
	h := &NuGetHandler{
		proxy:       nugetTestProxy(),
		upstreamURL: "http://localhost:1",
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/v3-flatcontainer/test//test.nupkg", nil)
	req.SetPathValue("id", "test")
	req.SetPathValue("version", "")
	req.SetPathValue("filename", "test.nupkg")

	w := httptest.NewRecorder()
	h.handleDownload(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestNuGetHandleDownloadMissingFilename(t *testing.T) {
	h := &NuGetHandler{
		proxy:       nugetTestProxy(),
		upstreamURL: "http://localhost:1",
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/v3-flatcontainer/test/1.0.0/", nil)
	req.SetPathValue("id", "test")
	req.SetPathValue("version", "1.0.0")
	req.SetPathValue("filename", "")

	w := httptest.NewRecorder()
	h.handleDownload(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestNuGetBuildUpstreamURLQueryPath(t *testing.T) {
	h := &NuGetHandler{
		upstreamURL: "https://api.nuget.org",
	}

	// Query endpoint should go to azuresearch
	req := httptest.NewRequest(http.MethodGet, "/query?q=json&skip=0&take=20", nil)
	got := h.buildUpstreamURL(req)
	want := "https://azuresearch-usnc.nuget.org/query?q=json&skip=0&take=20"
	if got != want {
		t.Errorf("buildUpstreamURL for /query = %q, want %q", got, want)
	}
}

func TestNuGetBuildUpstreamURLAutocompletePath(t *testing.T) {
	h := &NuGetHandler{
		upstreamURL: "https://api.nuget.org",
	}

	req := httptest.NewRequest(http.MethodGet, "/autocomplete?q=new&take=10", nil)
	got := h.buildUpstreamURL(req)
	want := "https://azuresearch-usnc.nuget.org/autocomplete?q=new&take=10"
	if got != want {
		t.Errorf("buildUpstreamURL for /autocomplete = %q, want %q", got, want)
	}
}

func TestNuGetBuildUpstreamURLRegularPath(t *testing.T) {
	h := &NuGetHandler{
		upstreamURL: "https://api.nuget.org",
	}

	req := httptest.NewRequest(http.MethodGet, "/v3/registration5-gz-semver2/newtonsoft.json/index.json", nil)
	got := h.buildUpstreamURL(req)
	want := "https://api.nuget.org/v3/registration5-gz-semver2/newtonsoft.json/index.json"
	if got != want {
		t.Errorf("buildUpstreamURL for registration = %q, want %q", got, want)
	}
}

func TestNuGetCooldownFiltering(t *testing.T) {
	now := time.Now()
	oldTime := now.Add(-7 * 24 * time.Hour).Format(time.RFC3339)
	recentTime := now.Add(-1 * time.Hour).Format(time.RFC3339)

	registration := map[string]any{
		"items": []any{
			map[string]any{
				"count": 2,
				"items": []any{
					map[string]any{
						"catalogEntry": map[string]any{
							"id":        "TestPackage",
							"version":   "1.0.0",
							"published": oldTime,
						},
					},
					map[string]any{
						"catalogEntry": map[string]any{
							"id":        "TestPackage",
							"version":   "2.0.0",
							"published": recentTime,
						},
					},
				},
			},
		},
	}

	body, err := json.Marshal(registration)
	if err != nil {
		t.Fatal(err)
	}

	proxy := testProxy()
	proxy.Cooldown = &cooldown.Config{
		Default: "3d",
	}

	h := &NuGetHandler{
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

	pages := result["items"].([]any)
	page := pages[0].(map[string]any)
	items := page["items"].([]any)

	if len(items) != 1 {
		t.Fatalf("expected 1 item after filtering, got %d", len(items))
	}

	entry := items[0].(map[string]any)["catalogEntry"].(map[string]any)
	if entry["version"] != testVersion100 {
		t.Errorf("expected version 1.0.0 to survive, got %s", entry["version"])
	}

	count := page["count"]
	if count != float64(1) {
		t.Errorf("expected page count to be 1, got %v", count)
	}
}

func TestNuGetCooldownFilteringWithPackageOverride(t *testing.T) {
	now := time.Now()
	recentTime := now.Add(-2 * time.Hour).Format(time.RFC3339)

	registration := map[string]any{
		"items": []any{
			map[string]any{
				"count": 1,
				"items": []any{
					map[string]any{
						"catalogEntry": map[string]any{
							"id":        "SpecialPackage",
							"version":   "1.0.0",
							"published": recentTime,
						},
					},
				},
			},
		},
	}

	body, err := json.Marshal(registration)
	if err != nil {
		t.Fatal(err)
	}

	proxy := testProxy()
	proxy.Cooldown = &cooldown.Config{
		Default:  "3d",
		Packages: map[string]string{"pkg:nuget/specialpackage": "1h"},
	}

	h := &NuGetHandler{
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

	pages := result["items"].([]any)
	page := pages[0].(map[string]any)
	items := page["items"].([]any)

	if len(items) != 1 {
		t.Fatalf("expected 1 item (package override allows it), got %d", len(items))
	}
}

func TestNuGetCooldownNoCooldownConfig(t *testing.T) {
	registration := map[string]any{
		"items": []any{
			map[string]any{
				"count": 1,
				"items": []any{
					map[string]any{
						"catalogEntry": map[string]any{
							"id":        "Test",
							"version":   "1.0.0",
							"published": time.Now().Format(time.RFC3339),
						},
					},
				},
			},
		},
	}

	body, err := json.Marshal(registration)
	if err != nil {
		t.Fatal(err)
	}

	// No cooldown - applyCooldownFiltering still works, just doesn't filter
	h := &NuGetHandler{
		proxy:    testProxy(),
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

	pages := result["items"].([]any)
	page := pages[0].(map[string]any)
	items := page["items"].([]any)

	// Without cooldown config on the handler, applyCooldownFiltering
	// is called but proxy.Cooldown is nil, so IsAllowed is never called
	// Actually, applyCooldownFiltering always runs the filter logic -
	// but the caller (handleRegistration) short-circuits when cooldown is disabled.
	// The function itself should still work fine with a nil Cooldown.
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
}

func TestNuGetCooldownFilteringNuGetTimestamp(t *testing.T) {
	// NuGet uses timestamps like "2024-09-07T01:37:52.233+00:00" which
	// have fractional seconds - verify these parse correctly
	now := time.Now()
	oldTime := now.Add(-7 * 24 * time.Hour).Format("2006-01-02T15:04:05.000-07:00")

	registration := map[string]any{
		"items": []any{
			map[string]any{
				"count": 1,
				"items": []any{
					map[string]any{
						"catalogEntry": map[string]any{
							"id":        "Test",
							"version":   "1.0.0",
							"published": oldTime,
						},
					},
				},
			},
		},
	}

	body, err := json.Marshal(registration)
	if err != nil {
		t.Fatal(err)
	}

	proxy := testProxy()
	proxy.Cooldown = &cooldown.Config{
		Default: "3d",
	}

	h := &NuGetHandler{
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

	pages := result["items"].([]any)
	page := pages[0].(map[string]any)
	items := page["items"].([]any)

	if len(items) != 1 {
		t.Fatalf("expected 1 item (old enough to pass cooldown), got %d", len(items))
	}
}

func TestNuGetHandleRegistrationWithCooldown(t *testing.T) {
	now := time.Now()
	oldTime := now.Add(-7 * 24 * time.Hour).Format(time.RFC3339)
	recentTime := now.Add(-1 * time.Hour).Format(time.RFC3339)

	registrationJSON, _ := json.Marshal(map[string]any{
		"items": []any{
			map[string]any{
				"count": 2,
				"items": []any{
					map[string]any{
						"catalogEntry": map[string]any{
							"id":        "TestPkg",
							"version":   "1.0.0",
							"published": oldTime,
						},
					},
					map[string]any{
						"catalogEntry": map[string]any{
							"id":        "TestPkg",
							"version":   "2.0.0",
							"published": recentTime,
						},
					},
				},
			},
		},
	})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(registrationJSON)
	}))
	defer upstream.Close()

	proxy := testProxy()
	proxy.Cooldown = &cooldown.Config{
		Default: "3d",
	}

	h := &NuGetHandler{
		proxy:       proxy,
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/v3/registration5-gz-semver2/testpkg/index.json", nil)
	w := httptest.NewRecorder()
	h.handleRegistration(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}

	pages := result["items"].([]any)
	page := pages[0].(map[string]any)
	items := page["items"].([]any)

	if len(items) != 1 {
		t.Fatalf("expected 1 item after cooldown filtering, got %d", len(items))
	}
}

func TestNuGetHandleRegistrationWithoutCooldown(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer upstream.Close()

	h := &NuGetHandler{
		proxy:       nugetTestProxy(), // no cooldown configured
		upstreamURL: upstream.URL,
		proxyURL:    "http://proxy.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/v3/registration5-gz-semver2/testpkg/index.json", nil)
	w := httptest.NewRecorder()
	h.handleRegistration(w, req)

	// Without cooldown, should proxy directly
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
}
