package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/git-pkgs/registries/fetch"
)

func TestArtifactDownloadUpstreamNotFoundReturns404(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		handler func(p *Proxy) http.Handler
	}{
		{"debian", "/pool/main/n/nginx/nginx_1.18.0-6_amd64.deb",
			func(p *Proxy) http.Handler { return NewDebianHandler(p, "http://localhost").Routes() }},
		{"rpm", "/releases/39/Everything/x86_64/os/Packages/n/nginx-1.24.0-1.fc39.x86_64.rpm",
			func(p *Proxy) http.Handler { return NewRPMHandler(p, "http://localhost").Routes() }},
		{"nuget", "/v3-flatcontainer/newtonsoft.json/13.0.3/newtonsoft.json.13.0.3.nupkg",
			func(p *Proxy) http.Handler { return NewNuGetHandler(p, "http://localhost").Routes() }},
		{"pypi", "/packages/packages/ab/cd/ef0123456789/requests-2.31.0-py3-none-any.whl",
			func(p *Proxy) http.Handler { return NewPyPIHandler(p, "http://localhost").Routes() }},
		{"cran", "/src/contrib/ggplot2_3.4.4.tar.gz",
			func(p *Proxy) http.Handler { return NewCRANHandler(p, "http://localhost").Routes() }},
		{"conda", "/conda-forge/linux-64/numpy-1.26.0-py311_0.tar.bz2",
			func(p *Proxy) http.Handler { return NewCondaHandler(p, "http://localhost").Routes() }},
		{"conan", "/v1/files/zlib/1.3.1/_/_/0/recipe/conan_sources.tgz",
			func(p *Proxy) http.Handler { return NewConanHandler(p, "http://localhost").Routes() }},
		{"gem", "/gems/rails-7.1.0.gem",
			func(p *Proxy) http.Handler { return NewGemHandler(p, "http://localhost").Routes() }},
		{"hex", "/tarballs/phoenix-1.7.10.tar",
			func(p *Proxy) http.Handler { return NewHexHandler(p, "http://localhost").Routes() }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxy, _, _, fetcher := setupTestProxy(t)
			fetcher.fetchErr = fetch.ErrNotFound

			srv := httptest.NewServer(tt.handler(proxy))
			defer srv.Close()

			resp, err := http.Get(srv.URL + tt.path)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("want 404 for missing upstream artifact, got %d", resp.StatusCode)
			}
		})
	}
}

func TestJuliaPackageUpstreamNotFoundReturns404(t *testing.T) {
	proxy, _, _, fetcher := setupTestProxy(t)
	fetcher.fetchErr = fetch.ErrNotFound

	dead := httptest.NewServer(http.NotFoundHandler())
	defer dead.Close()

	h := NewJuliaHandler(proxy, "http://localhost")
	h.upstreamURL = dead.URL

	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL +
		"/package/7876af07-990d-54b4-ab0e-23690620f79a/0123456789abcdef0123456789abcdef01234567")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("want 404 for missing upstream package, got %d", resp.StatusCode)
	}
}

func TestComposerDownloadUpstreamNotFoundReturns404(t *testing.T) {
	proxy, _, _, fetcher := setupTestProxy(t)
	fetcher.fetchErr = fetch.ErrNotFound

	meta := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/p2/monolog/monolog.json" {
			_, _ = w.Write([]byte(`{
				"packages": {
					"monolog/monolog": [
						{"version": "2.9.1", "dist": {"url": "https://example.com/monolog-2.9.1.zip", "type": "zip"}}
					]
				}
			}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer meta.Close()

	h := &ComposerHandler{proxy: proxy, repoURL: meta.URL, proxyURL: "http://localhost"}
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/files/monolog/monolog/2.9.1/monolog-2.9.1.zip")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("want 404 for missing upstream dist, got %d", resp.StatusCode)
	}
}

func TestContainerBlobUpstreamNotFoundReturns404(t *testing.T) {
	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token": "test-token-123"}`))
	}))
	defer authServer.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.Fetcher = &mockFetcherWithHeaders{
		fetchFn: func(_ context.Context, _ string, _ http.Header) (*fetch.Artifact, error) {
			return nil, fetch.ErrNotFound
		},
	}

	h := &ContainerHandler{
		proxy:       proxy,
		registryURL: "https://registry-1.docker.io",
		authURL:     authServer.URL,
		proxyURL:    "http://localhost:8080",
	}

	req := httptest.NewRequest(http.MethodGet,
		"/library/nginx/blobs/sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abcd", nil)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("want 404 for missing upstream blob, got %d; body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "BLOB_UNKNOWN") {
		t.Errorf("want BLOB_UNKNOWN error code in body, got: %s", w.Body.String())
	}
}
