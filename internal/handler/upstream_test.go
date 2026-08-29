package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandlerUpstreamConfiguration(t *testing.T) {
	const (
		proxyURL = "https://proxy.example.com/"
		baseURL  = "https://upstream.example.com"
	)
	hex := NewHexHandlerWithUpstreams(nil, proxyURL, baseURL+"/hex/", baseURL+"/hex-api/")
	pypi := NewPyPIHandlerWithUpstreams(nil, proxyURL, baseURL+"/pypi/", baseURL+"/pypi-download/")
	nuget := NewNuGetHandlerWithUpstreams(nil, proxyURL, baseURL+"/nuget/", baseURL+"/nuget-search/")
	composer := NewComposerHandlerWithUpstreams(
		nil, proxyURL, baseURL+"/composer/", baseURL+"/composer-repository/",
	)

	got := map[string]string{
		"gem":                 NewGemHandlerWithUpstream(nil, proxyURL, baseURL+"/gem/").upstreamURL,
		"go":                  NewGoHandlerWithUpstream(nil, proxyURL, baseURL+"/go/").upstreamURL,
		"hex":                 hex.upstreamURL,
		"hex_api":             hex.apiURL,
		"pub":                 NewPubHandlerWithUpstream(nil, proxyURL, baseURL+"/pub/").upstreamURL,
		"pypi":                pypi.upstreamURL,
		"pypi_download":       pypi.downloadURL,
		"nuget":               nuget.upstreamURL,
		"nuget_search":        nuget.searchURL,
		"composer":            composer.upstreamURL,
		"composer_repository": composer.repoURL,
		"conan":               NewConanHandlerWithUpstream(nil, proxyURL, baseURL+"/conan/").upstreamURL,
		"conda":               NewCondaHandlerWithUpstream(nil, proxyURL, baseURL+"/conda/").upstreamURL,
		"cran":                NewCRANHandlerWithUpstream(nil, proxyURL, baseURL+"/cran/").upstreamURL,
		"julia":               NewJuliaHandlerWithUpstream(nil, baseURL+"/julia/").upstreamURL,
		"oci_default":         NewContainerHandlerWithRegistry(nil, proxyURL, baseURL+"/oci/").registryURL,
		"rpm":                 NewRPMHandlerWithUpstream(nil, proxyURL, baseURL+"/rpm/").upstreamURL,
	}

	want := map[string]string{
		"gem":                 baseURL + "/gem",
		"go":                  baseURL + "/go",
		"hex":                 baseURL + "/hex",
		"hex_api":             baseURL + "/hex-api",
		"pub":                 baseURL + "/pub",
		"pypi":                baseURL + "/pypi",
		"pypi_download":       baseURL + "/pypi-download",
		"nuget":               baseURL + "/nuget",
		"nuget_search":        baseURL + "/nuget-search",
		"composer":            baseURL + "/composer",
		"composer_repository": baseURL + "/composer-repository",
		"conan":               baseURL + "/conan",
		"conda":               baseURL + "/conda",
		"cran":                baseURL + "/cran",
		"julia":               baseURL + "/julia",
		"oci_default":         baseURL + "/oci",
		"rpm":                 baseURL + "/rpm",
	}

	for name, wantURL := range want {
		if gotURL := got[name]; gotURL != wantURL {
			t.Errorf("%s upstream = %q, want %q", name, gotURL, wantURL)
		}
	}
}

func TestConfiguredUpstreamURL(t *testing.T) {
	if got := configuredUpstreamURL("", "https://default.example.com/"); got != "https://default.example.com" {
		t.Errorf("empty configured URL = %q, want default", got)
	}
	if got := configuredUpstreamURL("https://custom.example.com/", "https://default.example.com"); got != "https://custom.example.com" {
		t.Errorf("configured URL = %q, want trimmed custom URL", got)
	}
	if got := configuredUpstreamURL("https://custom.example.com///", "https://default.example.com"); got != "https://custom.example.com" {
		t.Errorf("configured URL with trailing slashes = %q, want trimmed custom URL", got)
	}
}

func TestHexHandlerUsesConfiguredAPIUpstream(t *testing.T) {
	var requestedPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		_, _ = io.WriteString(w, `{"releases":[]}`)
	}))
	defer upstream.Close()

	h := NewHexHandlerWithUpstreams(
		&Proxy{HTTPClient: upstream.Client()},
		"https://proxy.example.com",
		upstream.URL+"/hex",
		upstream.URL+"/hex-api",
	)
	_, err := h.fetchFilteredVersions(httptest.NewRequest(http.MethodGet, "/", nil), "demo")
	if err != nil {
		t.Fatalf("fetchFilteredVersions failed: %v", err)
	}
	if requestedPath != "/hex-api/api/packages/demo" {
		t.Errorf("API path = %q, want %q", requestedPath, "/hex-api/api/packages/demo")
	}
}

func TestPyPIHandlerRewritesConfiguredDownloadUpstream(t *testing.T) {
	h := NewPyPIHandlerWithUpstreams(
		nil,
		"https://proxy.example.com",
		"https://upstream.example.com/pypi",
		"https://upstream.example.com/pypi",
	)
	body := []byte(`<a href="https://upstream.example.com/pypi/packages/packages/ab/demo.whl#sha256=abc">demo</a>`)
	want := `<a href="https://proxy.example.com/pypi/packages/packages/packages/ab/demo.whl#sha256=abc">demo</a>`
	if got := string(h.rewriteSimpleHTML(body, nil)); got != want {
		t.Errorf("rewritten HTML = %q, want %q", got, want)
	}
}

func TestNuGetHandlerUsesConfiguredSearchUpstream(t *testing.T) {
	h := NewNuGetHandlerWithUpstreams(
		nil,
		"https://proxy.example.com",
		"https://upstream.example.com/nuget",
		"https://upstream.example.com/nuget-search",
	)
	req := httptest.NewRequest(http.MethodGet, "/query?q=demo", nil)
	want := "https://upstream.example.com/nuget-search/query?q=demo"
	if got := h.buildUpstreamURL(req); got != want {
		t.Errorf("search URL = %q, want %q", got, want)
	}
}
