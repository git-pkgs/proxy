package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/git-pkgs/registries/fetch"
)

const (
	nsTestBlob     = "layer bytes"
	nsTestProxyURL = "http://proxy.example.test"
)

// nsTestRegistry is a fake OCI registry serving one repository below an
// optional path prefix. It records every request URI it receives.
type nsTestRegistry struct {
	*httptest.Server
	repository string

	mu       sync.Mutex
	requests []string
}

func newNSTestRegistry(t *testing.T, repository, pathPrefix string) *nsTestRegistry {
	t.Helper()
	registry := &nsTestRegistry{repository: repository}
	manifest := nsTestManifest()
	blobDigest := "sha256:" + sha256Hex(nsTestBlob)
	manifestDigest := "sha256:" + sha256Hex(manifest)
	base := pathPrefix + "/v2/" + repository
	registry.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registry.mu.Lock()
		registry.requests = append(registry.requests, r.URL.RequestURI())
		registry.mu.Unlock()

		switch r.URL.Path {
		case base + "/manifests/latest", base + "/manifests/" + manifestDigest:
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = io.WriteString(w, manifest)
		case base + "/blobs/" + blobDigest:
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = io.WriteString(w, nsTestBlob)
		case base + "/tags/list":
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("last") == "" {
				w.Header().Set("Link", `<`+base+`/tags/list?last=1.0&n=1>; rel="next"`)
				_, _ = io.WriteString(w, `{"name":"`+repository+`","tags":["1.0"]}`)
				return
			}
			_, _ = io.WriteString(w, `{"name":"`+repository+`","tags":["2.0"]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(registry.Close)
	return registry
}

func nsTestManifest() string {
	return `{"schemaVersion":2,"layers":[{"digest":"sha256:` + sha256Hex(nsTestBlob) + `"}]}`
}

func (r *nsTestRegistry) host() string {
	return strings.TrimPrefix(r.URL, "http://")
}

func (r *nsTestRegistry) requestCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

func (r *nsTestRegistry) lastRequest() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.requests) == 0 {
		return ""
	}
	return r.requests[len(r.requests)-1]
}

// newNSTestHandler builds a container handler the way the server does, with a
// real fetcher so blob downloads reach the fake registries. Warnings logged
// while building the handler are written to the returned buffer.
func newNSTestHandler(t *testing.T, defaultURL string, named map[string]string) (http.Handler, *ContainerHandler, *bytes.Buffer) {
	t.Helper()
	proxy, _, _, _ := setupTestProxy(t)
	logs := &bytes.Buffer{}
	proxy.Logger = slog.New(slog.NewTextHandler(logs, nil))
	client := &http.Client{}
	proxy.HTTPClient = client
	proxy.MetadataTTL = time.Hour
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(client), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	h := NewContainerHandlerWithRegistry(proxy, nsTestProxyURL, defaultURL, named)
	return http.StripPrefix("/v2", h.Routes()), h, logs
}

func serveNS(routes http.Handler, target string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
	return response
}

func assertNameUnknown(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"NAME_UNKNOWN"`) {
		t.Errorf("body = %s, want NAME_UNKNOWN error", response.Body.String())
	}
}

func TestContainerHandler_NamespaceSelectsDefaultRegistry(t *testing.T) {
	registry := newNSTestRegistry(t, "library/nginx", "")
	// The default registry is a custom oci_default, so its host is only
	// resolvable when the index is built from the final registry URL.
	routes, _, _ := newNSTestHandler(t, registry.URL, nil)

	for _, namespace := range []string{"docker.io", "index.docker.io", "registry-1.docker.io", registry.host()} {
		t.Run(namespace, func(t *testing.T) {
			response := serveNS(routes, "/v2/library/nginx/manifests/latest?ns="+namespace)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
			}
			if got, want := registry.lastRequest(), "/v2/library/nginx/manifests/latest"; got != want {
				t.Errorf("upstream request = %q, want %q", got, want)
			}
		})
	}
}

func TestContainerHandler_NamespaceSelectsNamedRegistry(t *testing.T) {
	fallback := newNSTestRegistry(t, "owner/app", "")
	named := newNSTestRegistry(t, "owner/app", "")
	routes, _, _ := newNSTestHandler(t, fallback.URL, map[string]string{"ghcr": named.URL})
	digest := "sha256:" + sha256Hex(nsTestBlob)

	manifest := serveNS(routes, "/v2/owner/app/manifests/latest?ns="+named.host())
	if manifest.Code != http.StatusOK {
		t.Fatalf("manifest status = %d, want 200: %s", manifest.Code, manifest.Body.String())
	}
	blob := serveNS(routes, "/v2/owner/app/blobs/"+digest+"?ns="+named.host())
	if blob.Code != http.StatusOK {
		t.Fatalf("blob status = %d, want 200: %s", blob.Code, blob.Body.String())
	}
	if got := blob.Body.String(); got != nsTestBlob {
		t.Errorf("blob body = %q, want %q", got, nsTestBlob)
	}
	if got := named.requestCount(); got != 2 {
		t.Errorf("named registry requests = %d, want 2", got)
	}
	if got := fallback.requestCount(); got != 0 {
		t.Errorf("default registry requests = %d, want 0", got)
	}
}

func TestContainerHandler_NamespaceRejectsUnresolvableRequests(t *testing.T) {
	registry := newNSTestRegistry(t, "owner/app", "")
	other := newNSTestRegistry(t, "owner/app", "")
	routes, _, _ := newNSTestHandler(t, registry.URL, map[string]string{"ghcr": registry.URL, "quay": other.URL})
	digest := "sha256:" + sha256Hex(nsTestBlob)

	tests := map[string]string{
		"unknown host manifest":              "/v2/owner/app/manifests/latest?ns=quay.io",
		"unknown host blob":                  "/v2/owner/app/blobs/" + digest + "?ns=quay.io",
		"unknown host tags":                  "/v2/owner/app/tags/list?ns=quay.io",
		"multiple ns values":                 "/v2/owner/app/manifests/latest?ns=docker.io&ns=" + registry.host(),
		"prefix route with default host":     "/v2/upstream/quay/owner/app/manifests/latest?ns=" + registry.host(),
		"prefix route with other upstream":   "/v2/upstream/ghcr/owner/app/blobs/" + digest + "?ns=" + other.host(),
		"prefix route with unknown upstream": "/v2/upstream/nope/owner/app/manifests/latest?ns=" + registry.host(),
		"prefix route without repository":    "/v2/upstream/ghcr/manifests/latest?ns=" + registry.host(),
	}
	for name, target := range tests {
		t.Run(name, func(t *testing.T) {
			assertNameUnknown(t, serveNS(routes, target))
		})
	}
	if got := registry.requestCount() + other.requestCount(); got != 0 {
		t.Errorf("upstream requests = %d, want 0", got)
	}
}

func TestContainerHandler_NamespaceAcceptsPrefixRouteForOwnHost(t *testing.T) {
	// Per-registry containerd mirrors with override_path address the
	// upstream/{name}/ prefix and still send ns. They must keep working and
	// share the prefix route's cache entries.
	registry := newNSTestRegistry(t, "owner/app", "")
	routes, _, _ := newNSTestHandler(t, "", map[string]string{"ghcr": registry.URL})
	digest := "sha256:" + sha256Hex(nsTestBlob)

	for _, target := range []string{
		"/v2/upstream/ghcr/owner/app/manifests/latest?ns=" + registry.host(),
		"/v2/upstream/ghcr/owner/app/blobs/" + digest + "?ns=" + registry.host(),
	} {
		if response := serveNS(routes, target); response.Code != http.StatusOK {
			t.Fatalf("%s status = %d: %s", target, response.Code, response.Body.String())
		}
	}
	if got, want := registry.lastRequest(), "/v2/owner/app/blobs/"+digest; got != want {
		t.Errorf("upstream request = %q, want %q", got, want)
	}
	warmed := registry.requestCount()

	for _, target := range []string{
		"/v2/upstream/ghcr/owner/app/manifests/latest",
		"/v2/upstream/ghcr/owner/app/blobs/" + digest,
	} {
		if response := serveNS(routes, target); response.Code != http.StatusOK {
			t.Fatalf("%s status = %d: %s", target, response.Code, response.Body.String())
		}
	}
	if got := registry.requestCount(); got != warmed {
		t.Errorf("upstream requests after prefix pulls without ns = %d, want %d (cache hits)", got, warmed)
	}

	tags := serveNS(routes, "/v2/upstream/ghcr/owner/app/tags/list?n=1&ns="+registry.host())
	if tags.Code != http.StatusOK {
		t.Fatalf("tags status = %d: %s", tags.Code, tags.Body.String())
	}
	wantLink := `<` + nsTestProxyURL + `/v2/upstream/ghcr/owner/app/tags/list?last=1.0&n=1&ns=` + url.QueryEscape(registry.host()) + `>; rel="next"`
	if got := tags.Header().Get("Link"); got != wantLink {
		t.Errorf("Link = %q, want %q", got, wantLink)
	}
	if got, want := registry.lastRequest(), "/v2/owner/app/tags/list?n=1"; got != want {
		t.Errorf("upstream tags request = %q, want %q (ns must not be forwarded)", got, want)
	}
}

func TestContainerHandler_NamespacePrefixRouteAcceptsDockerHubAliases(t *testing.T) {
	// upstream.oci.hub points at Docker Hub and a docker.io hosts.toml mirror
	// with override_path addresses /v2/upstream/hub, so containerd sends
	// ns=docker.io. The dialer stands in for registry-1.docker.io.
	hub := newNSTestRegistry(t, "library/nginx", "")
	other := newNSTestRegistry(t, "library/nginx", "")
	routes, h, _ := newNSTestHandler(t, "", map[string]string{
		"hub":  "http://registry-1.docker.io",
		"quay": other.URL,
	})
	transport := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != "registry-1.docker.io:80" {
			return nil, fmt.Errorf("unexpected dial to %s", addr)
		}
		return (&net.Dialer{}).DialContext(ctx, network, hub.Listener.Addr().String())
	}}
	t.Cleanup(transport.CloseIdleConnections)
	h.proxy.HTTPClient = &http.Client{Transport: transport}

	for _, query := range []string{"?ns=docker.io", "?ns=index.docker.io", "?ns=registry-1.docker.io", "?ns=docker.io:443", ""} {
		response := serveNS(routes, "/v2/upstream/hub/library/nginx/manifests/latest"+query)
		if response.Code != http.StatusOK {
			t.Fatalf("%q status = %d: %s", query, response.Code, response.Body.String())
		}
	}
	if got := hub.requestCount(); got != 1 {
		t.Errorf("hub requests = %d, want 1 (the rest are cache hits)", got)
	}

	// A known host of another route still contradicts the prefix.
	assertNameUnknown(t, serveNS(routes, "/v2/upstream/hub/library/nginx/manifests/latest?ns="+other.host()))
	if got := other.requestCount(); got != 0 {
		t.Errorf("other registry requests = %d, want 0", got)
	}

	t.Run("Docker Hub mirror as named upstream", func(t *testing.T) {
		// The upstream is a Docker Hub mirror on some other host (mirror.gcr.io,
		// an Artifactory remote); nodes still pull docker.io/... through
		// /v2/upstream/hub, so ns says docker.io.
		mirror := newNSTestRegistry(t, "library/nginx", "")
		routes, _, _ := newNSTestHandler(t, "", map[string]string{"hub": mirror.URL})

		response := serveNS(routes, "/v2/upstream/hub/library/nginx/manifests/latest?ns=docker.io")
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", response.Code, response.Body.String())
		}
		if got, want := mirror.lastRequest(), "/v2/library/nginx/manifests/latest"; got != want {
			t.Errorf("upstream request = %q, want %q", got, want)
		}
	})
}

func TestContainerHandler_NamespacePrefixRouteRejectsUnknownHosts(t *testing.T) {
	// A _default wildcard mirror turns a pull of
	// unconfigured.example/upstream/mirror/owner/app into exactly this
	// request. Serving it would hand out owner/app from the mirror upstream
	// instead of letting containerd fall back to unconfigured.example.
	mirror := newNSTestRegistry(t, "owner/app", "")
	routes, h, logs := newNSTestHandler(t, "", map[string]string{"mirror": mirror.URL})

	for _, namespace := range []string{"unconfigured.example", "ghcr.io"} {
		assertNameUnknown(t, serveNS(routes, "/v2/upstream/mirror/owner/app/manifests/latest?ns="+namespace))
	}
	if got := mirror.requestCount(); got != 0 {
		t.Errorf("upstream requests = %d, want 0", got)
	}
	// containerd swallows the 404, so the refusal has to show up in the log.
	for _, want := range []string{"upstream.oci_mirrors", "upstream=mirror", "ns=ghcr.io"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs missing %q:\n%s", want, logs.String())
		}
	}

	// A node repeats the refused request for every pull; one line per
	// upstream/ns pair is enough. An ns longer than any registry host is
	// refused before it is logged or remembered.
	assertNameUnknown(t, serveNS(routes, "/v2/upstream/mirror/owner/app/manifests/latest?ns=GHCR.io"))
	long := strings.Repeat("a", 300) + ".example"
	assertNameUnknown(t, serveNS(routes, "/v2/upstream/mirror/owner/app/manifests/latest?ns="+long))
	if got := strings.Count(logs.String(), "upstream.oci_mirrors"); got != 2 {
		t.Errorf("refusal warnings = %d, want 2 (one per upstream/ns pair):\n%s", got, logs.String())
	}
	if strings.Contains(logs.String(), "aaaa") {
		t.Errorf("logs mention the overlong ns, want it refused silently:\n%s", logs.String())
	}
	if got := len(h.refusals); got != 2 {
		t.Errorf("remembered refusals = %d, want 2", got)
	}
}

func TestContainerHandler_NamespaceMirroredRegistries(t *testing.T) {
	// The upstream mirrors ghcr.io, and upstream.oci_mirrors says so. Nodes
	// keep pulling ghcr.io/..., either through a ghcr.io hosts.toml pointing
	// at /v2/upstream/ghcr or through the _default wildcard mirror.
	tests := map[string]string{
		"plain mirror host":  "",
		"artifactory remote": "/artifactory/api/docker/ghcr-remote",
	}
	for name, pathPrefix := range tests {
		t.Run(name, func(t *testing.T) {
			mirror := newNSTestRegistry(t, "owner/app", pathPrefix)
			routes, h, _ := newNSTestHandler(t, "", map[string]string{"ghcr": mirror.URL + pathPrefix})
			h.SetMirroredRegistries(map[string][]string{"ghcr": {"ghcr.io"}})

			for _, target := range []string{
				"/v2/upstream/ghcr/owner/app/manifests/latest?ns=ghcr.io",
				"/v2/upstream/ghcr/owner/app/manifests/latest?ns=GHCR.io:443",
				"/v2/owner/app/manifests/latest?ns=ghcr.io",
			} {
				before := mirror.requestCount()
				response := serveNS(routes, target)
				if response.Code != http.StatusOK {
					t.Fatalf("GET %s status = %d: %s", target, response.Code, response.Body.String())
				}
				// The manifest is cached after the first request, so only
				// the first one may reach the upstream.
				if before == 0 {
					if got, want := mirror.lastRequest(), pathPrefix+"/v2/owner/app/manifests/latest"; got != want {
						t.Errorf("upstream request = %q, want %q", got, want)
					}
				}
			}
			if got := mirror.requestCount(); got != 1 {
				t.Errorf("upstream requests = %d, want 1 (all routes share the cache)", got)
			}

			assertNameUnknown(t, serveNS(routes, "/v2/upstream/ghcr/owner/app/manifests/latest?ns=quay.io"))
		})
	}
}

func TestContainerHandler_NamespaceMirrorHostsRankBelowConfiguredURLs(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	logs := &bytes.Buffer{}
	proxy.Logger = slog.New(slog.NewTextHandler(logs, nil))
	h := NewContainerHandlerWithRegistry(proxy, nsTestProxyURL, "", map[string]string{
		"ghcr":  "https://ghcr.io",
		"art":   "https://artifactory.example/api/docker/ghcr-remote",
		"other": "https://other.example",
	})
	h.SetMirroredRegistries(map[string][]string{"art": {"ghcr.io"}, "missing": {"quay.io"}})

	if got := h.namespaces["ghcr.io"]; got != "ghcr" {
		t.Errorf("index[ghcr.io] = %q, want ghcr (configured URL wins over a mirror entry)", got)
	}
	if !h.namespaceNamesPrefixUpstream("ghcr.io", "upstream/art/owner/app") {
		t.Error("ns ghcr.io refused on upstream/art/, want accepted as its mirrored registry")
	}
	if h.namespaceNamesPrefixUpstream("ghcr.io", "upstream/other/owner/app") {
		t.Error("ns ghcr.io accepted on upstream/other/, want refused: only art lists it")
	}
	if _, ok := h.namespaces["quay.io"]; ok {
		t.Error("index has quay.io from a mirror entry without upstream")
	}
	for _, want := range []string{"ghcr.io=ghcr", "upstream=missing"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs missing %q:\n%s", want, logs.String())
		}
	}
}

func TestContainerHandler_NamespaceSharesCacheWithOtherRoutes(t *testing.T) {
	digest := "sha256:" + sha256Hex(nsTestBlob)
	manifestDigest := "sha256:" + sha256Hex(nsTestManifest())

	t.Run("named registry", func(t *testing.T) {
		registry := newNSTestRegistry(t, "owner/app", "")
		routes, _, _ := newNSTestHandler(t, "", map[string]string{"ghcr": registry.URL})

		for _, target := range []string{
			"/v2/upstream/ghcr/owner/app/blobs/" + digest,
			"/v2/upstream/ghcr/owner/app/manifests/" + manifestDigest,
		} {
			if response := serveNS(routes, target); response.Code != http.StatusOK {
				t.Fatalf("warm %s status = %d: %s", target, response.Code, response.Body.String())
			}
		}
		warmed := registry.requestCount()
		for _, target := range []string{
			"/v2/owner/app/blobs/" + digest + "?ns=" + registry.host(),
			"/v2/owner/app/manifests/" + manifestDigest + "?ns=" + registry.host(),
		} {
			if response := serveNS(routes, target); response.Code != http.StatusOK {
				t.Fatalf("ns %s status = %d: %s", target, response.Code, response.Body.String())
			}
		}
		if got := registry.requestCount(); got != warmed {
			t.Errorf("upstream requests after ns pulls = %d, want %d (cache hits)", got, warmed)
		}
	})

	t.Run("default registry", func(t *testing.T) {
		registry := newNSTestRegistry(t, "library/nginx", "")
		routes, _, _ := newNSTestHandler(t, registry.URL, nil)

		for _, target := range []string{
			"/v2/library/nginx/blobs/" + digest + "?ns=docker.io",
			"/v2/library/nginx/manifests/" + manifestDigest + "?ns=docker.io",
		} {
			if response := serveNS(routes, target); response.Code != http.StatusOK {
				t.Fatalf("warm %s status = %d: %s", target, response.Code, response.Body.String())
			}
		}
		warmed := registry.requestCount()
		for _, target := range []string{
			"/v2/library/nginx/blobs/" + digest,
			"/v2/library/nginx/manifests/" + manifestDigest,
		} {
			if response := serveNS(routes, target); response.Code != http.StatusOK {
				t.Fatalf("unprefixed %s status = %d: %s", target, response.Code, response.Body.String())
			}
		}
		if got := registry.requestCount(); got != warmed {
			t.Errorf("upstream requests after unprefixed pulls = %d, want %d (cache hits)", got, warmed)
		}
	})
}

func TestContainerHandler_NamespaceTagsList(t *testing.T) {
	registry := newNSTestRegistry(t, "owner/app", "")
	routes, _, _ := newNSTestHandler(t, "", map[string]string{"ghcr": registry.URL})

	// The prefix route fills the shared cache entry first.
	prefixed := serveNS(routes, "/v2/upstream/ghcr/owner/app/tags/list?n=1")
	if prefixed.Code != http.StatusOK {
		t.Fatalf("prefixed status = %d: %s", prefixed.Code, prefixed.Body.String())
	}
	const wantPrefixedLink = `<` + nsTestProxyURL + `/v2/upstream/ghcr/owner/app/tags/list?last=1.0&n=1>; rel="next"`
	if got := prefixed.Header().Get("Link"); got != wantPrefixedLink {
		t.Errorf("prefixed Link = %q, want %q", got, wantPrefixedLink)
	}

	namespaced := serveNS(routes, "/v2/owner/app/tags/list?n=1&ns="+registry.host())
	if namespaced.Code != http.StatusOK {
		t.Fatalf("ns status = %d: %s", namespaced.Code, namespaced.Body.String())
	}
	if got := registry.requestCount(); got != 1 {
		t.Errorf("upstream requests = %d, want 1 (ns request served from the shared cache)", got)
	}
	wantNSLink := `<` + nsTestProxyURL + `/v2/owner/app/tags/list?last=1.0&n=1&ns=` + url.QueryEscape(registry.host()) + `>; rel="next"`
	link := namespaced.Header().Get("Link")
	if link != wantNSLink {
		t.Fatalf("ns Link = %q, want %q", link, wantNSLink)
	}

	next := serveNS(routes, strings.TrimPrefix(strings.SplitN(link, ">", 2)[0], "<"))
	if next.Code != http.StatusOK {
		t.Fatalf("next page status = %d: %s", next.Code, next.Body.String())
	}
	if got, want := next.Body.String(), `{"name":"owner/app","tags":["2.0"]}`; got != want {
		t.Errorf("next page body = %q, want %q", got, want)
	}
	if got, want := registry.lastRequest(), "/v2/owner/app/tags/list?last=1.0&n=1"; got != want {
		t.Errorf("upstream request = %q, want %q (ns must not be forwarded)", got, want)
	}
}

func TestContainerHandler_NamespaceDefaultBypassesRepositoryPrefixRoutes(t *testing.T) {
	hub := newNSTestRegistry(t, "homebrew/core/jq", "")
	brew := newNSTestRegistry(t, "homebrew/core/jq", "")
	routes, h, _ := newNSTestHandler(t, hub.URL, nil)
	RegisterHomebrewArtifacts(h, brew.URL)

	if response := serveNS(routes, "/v2/homebrew/core/jq/manifests/latest?ns=docker.io"); response.Code != http.StatusOK {
		t.Fatalf("ns status = %d: %s", response.Code, response.Body.String())
	}
	if hub.requestCount() != 1 || brew.requestCount() != 0 {
		t.Errorf("ns request reached hub=%d brew=%d, want hub=1 brew=0", hub.requestCount(), brew.requestCount())
	}

	if response := serveNS(routes, "/v2/homebrew/core/jq/manifests/latest"); response.Code != http.StatusOK {
		t.Fatalf("unprefixed status = %d: %s", response.Code, response.Body.String())
	}
	if hub.requestCount() != 1 || brew.requestCount() != 1 {
		t.Errorf("unprefixed request reached hub=%d brew=%d, want hub=1 brew=1", hub.requestCount(), brew.requestCount())
	}
}

func TestContainerHandler_NamespaceMatchesConfiguredHosts(t *testing.T) {
	// Each pair is a configured registry URL and an ns value containerd sends
	// for an image reference on that registry. The scheme-default port may be
	// spelled out or left out on either side; any other port must match.
	matches := []struct {
		configured string
		namespace  string
	}{
		{"https://GHCR.io", "ghcr.io"},
		{"https://ghcr.io/", "GHCR.IO"},
		{"http://[fd00::1]:5000", "[fd00::1]:5000"},
		{"https://[fd00::1]", "[fd00::1]"},
		{"https://[fd00::1]", "[fd00::1]:443"},
		{"https://reg.example:80", "reg.example:80"},
		{"https://reg.example:443", "reg.example"},
		{"https://reg.example", "reg.example:443"},
		{"http://reg.example:80", "reg.example"},
		{"http://reg.example", "reg.example:80"},
		{"http://reg.example:5000", "reg.example:5000"},
	}
	for _, tt := range matches {
		t.Run(tt.configured+" "+tt.namespace, func(t *testing.T) {
			h := NewContainerHandlerWithRegistry(nil, nsTestProxyURL, "", map[string]string{"lab": tt.configured})
			registryURL, upstreamName, cacheName, ok := h.registryForNamespace(tt.namespace, "owner/app")
			if !ok {
				t.Fatalf("ns %q did not resolve for upstream %q", tt.namespace, tt.configured)
			}
			if registryURL != strings.TrimSuffix(tt.configured, "/") || upstreamName != "owner/app" || cacheName != "upstream/lab/owner/app" {
				t.Errorf("route = (%q, %q, %q), want (%q, owner/app, upstream/lab/owner/app)",
					registryURL, upstreamName, cacheName, tt.configured)
			}
		})
	}

	mismatches := []struct {
		configured string
		namespace  string
	}{
		{"http://reg.example:5000", "reg.example:5001"},
		{"https://reg.example:80", "reg.example"},
		{"https://reg.example", "reg.example:80"},
		{"http://reg.example:443", "reg.example"},
	}
	for _, tt := range mismatches {
		t.Run("mismatch "+tt.configured+" "+tt.namespace, func(t *testing.T) {
			h := NewContainerHandlerWithRegistry(nil, nsTestProxyURL, "", map[string]string{"lab": tt.configured})
			if _, _, _, ok := h.registryForNamespace(tt.namespace, "owner/app"); ok {
				t.Errorf("ns %q resolved for upstream %q, want no match", tt.namespace, tt.configured)
			}
		})
	}
}

func TestContainerHandler_NamespaceKeepsNonDefaultPorts(t *testing.T) {
	// Two registries on one host that differ only in the port, one of them on
	// the scheme-default port. Tests cannot listen on ports 80 or 443, so the
	// dialer maps those addresses to the fake registries.
	onDefaultPort := newNSTestRegistry(t, "owner/app", "")
	onOtherPort := newNSTestRegistry(t, "owner/app", "")
	routes, h, _ := newNSTestHandler(t, "", map[string]string{
		"std": "http://registry.example",
		"alt": "http://registry.example:443",
	})
	endpoints := map[string]string{
		"registry.example:80":  onDefaultPort.Listener.Addr().String(),
		"registry.example:443": onOtherPort.Listener.Addr().String(),
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		target, ok := endpoints[addr]
		if !ok {
			return nil, fmt.Errorf("unexpected dial to %s", addr)
		}
		return (&net.Dialer{}).DialContext(ctx, network, target)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	h.proxy.HTTPClient = &http.Client{Transport: transport}

	// Every request below misses the cache: a different registry or a
	// different endpoint than the request before it.
	tests := []struct {
		target      string
		wantDefault int
		wantOther   int
	}{
		{"/v2/owner/app/manifests/latest?ns=registry.example:80", 1, 0},
		{"/v2/owner/app/manifests/latest?ns=registry.example:443", 1, 1},
		{"/v2/owner/app/tags/list?ns=registry.example", 2, 1},
	}
	for _, tt := range tests {
		response := serveNS(routes, tt.target)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d: %s", tt.target, response.Code, response.Body.String())
		}
		if onDefaultPort.requestCount() != tt.wantDefault || onOtherPort.requestCount() != tt.wantOther {
			t.Errorf("after %s: requests default-port=%d other-port=%d, want %d/%d",
				tt.target, onDefaultPort.requestCount(), onOtherPort.requestCount(), tt.wantDefault, tt.wantOther)
		}
	}
}

func TestContainerHandler_NamespaceHostCollisions(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	logs := &bytes.Buffer{}
	proxy.Logger = slog.New(slog.NewTextHandler(logs, nil))
	h := NewContainerHandlerWithRegistry(proxy, nsTestProxyURL, "https://mirror.example", map[string]string{
		"zeta":   "https://shared.example",
		"alpha":  "https://Shared.example:443",
		"mirror": "https://mirror.example",
		"hub":    "https://registry-1.docker.io",
	})

	tests := map[string]string{
		"shared.example":       "https://Shared.example:443",
		"mirror.example":       "https://mirror.example",
		"registry-1.docker.io": "https://mirror.example",
	}
	for namespace, want := range tests {
		registryURL, _, cacheName, ok := h.registryForNamespace(namespace, "owner/app")
		if !ok || registryURL != want {
			t.Errorf("ns %q resolved to (%q, %v), want %q", namespace, registryURL, ok, want)
		}
		if namespace == "shared.example" && cacheName != "upstream/alpha/owner/app" {
			t.Errorf("ns %q cache name = %q, want upstream/alpha/owner/app", namespace, cacheName)
		}
	}
	for _, upstream := range []string{"upstream=zeta", "upstream=mirror", "upstream=hub"} {
		if !strings.Contains(logs.String(), upstream) {
			t.Errorf("missing collision warning for %s in logs:\n%s", upstream, logs.String())
		}
	}
}

func TestContainerHandler_NamespaceCollisionWarningNamesEachOwner(t *testing.T) {
	// r's two keys end up with two different owners; the warning has to
	// name both, or an admin fixing one entry misses the other.
	proxy, _, _, _ := setupTestProxy(t)
	logs := &bytes.Buffer{}
	proxy.Logger = slog.New(slog.NewTextHandler(logs, nil))
	h := NewContainerHandlerWithRegistry(proxy, nsTestProxyURL, "", map[string]string{
		"p": "http://h.example",
		"q": "http://h.example:443",
		"r": "https://h.example",
	})

	for namespace, want := range map[string]string{"h.example": "p", "h.example:80": "p", "h.example:443": "q"} {
		if got := h.namespaces[namespace]; got != want {
			t.Errorf("index[%q] = %q, want %q", namespace, got, want)
		}
	}
	for _, want := range []string{"upstream=r", "h.example=p", "h.example:443=q"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs missing %q:\n%s", want, logs.String())
		}
	}
}

func TestContainerHandler_NamespaceSkipsRegistryURLsWithPath(t *testing.T) {
	registry := newNSTestRegistry(t, "owner/app", "/artifactory/api/docker/remote")
	routes, _, logs := newNSTestHandler(t, "", map[string]string{
		"art": registry.URL + "/artifactory/api/docker/remote",
	})

	assertNameUnknown(t, serveNS(routes, "/v2/owner/app/manifests/latest?ns="+registry.host()))
	if got := registry.requestCount(); got != 0 {
		t.Errorf("upstream requests via ns = %d, want 0", got)
	}
	if !strings.Contains(logs.String(), "upstream=art") {
		t.Errorf("missing warning for path-prefixed upstream in logs:\n%s", logs.String())
	}

	// Per-registry mirrors with override_path reach it with ns attached.
	if response := serveNS(routes, "/v2/upstream/art/owner/app/manifests/latest?ns="+registry.host()); response.Code != http.StatusOK {
		t.Fatalf("prefix route with ns status = %d, want 200: %s", response.Code, response.Body.String())
	}
	if got := registry.requestCount(); got != 1 {
		t.Errorf("upstream requests via prefix route with ns = %d, want 1", got)
	}
	if response := serveNS(routes, "/v2/upstream/art/owner/app/manifests/latest"); response.Code != http.StatusOK {
		t.Fatalf("prefix route status = %d, want 200: %s", response.Code, response.Body.String())
	}

	t.Run("default registry", func(t *testing.T) {
		mirror := newNSTestRegistry(t, "library/nginx", "/hub")
		routes, _, logs := newNSTestHandler(t, mirror.URL+"/hub", nil)

		assertNameUnknown(t, serveNS(routes, "/v2/library/nginx/manifests/latest?ns="+mirror.host()))
		if got := mirror.requestCount(); got != 0 {
			t.Errorf("upstream requests via ns host = %d, want 0", got)
		}
		if response := serveNS(routes, "/v2/library/nginx/manifests/latest?ns=docker.io"); response.Code != http.StatusOK {
			t.Fatalf("ns=docker.io status = %d, want 200: %s", response.Code, response.Body.String())
		}
		if !strings.Contains(logs.String(), "default OCI registry") {
			t.Errorf("missing warning for path-prefixed default registry in logs:\n%s", logs.String())
		}
	})
}

func TestContainerHandler_TagsListIgnoresLegacyCacheRows(t *testing.T) {
	registry := newNSTestRegistry(t, "owner/app", "")
	routes, h, _ := newNSTestHandler(t, "", map[string]string{"ghcr": registry.URL})

	// Rows written before the raw-link format hold a Link already rewritten
	// for the route that filled them. Seed one under the legacy identity.
	query := url.Values{"n": {"1"}}
	legacySum := sha256.Sum256([]byte(registry.URL + "\x00owner/app\x00" + query.Encode()))
	legacyKey := hex.EncodeToString(legacySum[:])
	if legacyKey == h.containerTagsCacheKey(registry.URL, "owner/app", query) {
		t.Fatal("legacy and current tag-list cache keys are equal")
	}
	legacy := &cachedContainerTags{
		body:        []byte(`{"name":"owner/app","tags":["legacy"]}`),
		contentType: contentTypeJSON,
		link:        `<` + nsTestProxyURL + `/v2/upstream/ghcr/owner/app/tags/list?last=legacy&n=1>; rel="next"`,
		fetchedAt:   time.Now(),
	}
	if err := h.storeContainerTags(context.Background(), legacyKey, legacy); err != nil {
		t.Fatalf("store legacy tag list: %v", err)
	}

	response := serveNS(routes, "/v2/owner/app/tags/list?n=1&ns="+registry.host())
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	if got, want := response.Body.String(), `{"name":"owner/app","tags":["1.0"]}`; got != want {
		t.Errorf("body = %q, want %q (legacy row must not be served)", got, want)
	}
	if got := registry.requestCount(); got != 1 {
		t.Errorf("upstream requests = %d, want 1 (legacy row must not be served)", got)
	}
}

func TestContainerHandler_NamespaceWarningsRedactCredentials(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	logs := &bytes.Buffer{}
	proxy.Logger = slog.New(slog.NewTextHandler(logs, nil))
	NewContainerHandlerWithRegistry(proxy, nsTestProxyURL, "https://svc:s3cret@mirror.example/hub", map[string]string{
		"art": "https://bot:hunter2@art.example/artifactory/api/docker/remote",
	})

	for _, secret := range []string{"s3cret", "hunter2"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("logs contain credential %q:\n%s", secret, logs.String())
		}
	}
	for _, want := range []string{
		"svc:xxxxx@mirror.example/hub",
		"bot:xxxxx@art.example/artifactory",
		"Docker Hub aliases still select it",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs missing %q:\n%s", want, logs.String())
		}
	}
}
