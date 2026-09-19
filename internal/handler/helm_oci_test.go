package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/registries/fetch"
)

func TestHelmHandler_MixedOCIIndex(t *testing.T) {
	chart := "http chart content"
	digest := helmSHA256Hex([]byte(chart))
	const ociURL = "oci://ghcr.io/stakater/saap-catalog/charts/konfigurator-0.1.40.tgz"
	index := fmt.Sprintf(`apiVersion: v1
entries:
  konfigurator:
    - digest: %s
      urls: [%s]
      version: 0.1.40
  demo:
    - digest: %s
      urls: [oci://ghcr.io/owner/demo:1.0.0, demo-1.0.0.tgz]
      version: 1.0.0
`, digest, ociURL, digest)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.yaml":
			_, _ = fmt.Fprint(w, index)
		case "/demo-1.0.0.tgz":
			_, _ = fmt.Fprint(w, chart)
		default:
			t.Errorf("unexpected HTTP upstream request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	p, _, _, _ := setupTestProxy(t)
	p.CacheMetadata = true
	p.MetadataTTL = time.Hour
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	p.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	h := NewHelmHandler(p, "https://proxy.example", map[string]string{"mixed": upstream.URL})
	response := serveHelmRequest(h, "/mixed/index.yaml")
	if response.Code != http.StatusOK {
		t.Fatalf("index status = %d: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), ociURL) || !strings.Contains(response.Body.String(), h.chartProxyURL("mixed", digest, "demo-1.0.0.tgz")) {
		t.Fatalf("incorrectly rewritten index: %s", response.Body.String())
	}
	download, err := h.findChartDownload(upstream.URL, []byte(index), digest, "demo-1.0.0.tgz")
	if err != nil || download != upstream.URL+"/demo-1.0.0.tgz" {
		t.Fatalf("HTTP chart lookup = %q, %v", download, err)
	}
	// Even a .tgz-shaped OCI reference must never reach the HTTP fetcher.
	response = serveHelmRequest(h, "/mixed/charts/"+digest+"/konfigurator-0.1.40.tgz")
	if response.Code != http.StatusNotFound {
		t.Fatalf("OCI reference reached HTTP download route: status=%d", response.Code)
	}
	response = serveHelmRequest(h, "/mixed/charts/"+digest+"/demo-1.0.0.tgz")
	if response.Code != http.StatusOK || response.Body.String() != chart {
		t.Fatalf("cold HTTP chart status=%d body=%s", response.Code, response.Body.String())
	}
	upstream.Close()
	response = serveHelmRequest(h, "/mixed/charts/"+digest+"/demo-1.0.0.tgz")
	if response.Code != http.StatusOK || response.Body.String() != chart {
		t.Fatalf("cached HTTP chart status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestHelmOCIReferenceRewriting(t *testing.T) {
	for _, tt := range []struct {
		name, reference, defaultRegistry, proxyURL string
		registries                                 map[string]string
		want                                       string
	}{
		{name: "unmatched", reference: "oci://ghcr.io/owner/chart:1.0.0", want: "oci://ghcr.io/owner/chart:1.0.0"},
		{name: "named", reference: "oci://ghcr.io/owner/chart:1.0.0", registries: map[string]string{"ghcr": "https://ghcr.io"}, want: "oci://proxy.example/upstream/ghcr/owner/chart:1.0.0"},
		{name: "digest", reference: "oci://ghcr.io/owner/chart@sha256:" + strings.Repeat("a", 64), registries: map[string]string{"ghcr": "https://ghcr.io/"}, want: "oci://proxy.example/upstream/ghcr/owner/chart@sha256:" + strings.Repeat("a", 64)},
		{name: "reported tgz reference", reference: "oci://ghcr.io/stakater/saap-catalog/charts/konfigurator-0.1.40.tgz", registries: map[string]string{"ghcr": "https://ghcr.io"}, want: "oci://proxy.example/upstream/ghcr/stakater/saap-catalog/charts/konfigurator-0.1.40.tgz"},
		{name: "default", reference: "oci://registry.example/owner/chart:1.0.0", defaultRegistry: "https://registry.example", want: "oci://proxy.example/owner/chart:1.0.0"},
		{name: "named preferred and stable", reference: "oci://ghcr.io/owner/chart", defaultRegistry: "https://ghcr.io", registries: map[string]string{"z": "https://ghcr.io", "a": "https://ghcr.io"}, want: "oci://proxy.example/upstream/a/owner/chart"},
		{name: "port", reference: "oci://registry.example:5000/owner/chart", registries: map[string]string{"local": "http://registry.example:5000"}, proxyURL: "http://localhost:8080", want: "oci://localhost:8080/upstream/local/owner/chart"},
		{name: "different port", reference: "oci://registry.example:5001/owner/chart", registries: map[string]string{"local": "http://registry.example:5000"}, want: "oci://registry.example:5001/owner/chart"},
		{name: "different host", reference: "oci://ghcr.io.evil.example/owner/chart", registries: map[string]string{"ghcr": "https://ghcr.io"}, want: "oci://ghcr.io.evil.example/owner/chart"},
		{name: "escaped path", reference: "oci://ghcr.io/owner/chart:1.0.0%2Bbuild", registries: map[string]string{"ghcr": "https://ghcr.io"}, want: "oci://proxy.example/upstream/ghcr/owner/chart:1.0.0%2Bbuild"},
		{name: "upstream API mount", reference: "oci://ghcr.io/owner/chart", registries: map[string]string{"ghcr": "https://ghcr.io/mount"}, want: "oci://ghcr.io/owner/chart"},
		{name: "proxy path prefix", reference: "oci://ghcr.io/owner/chart", proxyURL: "https://proxy.example/mount", registries: map[string]string{"ghcr": "https://ghcr.io"}, want: "oci://ghcr.io/owner/chart"},
		{name: "reserved default prefix", reference: "oci://ghcr.io/upstream/other/chart", defaultRegistry: "https://ghcr.io", want: "oci://ghcr.io/upstream/other/chart"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.proxyURL == "" {
				tt.proxyURL = "https://proxy.example"
			}
			h := NewHelmHandlerWithOCIRegistries(&Proxy{}, tt.proxyURL, nil, tt.defaultRegistry, tt.registries)
			index := fmt.Sprintf("apiVersion: v1\nentries:\n  demo:\n    - digest: %s\n      urls: [%q]\n", strings.Repeat("a", 64), tt.reference)
			rewritten, err := h.rewriteIndex("mixed", "https://charts.example", []byte(index))
			if err != nil {
				t.Fatal(err)
			}
			_, entries, err := parseHelmIndex(rewritten)
			if err != nil {
				t.Fatal(err)
			}
			got := helmMappingValue(entries.Content[1].Content[0], "urls").Content[0].Value
			if got != tt.want {
				t.Fatalf("rewritten reference = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHelmIndexRejectsInvalidChartReferences(t *testing.T) {
	for _, reference := range []string{"oci:///chart", "oci://ghcr.io", "oci://user:secret@ghcr.io/chart", "oci://ghcr.io/../chart", "oci://ghcr.io/%2e%2e/chart", "oci://ghcr.io/chart?token=x", "file:///chart.tgz", "ftp://example.com/chart.tgz", "https://example.com/chart.zip"} {
		t.Run(reference, func(t *testing.T) {
			h := NewHelmHandler(&Proxy{}, "https://proxy.example", nil)
			index := fmt.Sprintf("apiVersion: v1\nentries:\n  demo:\n    - digest: %s\n      urls: [%q]\n", strings.Repeat("a", 64), reference)
			if _, err := h.rewriteIndex("mixed", "https://charts.example", []byte(index)); err == nil {
				t.Fatal("invalid reference was accepted")
			}
		})
	}
}

func TestHelmOCICooldown(t *testing.T) {
	p := &Proxy{Cooldown: &cooldown.Config{Default: "3d"}}
	h := NewHelmHandlerWithOCIRegistries(p, "https://proxy.example", nil, "https://ghcr.io", nil)
	index := fmt.Sprintf(`apiVersion: v1
entries:
  demo:
    - digest: %s
      created: %s
      urls: [oci://ghcr.io/owner/demo:1.0.0]
    - digest: %s
      created: %s
      urls: [oci://ghcr.io/owner/demo:2.0.0]
`, strings.Repeat("a", 64), time.Now().Add(-7*24*time.Hour).Format(time.RFC3339),
		strings.Repeat("b", 64), time.Now().Add(-time.Hour).Format(time.RFC3339))
	rewritten, err := h.rewriteIndex("mixed", "https://charts.example", []byte(index))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rewritten), "demo:2.0.0") || !strings.Contains(string(rewritten), "oci://proxy.example/owner/demo:1.0.0") {
		t.Fatalf("incorrect cooldown filtering: %s", rewritten)
	}
}

func TestHelmRewrittenOCIRouteServesChart(t *testing.T) {
	chart := "OCI Helm chart layer"
	digest := "sha256:" + helmSHA256Hex([]byte(chart))
	manifest := `{"schemaVersion":2,"layers":[{"mediaType":"application/vnd.cncf.helm.chart.content.v1.tar+gzip","digest":"` + digest + `"}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/owner/demo/manifests/1.0.0":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			_, _ = fmt.Fprint(w, manifest)
		case "/v2/owner/demo/blobs/" + digest:
			_, _ = fmt.Fprint(w, chart)
		default:
			t.Errorf("unexpected registry request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	p, _, _, _ := setupTestProxy(t)
	p.HTTPClient = upstream.Client()
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	p.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	registries := map[string]string{"local": upstream.URL}
	h := NewHelmHandlerWithOCIRegistries(p, "https://proxy.example", nil, "", registries)
	reference := "oci://" + strings.TrimPrefix(upstream.URL, "http://") + "/owner/demo:1.0.0"
	rewritten, err := url.Parse(h.ociChartProxyURL(reference))
	if err != nil {
		t.Fatal(err)
	}
	if rewritten.Host != "proxy.example" {
		t.Fatalf("OCI reference was not proxied: %s", rewritten)
	}
	repository := strings.TrimSuffix(rewritten.Path, ":1.0.0")
	container := NewContainerHandlerWithRegistry(p, "https://proxy.example", "", registries)
	for _, tt := range []struct{ path, want string }{
		{repository + "/manifests/1.0.0", manifest},
		{repository + "/blobs/" + digest, chart},
	} {
		w := httptest.NewRecorder()
		container.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, tt.path, nil))
		if w.Code != http.StatusOK || w.Body.String() != tt.want {
			t.Fatalf("OCI GET %s: status=%d body=%s", tt.path, w.Code, w.Body.String())
		}
	}
}
