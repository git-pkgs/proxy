package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	upstreamhttp "github.com/git-pkgs/proxy/internal/httpclient"
	"github.com/git-pkgs/registries/fetch"
)

func TestContainerHandler_parseBlobPath(t *testing.T) {
	h := &ContainerHandler{}

	tests := []struct {
		path       string
		wantName   string
		wantDigest string
	}{
		{
			path:       "library/nginx/blobs/sha256:abc123def456",
			wantName:   "library/nginx",
			wantDigest: "sha256:abc123def456",
		},
		{
			path:       "myorg/myrepo/blobs/sha256:0123456789abcdef",
			wantName:   "myorg/myrepo",
			wantDigest: "sha256:0123456789abcdef",
		},
		{
			path:       "deep/nested/repo/name/blobs/sha256:fedcba9876543210",
			wantName:   "deep/nested/repo/name",
			wantDigest: "sha256:fedcba9876543210",
		},
		{
			path:       "invalid/path",
			wantName:   "",
			wantDigest: "",
		},
		{
			path:       "repo/blobs/md5:invalid",
			wantName:   "",
			wantDigest: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			name, digest := h.parseBlobPath(tt.path)
			if name != tt.wantName {
				t.Errorf("parseBlobPath() name = %q, want %q", name, tt.wantName)
			}
			if digest != tt.wantDigest {
				t.Errorf("parseBlobPath() digest = %q, want %q", digest, tt.wantDigest)
			}
		})
	}
}

func TestContainerHandler_parseManifestPath(t *testing.T) {
	h := &ContainerHandler{}

	tests := []struct {
		path          string
		wantName      string
		wantReference string
	}{
		{
			path:          "library/nginx/manifests/latest",
			wantName:      "library/nginx",
			wantReference: "latest",
		},
		{
			path:          "myorg/myrepo/manifests/v1.0.0",
			wantName:      "myorg/myrepo",
			wantReference: "v1.0.0",
		},
		{
			path:          "repo/manifests/sha256:abc123",
			wantName:      "repo",
			wantReference: "sha256:abc123",
		},
		{
			path:     "invalid/path",
			wantName: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			name, ref := h.parseManifestPath(tt.path)
			if name != tt.wantName {
				t.Errorf("parseManifestPath() name = %q, want %q", name, tt.wantName)
			}
			if ref != tt.wantReference {
				t.Errorf("parseManifestPath() reference = %q, want %q", ref, tt.wantReference)
			}
		})
	}
}

func TestContainerHandler_parseTagsListPath(t *testing.T) {
	h := &ContainerHandler{}

	tests := []struct {
		path     string
		wantName string
	}{
		{
			path:     "library/nginx/tags/list",
			wantName: "library/nginx",
		},
		{
			path:     "myorg/myrepo/tags/list",
			wantName: "myorg/myrepo",
		},
		{
			path:     "invalid/path",
			wantName: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			name := h.parseTagsListPath(tt.path)
			if name != tt.wantName {
				t.Errorf("parseTagsListPath() = %q, want %q", name, tt.wantName)
			}
		})
	}
}

func TestContainerHandler_TagsListUsesStaleCacheOnUpstreamFailure(t *testing.T) {
	tags := `{"name":"library/nginx","tags":["1.0","latest"]}`
	upstreamAvailable := true
	upstreamRequests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests++
		if r.URL.Path != "/v2/library/nginx/tags/list" {
			http.NotFound(w, r)
			return
		}
		if !upstreamAvailable {
			http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"tags-etag"`)
		_, _ = io.WriteString(w, tags)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	proxy.MetadataTTL = 0
	h := &ContainerHandler{proxy: proxy, registryURL: upstream.URL, proxyURL: "http://localhost:8080"}

	first := httptest.NewRecorder()
	h.Routes().ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/library/nginx/tags/list?n=2", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("initial status = %d, want 200: %s", first.Code, first.Body.String())
	}
	if first.Body.String() != tags {
		t.Errorf("initial body = %q, want %q", first.Body.String(), tags)
	}

	upstreamAvailable = false
	second := httptest.NewRecorder()
	h.Routes().ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/library/nginx/tags/list?n=2", nil))
	if second.Code != http.StatusOK {
		t.Fatalf("stale status = %d, want 200: %s", second.Code, second.Body.String())
	}
	if second.Body.String() != tags {
		t.Errorf("stale body = %q, want %q", second.Body.String(), tags)
	}
	if got := second.Header().Get("Warning"); got != `110 - "Response is Stale"` {
		t.Errorf("Warning = %q, want stale warning", got)
	}
	if upstreamRequests != 2 {
		t.Errorf("upstream requests = %d, want 2", upstreamRequests)
	}
}

func TestContainerHandler_TagsListCachesPaginationLink(t *testing.T) {
	tags := `{"name":"library/nginx","tags":["1.0"]}`
	upstreamAvailable := true
	upstreamRequests := 0
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests++
		if !upstreamAvailable {
			http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Link", `<`+upstream.URL+`/v2/library/nginx/tags/list?last=1.0&n=2>; rel="next"`)
		_, _ = io.WriteString(w, tags)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	proxy.MetadataTTL = time.Hour
	h := &ContainerHandler{proxy: proxy, registryURL: upstream.URL, proxyURL: "http://proxy.example.test"}
	wantLink := `<http://proxy.example.test/v2/library/nginx/tags/list?last=1.0&n=2>; rel="next"`

	warmRequest := httptest.NewRequest(http.MethodGet, "/library/nginx/tags/list?n=2", nil)
	warm := httptest.NewRecorder()
	h.Routes().ServeHTTP(warm, warmRequest)
	if warm.Code != http.StatusOK {
		t.Fatalf("warm status = %d, want 200: %s", warm.Code, warm.Body.String())
	}
	if got := warm.Header().Get("Link"); got != wantLink {
		t.Errorf("warm Link = %q, want %q", got, wantLink)
	}

	fresh := httptest.NewRecorder()
	h.Routes().ServeHTTP(fresh, httptest.NewRequest(http.MethodGet, "/library/nginx/tags/list?n=2", nil))
	if fresh.Code != http.StatusOK {
		t.Fatalf("fresh status = %d, want 200: %s", fresh.Code, fresh.Body.String())
	}
	if got := fresh.Header().Get("Link"); got != wantLink {
		t.Errorf("fresh Link = %q, want %q", got, wantLink)
	}
	if upstreamRequests != 1 {
		t.Fatalf("upstream requests after fresh cache hit = %d, want 1", upstreamRequests)
	}

	proxy.MetadataTTL = 0
	upstreamAvailable = false
	stale := httptest.NewRecorder()
	h.Routes().ServeHTTP(stale, httptest.NewRequest(http.MethodGet, "/library/nginx/tags/list?n=2", nil))
	if stale.Code != http.StatusOK {
		t.Fatalf("stale status = %d, want 200: %s", stale.Code, stale.Body.String())
	}
	if got := stale.Header().Get("Link"); got != wantLink {
		t.Errorf("stale Link = %q, want %q", got, wantLink)
	}
	if got := stale.Header().Get("Warning"); got != `110 - "Response is Stale"` {
		t.Errorf("stale Warning = %q, want stale warning", got)
	}
	if upstreamRequests != 2 {
		t.Errorf("upstream requests after stale fallback = %d, want 2", upstreamRequests)
	}
}

func TestContainerHandler_TagsListRewritesRelativePaginationLinkForNamedRegistry(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/owner/repo/tags/list" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("n") != "1" {
			http.Error(w, "unexpected page size", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("last") == "" {
			w.Header().Set("Link", `</v2/owner/repo/tags/list?last=1.0&n=1>; rel="next"`)
			_, _ = io.WriteString(w, `{"name":"owner/repo","tags":["1.0"]}`)
			return
		}
		if r.URL.Query().Get("last") != "1.0" {
			http.Error(w, "unexpected pagination token", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `{"name":"owner/repo","tags":["2.0"]}`)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	h := NewContainerHandler(proxy, "http://proxy.example.test", map[string]string{"test": upstream.URL})
	routes := http.StripPrefix("/v2", h.Routes())

	first := httptest.NewRecorder()
	routes.ServeHTTP(first, httptest.NewRequest(http.MethodGet,
		"/v2/upstream/test/owner/repo/tags/list?n=1", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first page status = %d, want 200: %s", first.Code, first.Body.String())
	}
	const wantLink = `<http://proxy.example.test/v2/upstream/test/owner/repo/tags/list?last=1.0&n=1>; rel="next"`
	if got := first.Header().Get("Link"); got != wantLink {
		t.Fatalf("first page Link = %q, want %q", got, wantLink)
	}

	nextURL := strings.TrimPrefix(strings.SplitN(first.Header().Get("Link"), ">", 2)[0], "<")
	next := httptest.NewRecorder()
	routes.ServeHTTP(next, httptest.NewRequest(http.MethodGet, nextURL, nil))
	if next.Code != http.StatusOK {
		t.Fatalf("next page status = %d, want 200: %s", next.Code, next.Body.String())
	}
	if got, want := next.Body.String(), `{"name":"owner/repo","tags":["2.0"]}`; got != want {
		t.Errorf("next page body = %q, want %q", got, want)
	}
}

func TestContainerHandler_NamedOCIRegistryServesHelmArtifacts(t *testing.T) {
	const blob = "chart archive"
	digest := "sha256:" + sha256Hex(blob)
	manifest := `{"schemaVersion":2,"config":{"mediaType":"application/vnd.cncf.helm.config.v1+json"},"layers":[{"mediaType":"application/vnd.cncf.helm.chart.content.v1.tar+gzip","digest":"` + digest + `"}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/owner/demo/manifests/1.0.0":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", "sha256:"+sha256Hex(manifest))
			_, _ = io.WriteString(w, manifest)
		case "/v2/owner/demo/blobs/" + digest:
			w.Header().Set("Content-Type", "application/vnd.cncf.helm.chart.content.v1.tar+gzip")
			_, _ = io.WriteString(w, blob)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	h := NewContainerHandler(proxy, "http://proxy.example", map[string]string{"ghcr": upstream.URL})

	manifestResponse := httptest.NewRecorder()
	h.Routes().ServeHTTP(manifestResponse,
		httptest.NewRequest(http.MethodGet, "/upstream/ghcr/owner/demo/manifests/1.0.0", nil))
	if manifestResponse.Code != http.StatusOK {
		t.Fatalf("manifest status = %d, want 200: %s", manifestResponse.Code, manifestResponse.Body.String())
	}
	if got := manifestResponse.Header().Get("Content-Type"); got != "application/vnd.oci.image.manifest.v1+json" {
		t.Errorf("manifest Content-Type = %q", got)
	}

	blobResponse := httptest.NewRecorder()
	h.Routes().ServeHTTP(blobResponse,
		httptest.NewRequest(http.MethodGet, "/upstream/ghcr/owner/demo/blobs/"+digest, nil))
	if blobResponse.Code != http.StatusOK {
		t.Fatalf("blob status = %d, want 200: %s", blobResponse.Code, blobResponse.Body.String())
	}
	if got := blobResponse.Header().Get("Content-Type"); got != "application/vnd.cncf.helm.chart.content.v1.tar+gzip" {
		t.Errorf("blob Content-Type = %q", got)
	}
}

func TestContainerHandler_registryURLForUsesLongestRepositoryPrefix(t *testing.T) {
	h := &ContainerHandler{registryURL: "https://registry-1.docker.io"}
	h.RegisterRegistry("homebrew", "https://example.test")
	h.RegisterRegistry("homebrew/core", "https://ghcr.io/")

	tests := map[string]string{
		"homebrew/core":            "https://ghcr.io",
		"homebrew/core/jq":         "https://ghcr.io",
		"homebrew/portable-ruby":   "https://example.test",
		"homebrew-core/jq":         "https://registry-1.docker.io",
		"library/homebrew/core/jq": "https://registry-1.docker.io",
	}
	for name, want := range tests {
		if got := h.registryURLFor(name); got != want {
			t.Errorf("registryURLFor(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestContainerHandler_BlobDownload_DiscoversBearerChallenge(t *testing.T) {
	blob := "upstream blob"
	digest := sha256Digest([]byte(blob))
	registryRequests := 0
	tokenRequests := 0
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			tokenRequests++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      "discovered-token",
				"expires_in": 3600,
			})
		case "/v2/library/nginx/blobs/" + digest:
			registryRequests++
			if r.Header.Get("Authorization") != "Bearer discovered-token" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+upstream.URL+`/token",service="registry.test",scope="repository:library/nginx:pull"`)
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = io.WriteString(w, blob)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	authTransport := upstreamhttp.NewTransport(http.DefaultTransport, nil)
	client := &http.Client{Transport: authTransport}
	artifactFetcher := fetch.NewFetcher(
		fetch.WithHTTPClient(client),
		fetch.WithMaxRetries(0),
	)
	t.Cleanup(func() { _ = artifactFetcher.Close() })
	proxy.Fetcher = artifactFetcher
	proxy.HTTPClient = client

	h := &ContainerHandler{
		proxy:       proxy,
		registryURL: upstream.URL,
		proxyURL:    "http://localhost:8080",
	}

	for range 2 {
		req := httptest.NewRequest(http.MethodGet, "/library/nginx/blobs/"+digest, nil)
		w := httptest.NewRecorder()
		h.Routes().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
		}
		if got := w.Body.String(); got != blob {
			t.Errorf("body = %q, want %q", got, blob)
		}
	}

	if tokenRequests != 1 {
		t.Errorf("token requests = %d, want 1", tokenRequests)
	}
	if registryRequests != 2 {
		t.Errorf("registry requests = %d, want 2", registryRequests)
	}
}

func TestContainerHandler_HomebrewBlobDoesNotForwardClientCredentialsToRegistryOrCDN(t *testing.T) {
	blob := "homebrew bottle"
	digest := sha256Digest([]byte(blob))
	var registryAuthorization, cdnAuthorization string

	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/vnd.homebrew.bottle")
		_, _ = io.WriteString(w, blob)
	}))
	defer cdn.Close()

	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registryAuthorization = r.Header.Get("Authorization")
		http.Redirect(w, r, cdn.URL+"/bottle", http.StatusTemporaryRedirect)
	}))
	defer registry.Close()

	defaultRequests := 0
	defaultRegistry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defaultRequests++
		http.NotFound(w, r)
	}))
	defer defaultRegistry.Close()

	proxy, _, _, _ := setupTestProxy(t)
	client := registry.Client()
	artifactFetcher := fetch.NewFetcher(
		fetch.WithHTTPClient(client),
		fetch.WithMaxRetries(0),
	)
	t.Cleanup(func() { _ = artifactFetcher.Close() })
	proxy.Fetcher = artifactFetcher

	h := &ContainerHandler{proxy: proxy, registryURL: defaultRegistry.URL}
	h.RegisterRegistry("homebrew/core", registry.URL)
	req := httptest.NewRequest(http.MethodGet, "/homebrew/core/jq/blobs/"+digest, nil)
	req.Header.Set("Authorization", "Bearer client-secret")
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if got := w.Body.String(); got != blob {
		t.Errorf("body = %q, want %q", got, blob)
	}
	if got := w.Header().Get("Content-Type"); got != "application/vnd.homebrew.bottle" {
		t.Errorf("Content-Type = %q, want application/vnd.homebrew.bottle", got)
	}
	if registryAuthorization != "" {
		t.Errorf("registry Authorization = %q, want empty", registryAuthorization)
	}
	if cdnAuthorization != "" {
		t.Errorf("CDN Authorization = %q, want empty", cdnAuthorization)
	}
	if defaultRequests != 0 {
		t.Errorf("default registry requests = %d, want 0", defaultRequests)
	}
}

func TestContainerHandler_BlobDigestMismatchIsNotCached(t *testing.T) {
	proxy, _, store, fetcher := setupTestProxy(t)
	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("wrong bottle")),
		ContentType: "application/octet-stream",
	}
	digest := sha256Digest([]byte("expected bottle"))
	h := &ContainerHandler{proxy: proxy, registryURL: "https://registry.example.test"}

	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/homebrew/core/jq/blobs/"+digest, nil))

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadGateway, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "DIGEST_INVALID") {
		t.Errorf("body = %q, want DIGEST_INVALID", w.Body.String())
	}
	cached, err := proxy.GetCachedArtifact(t.Context(), "oci", "homebrew/core/jq", digest, digest)
	if err != nil {
		t.Fatalf("checking cache: %v", err)
	}
	if cached != nil {
		t.Error("digest-mismatched blob was recorded in the cache")
	}
	if len(store.files) != 0 {
		t.Errorf("stored files = %d, want 0", len(store.files))
	}
}

func TestContainerHandler_CachedImagePullSurvivesRegistryAndTokenOutages(t *testing.T) {
	manifest := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`
	blob := "cached image blob"
	manifestDigest := sha256Digest([]byte(manifest))
	blobDigest := sha256Digest([]byte(blob))
	registryAvailable := true
	tokenAvailable := true
	registryRequests := 0
	tokenRequests := 0

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tokenRequests++
		if !tokenAvailable {
			http.Error(w, "token service unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "discovered-token",
			"expires_in": 3600,
		})
	}))
	defer tokenServer.Close()

	registryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registryRequests++
		if !registryAvailable {
			http.Error(w, "registry unavailable", http.StatusServiceUnavailable)
			return
		}
		if r.Header.Get("Authorization") != "Bearer discovered-token" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+tokenServer.URL+`",service="registry.test",scope="repository:library/nginx:pull"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}

		switch r.URL.Path {
		case "/v2/library/nginx/manifests/latest":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			_, _ = io.WriteString(w, manifest)
		case "/v2/library/nginx/blobs/" + blobDigest:
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = io.WriteString(w, blob)
		default:
			http.NotFound(w, r)
		}
	}))
	defer registryServer.Close()

	warmProxy, db, store, _ := setupTestProxy(t)
	warmClient := &http.Client{Transport: upstreamhttp.NewTransport(http.DefaultTransport, nil)}
	warmFetcher := fetch.NewFetcher(
		fetch.WithHTTPClient(warmClient),
		fetch.WithMaxRetries(0),
	)
	t.Cleanup(func() { _ = warmFetcher.Close() })
	warmProxy.Fetcher = warmFetcher
	warmProxy.HTTPClient = warmClient
	warmProxy.MetadataTTL = time.Hour
	warmHandler := (&ContainerHandler{
		proxy:       warmProxy,
		registryURL: registryServer.URL,
		proxyURL:    "http://localhost:8080",
	}).Routes()

	for _, request := range []struct {
		path string
		body string
	}{
		{path: "/library/nginx/manifests/latest", body: manifest},
		{path: "/library/nginx/blobs/" + blobDigest, body: blob},
	} {
		response := httptest.NewRecorder()
		warmHandler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, request.path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("warming %s: status = %d, want %d; body: %s", request.path, response.Code, http.StatusOK, response.Body.String())
		}
		if got := response.Body.String(); got != request.body {
			t.Fatalf("warming %s: body = %q, want %q", request.path, got, request.body)
		}
	}

	warmRegistryRequests := registryRequests
	warmTokenRequests := tokenRequests
	registryAvailable = false
	tokenAvailable = false

	offlineClient := &http.Client{Transport: upstreamhttp.NewTransport(http.DefaultTransport, nil)}
	offlineFetcher := fetch.NewFetcher(
		fetch.WithHTTPClient(offlineClient),
		fetch.WithMaxRetries(0),
	)
	t.Cleanup(func() { _ = offlineFetcher.Close() })
	offlineProxy := NewProxy(db, store, offlineFetcher, fetch.NewResolver(), warmProxy.Logger)
	offlineProxy.HTTPClient = offlineClient
	offlineProxy.MetadataTTL = time.Hour
	offlineHandler := (&ContainerHandler{
		proxy:       offlineProxy,
		registryURL: registryServer.URL,
		proxyURL:    "http://localhost:8080",
	}).Routes()

	for _, request := range []struct {
		name   string
		path   string
		body   string
		digest string
	}{
		{name: "tag manifest", path: "/library/nginx/manifests/latest", body: manifest, digest: manifestDigest},
		{name: "digest manifest", path: "/library/nginx/manifests/" + manifestDigest, body: manifest, digest: manifestDigest},
		{name: "blob", path: "/library/nginx/blobs/" + blobDigest, body: blob, digest: blobDigest},
	} {
		t.Run(request.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			offlineHandler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, request.path, nil))
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body: %s", response.Code, http.StatusOK, response.Body.String())
			}
			if got := response.Body.String(); got != request.body {
				t.Errorf("body = %q, want %q", got, request.body)
			}
			if got := response.Header().Get("Docker-Content-Digest"); got != request.digest {
				t.Errorf("Docker-Content-Digest = %q, want %q", got, request.digest)
			}
		})
	}

	if registryRequests != warmRegistryRequests {
		t.Errorf("offline registry requests = %d, want 0", registryRequests-warmRegistryRequests)
	}
	if tokenRequests != warmTokenRequests {
		t.Errorf("offline token requests = %d, want 0", tokenRequests-warmTokenRequests)
	}
}

func TestContainerHandler_BlobDownload_CacheHitSkipsAuth(t *testing.T) {
	proxy, db, store, fetcher := setupTestProxy(t)
	digest := "sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abcd"
	seedPackage(t, db, store, "oci", "library/nginx", digest, digest, "cached blob")

	upstreamRequests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamRequests++
		http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	h := &ContainerHandler{
		proxy:       proxy,
		registryURL: upstream.URL,
		proxyURL:    "http://localhost:8080",
	}

	req := httptest.NewRequest(http.MethodGet, "/library/nginx/blobs/"+digest, nil)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if got := w.Body.String(); got != "cached blob" {
		t.Errorf("body = %q, want %q", got, "cached blob")
	}
	if upstreamRequests != 0 {
		t.Errorf("upstream requests = %d, want 0", upstreamRequests)
	}
	if fetcher.fetchCalled {
		t.Error("fetcher should not be called on cache hit")
	}
}

func TestContainerHandler_BlobHead_CacheHitSkipsUpstreamAndAuth(t *testing.T) {
	proxy, db, store, fetcher := setupTestProxy(t)
	digest := "sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abcd"
	seedPackage(t, db, store, "oci", "library/nginx", digest, digest, "cached blob")

	upstreamRequests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamRequests++
		http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()
	proxy.HTTPClient = upstream.Client()

	h := &ContainerHandler{
		proxy:       proxy,
		registryURL: upstream.URL,
		proxyURL:    "http://localhost:8080",
	}

	req := httptest.NewRequest(http.MethodHead, "/library/nginx/blobs/"+digest, nil)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if got := w.Header().Get("Docker-Content-Digest"); got != digest {
		t.Errorf("Docker-Content-Digest = %q, want %q", got, digest)
	}
	if got := w.Header().Get("Content-Length"); got != "11" {
		t.Errorf("Content-Length = %q, want %q", got, "11")
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD response body length = %d, want 0", w.Body.Len())
	}
	if upstreamRequests != 0 {
		t.Errorf("upstream requests = %d, want 0", upstreamRequests)
	}
	if fetcher.fetchCalled {
		t.Error("fetcher should not be called on cache hit")
	}
}

func TestContainerHandler_BlobHead_DirectServeRedirects(t *testing.T) {
	proxy, db, store, fetcher := setupTestProxy(t)
	digest := "sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abcd"
	seedPackage(t, db, store, "oci", "library/nginx", digest, digest, "cached blob")
	store.signedURL = "https://storage.example.test/cached-blob?signature=test"
	proxy.DirectServe = true

	h := &ContainerHandler{
		proxy:       proxy,
		registryURL: "https://registry.example.test",
		proxyURL:    "http://localhost:8080",
	}

	req := httptest.NewRequest(http.MethodHead, "/library/nginx/blobs/"+digest, nil)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusFound)
	}
	if got := w.Header().Get("Location"); got != store.signedURL {
		t.Errorf("Location = %q, want %q", got, store.signedURL)
	}
	wantETag := `"` + sha256Hex("cached blob") + `"`
	if got := w.Header().Get("ETag"); got != wantETag {
		t.Errorf("ETag = %q, want %q", got, wantETag)
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD response body length = %d, want 0", w.Body.Len())
	}
	if fetcher.fetchCalled {
		t.Error("fetcher should not be called on cache hit")
	}
}

func TestContainerHandler_ManifestByDigest_CacheHitSkipsUpstream(t *testing.T) {
	manifest := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`
	digest := sha256Digest([]byte(manifest))
	lastModified := time.Date(2026, time.August, 14, 9, 30, 0, 0, time.UTC)
	upstreamAvailable := true
	upstreamRequests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests++
		if !upstreamAvailable {
			http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path != "/v2/library/nginx/manifests/"+digest {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("ETag", `"manifest-etag"`)
		w.Header().Set("Last-Modified", lastModified.Format(http.TimeFormat))
		if r.Method != http.MethodHead {
			_, _ = io.WriteString(w, manifest)
		}
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	h := &ContainerHandler{proxy: proxy, registryURL: upstream.URL, proxyURL: "http://localhost:8080"}

	first := httptest.NewRecorder()
	h.Routes().ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/library/nginx/manifests/"+digest, nil))
	if first.Code != http.StatusOK {
		t.Fatalf("initial status = %d, want %d; body: %s", first.Code, http.StatusOK, first.Body.String())
	}
	if first.Body.String() != manifest {
		t.Fatalf("initial body = %q, want %q", first.Body.String(), manifest)
	}

	upstreamAvailable = false
	second := httptest.NewRecorder()
	h.Routes().ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/library/nginx/manifests/"+digest, nil))
	if second.Code != http.StatusOK {
		t.Fatalf("cached status = %d, want %d; body: %s", second.Code, http.StatusOK, second.Body.String())
	}
	if second.Body.String() != manifest {
		t.Errorf("cached body = %q, want %q", second.Body.String(), manifest)
	}
	if got := second.Header().Get("Docker-Content-Digest"); got != digest {
		t.Errorf("cached Docker-Content-Digest = %q, want %q", got, digest)
	}
	if got := second.Header().Get("Last-Modified"); got != lastModified.Format(http.TimeFormat) {
		t.Errorf("cached Last-Modified = %q, want %q", got, lastModified.Format(http.TimeFormat))
	}

	conditionalRequest := httptest.NewRequest(http.MethodGet, "/library/nginx/manifests/"+digest, nil)
	conditionalRequest.Header.Set("If-None-Match", `"manifest-etag"`)
	conditional := httptest.NewRecorder()
	h.Routes().ServeHTTP(conditional, conditionalRequest)
	if conditional.Code != http.StatusNotModified {
		t.Fatalf("conditional status = %d, want %d", conditional.Code, http.StatusNotModified)
	}
	if got := conditional.Header().Get("ETag"); got != `"manifest-etag"` {
		t.Errorf("conditional ETag = %q, want %q", got, `"manifest-etag"`)
	}
	if conditional.Body.Len() != 0 {
		t.Errorf("conditional body length = %d, want 0", conditional.Body.Len())
	}

	head := httptest.NewRecorder()
	h.Routes().ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/library/nginx/manifests/"+digest, nil))
	if head.Code != http.StatusOK {
		t.Fatalf("cached HEAD status = %d, want %d", head.Code, http.StatusOK)
	}
	wantLength := strconv.Itoa(len(manifest))
	if got := head.Header().Get("Content-Length"); got != wantLength {
		t.Errorf("cached HEAD Content-Length = %q, want %q", got, wantLength)
	}
	if head.Body.Len() != 0 {
		t.Errorf("cached HEAD body length = %d, want 0", head.Body.Len())
	}
	if upstreamRequests != 1 {
		t.Errorf("upstream requests = %d, want 1", upstreamRequests)
	}
}

func TestContainerHandler_ManifestDigestMismatchIsNotCached(t *testing.T) {
	manifest := `{"schemaVersion":2}`
	digest := sha256Digest([]byte("different manifest"))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", digest)
		_, _ = io.WriteString(w, manifest)
	}))
	defer upstream.Close()

	proxy, db, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	h := &ContainerHandler{proxy: proxy, registryURL: upstream.URL}
	req := httptest.NewRequest(http.MethodGet, "/homebrew/core/jq/manifests/"+digest, nil)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadGateway, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "DIGEST_INVALID") {
		t.Errorf("body = %q, want DIGEST_INVALID", w.Body.String())
	}
	cacheKey := h.containerManifestCacheKey(upstream.URL, "homebrew/core/jq", digest, containerManifestAccept(req))
	entry, err := db.GetMetadataCache(containerManifestCacheEcosystem, cacheKey)
	if err != nil {
		t.Fatalf("checking manifest cache: %v", err)
	}
	if entry != nil {
		t.Error("digest-mismatched manifest was recorded in the cache")
	}
}

func TestContainerHandler_ManifestNonSHA256DigestReferenceIsProxied(t *testing.T) {
	manifest := `{"schemaVersion":2}`
	sha512Reference := "sha512:" + strings.Repeat("a", 128)
	sha512Header := "sha512:" + strings.Repeat("b", 128)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", sha512Header)
		_, _ = io.WriteString(w, manifest)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	h := &ContainerHandler{proxy: proxy, registryURL: upstream.URL}

	for _, reference := range []string{sha512Reference, "latest"} {
		t.Run(reference, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/library/nginx/manifests/"+reference, nil))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
			}
			if got := w.Body.String(); got != manifest {
				t.Errorf("body = %q, want %q", got, manifest)
			}
			if got := w.Header().Get("Docker-Content-Digest"); got != sha512Header {
				t.Errorf("Docker-Content-Digest = %q, want %q", got, sha512Header)
			}
		})
	}
}

func TestContainerHandler_ManifestTagWithInvalidDigestIsNotAliased(t *testing.T) {
	manifest := `{"schemaVersion":2}`
	invalidDigest := sha256Digest([]byte("different manifest"))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", invalidDigest)
		_, _ = io.WriteString(w, manifest)
	}))
	defer upstream.Close()

	proxy, db, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	h := &ContainerHandler{proxy: proxy, registryURL: upstream.URL}
	req := httptest.NewRequest(http.MethodGet, "/homebrew/core/jq/manifests/latest", nil)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadGateway, w.Body.String())
	}
	cacheKey := h.containerManifestCacheKey(upstream.URL, "homebrew/core/jq", invalidDigest, containerManifestAccept(req))
	entry, err := db.GetMetadataCache(containerManifestCacheEcosystem, cacheKey)
	if err != nil {
		t.Fatalf("checking manifest cache: %v", err)
	}
	if entry != nil {
		t.Error("tag manifest was cached under an unverified digest")
	}
}

func TestContainerHandler_ManifestByTag_UsesStaleCacheOnUpstreamFailure(t *testing.T) {
	manifest := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json"}`
	digest := sha256Digest([]byte(manifest))
	upstreamAvailable := true
	upstreamRequests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamRequests++
		if !upstreamAvailable {
			http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
		w.Header().Set("Docker-Content-Digest", digest)
		_, _ = io.WriteString(w, manifest)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	proxy.MetadataTTL = 0
	h := &ContainerHandler{proxy: proxy, registryURL: upstream.URL, proxyURL: "http://localhost:8080"}

	first := httptest.NewRecorder()
	h.Routes().ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/library/nginx/manifests/latest", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("initial status = %d, want %d; body: %s", first.Code, http.StatusOK, first.Body.String())
	}

	upstreamAvailable = false
	second := httptest.NewRecorder()
	h.Routes().ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/library/nginx/manifests/latest", nil))
	if second.Code != http.StatusOK {
		t.Fatalf("stale status = %d, want %d; body: %s", second.Code, http.StatusOK, second.Body.String())
	}
	if second.Body.String() != manifest {
		t.Errorf("stale body = %q, want %q", second.Body.String(), manifest)
	}
	if got := second.Header().Get("Warning"); got != `110 - "Response is Stale"` {
		t.Errorf("Warning = %q, want stale warning", got)
	}
	if got := second.Header().Get("Docker-Content-Digest"); got != digest {
		t.Errorf("stale Docker-Content-Digest = %q, want %q", got, digest)
	}
	if upstreamRequests != 2 {
		t.Errorf("upstream requests = %d, want 2", upstreamRequests)
	}
}

func TestContainerHandler_ManifestVariantCacheNormalizesCompatibleAccept(t *testing.T) {
	manifest := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json"}`
	digest := sha256Digest([]byte(manifest))
	upstreamAvailable := true
	upstreamRequests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests++
		if !upstreamAvailable {
			http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "")
		w.Header().Set("Docker-Content-Digest", digest)
		_, _ = io.WriteString(w, manifest)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	proxy.MetadataTTL = 0
	h := &ContainerHandler{proxy: proxy, registryURL: upstream.URL, proxyURL: "http://localhost:8080"}

	firstRequest := httptest.NewRequest(http.MethodGet, "/library/nginx/manifests/latest", nil)
	firstRequest.Header.Set("Accept", "application/vnd.oci.image.index.v1+json;q=1, application/vnd.oci.image.manifest.v1+json;q=1, application/vnd.oci.image.index.v1+json;q=1")
	first := httptest.NewRecorder()
	h.Routes().ServeHTTP(first, firstRequest)
	if first.Code != http.StatusOK {
		t.Fatalf("initial status = %d, want 200: %s", first.Code, first.Body.String())
	}

	upstreamAvailable = false
	secondRequest := httptest.NewRequest(http.MethodGet, "/library/nginx/manifests/latest", nil)
	secondRequest.Header.Set("Accept", " application/vnd.oci.image.manifest.v1+json , application/vnd.oci.image.index.v1+json ")
	second := httptest.NewRecorder()
	h.Routes().ServeHTTP(second, secondRequest)
	if second.Code != http.StatusOK {
		t.Fatalf("stale status = %d, want 200: %s", second.Code, second.Body.String())
	}
	if second.Body.String() != manifest {
		t.Errorf("stale body = %q, want %q", second.Body.String(), manifest)
	}
	if got := second.Header().Get("Warning"); got != `110 - "Response is Stale"` {
		t.Errorf("Warning = %q, want stale warning", got)
	}
	if upstreamRequests != 2 {
		t.Errorf("upstream requests = %d, want 2", upstreamRequests)
	}
}

func TestContainerHandler_ManifestMigratesLegacyAcceptCacheKey(t *testing.T) {
	digest := "sha256:cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
	manifest := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json"}`
	upstreamRequests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests++
		http.Error(w, "upstream should not be called", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	proxy.MetadataTTL = time.Hour
	h := &ContainerHandler{proxy: proxy, registryURL: upstream.URL, proxyURL: "http://localhost:8080"}
	accept := "application/vnd.oci.image.index.v1+json;q=1, application/vnd.oci.image.manifest.v1+json;q=1"
	cacheAccept := normalizeContainerManifestAccept(accept)
	legacyCacheKey := h.containerManifestCacheKey(upstream.URL, "library/nginx", "latest", accept)
	cacheKey := h.containerManifestCacheKey(upstream.URL, "library/nginx", "latest", cacheAccept)
	if legacyCacheKey == cacheKey {
		t.Fatal("legacy and normalized cache keys are equal")
	}

	request := httptest.NewRequest(http.MethodGet, "/library/nginx/manifests/latest", nil)
	request.Header.Set("Accept", accept)
	legacyManifest := &cachedContainerManifest{
		body:          []byte(manifest),
		contentType:   "application/vnd.oci.image.index.v1+json",
		contentDigest: digest,
		fetchedAt:     time.Now(),
	}
	if err := h.storeContainerManifest(request.Context(), legacyCacheKey, legacyManifest); err != nil {
		t.Fatalf("store legacy manifest: %v", err)
	}

	response := httptest.NewRecorder()
	h.Routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	if response.Body.String() != manifest {
		t.Errorf("body = %q, want %q", response.Body.String(), manifest)
	}
	if upstreamRequests != 0 {
		t.Errorf("upstream requests = %d, want 0", upstreamRequests)
	}
	migrated, err := h.loadContainerManifest(request.Context(), cacheKey)
	if err != nil {
		t.Fatalf("load migrated manifest: %v", err)
	}
	if migrated == nil {
		t.Error("normalized cache entry was not created")
	}
}

func TestContainerHandler_ManifestDualWritesLegacyAcceptCacheKeys(t *testing.T) {
	manifest := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`
	digest := sha256Digest([]byte(manifest))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/library/nginx/manifests/latest" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", digest)
		_, _ = io.WriteString(w, manifest)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	h := &ContainerHandler{proxy: proxy, registryURL: upstream.URL, proxyURL: "http://localhost:8080"}
	accept := "application/vnd.oci.image.manifest.v1+json;q=1, application/vnd.oci.image.manifest.v1+json;q=1"
	request := httptest.NewRequest(http.MethodGet, "/library/nginx/manifests/latest", nil)
	request.Header.Set("Accept", accept)
	response := httptest.NewRecorder()
	h.Routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}

	for _, reference := range []string{"latest", digest} {
		legacyCacheKey := h.containerManifestCacheKey(upstream.URL, "library/nginx", reference, accept)
		cached, err := h.loadContainerManifest(request.Context(), legacyCacheKey)
		if err != nil {
			t.Fatalf("load legacy %s manifest: %v", reference, err)
		}
		if cached == nil {
			t.Errorf("legacy %s cache entry was not written", reference)
		}
	}
}

func TestContainerHandler_ManifestVariantCacheHonorsSpecificAcceptExclusions(t *testing.T) {
	manifest := `{"schemaVersion":2}`
	digest := sha256Digest([]byte(manifest))
	upstreamAvailable := true
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !upstreamAvailable {
			http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
		w.Header().Set("Docker-Content-Digest", digest)
		_, _ = io.WriteString(w, manifest)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	proxy.MetadataTTL = 0
	h := &ContainerHandler{proxy: proxy, registryURL: upstream.URL, proxyURL: "http://localhost:8080"}

	warmRequest := httptest.NewRequest(http.MethodGet, "/library/nginx/manifests/latest", nil)
	warmRequest.Header.Set("Accept", "application/vnd.oci.image.index.v1+json;q=0, */*;q=1")
	warm := httptest.NewRecorder()
	h.Routes().ServeHTTP(warm, warmRequest)
	if warm.Code != http.StatusOK {
		t.Fatalf("warm status = %d, want 200", warm.Code)
	}

	upstreamAvailable = false
	offlineRequest := httptest.NewRequest(http.MethodGet, "/library/nginx/manifests/latest", nil)
	offlineRequest.Header.Set("Accept", "application/vnd.oci.image.index.v1+json;q=0, */*;q=1")
	offline := httptest.NewRecorder()
	h.Routes().ServeHTTP(offline, offlineRequest)
	if offline.Code != http.StatusServiceUnavailable {
		t.Errorf("offline status = %d, want 503", offline.Code)
	}
}

func TestContainerHandler_ManifestVariantCacheHonorsParameterizedAcceptExclusions(t *testing.T) {
	contentType := "application/vnd.oci.image.index.v1+json; charset=utf-8"
	upstreamAvailable := true
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !upstreamAvailable {
			http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", contentType)
		_, _ = io.WriteString(w, `{"schemaVersion":2}`)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	proxy.MetadataTTL = 0
	h := &ContainerHandler{proxy: proxy, registryURL: upstream.URL, proxyURL: "http://localhost:8080"}
	accept := "application/vnd.oci.image.index.v1+json;q=1, application/vnd.oci.image.index.v1+json;charset=utf-8;q=0"

	warmRequest := httptest.NewRequest(http.MethodGet, "/library/nginx/manifests/latest", nil)
	warmRequest.Header.Set("Accept", accept)
	warm := httptest.NewRecorder()
	h.Routes().ServeHTTP(warm, warmRequest)
	if warm.Code != http.StatusOK {
		t.Fatalf("warm status = %d, want 200", warm.Code)
	}

	upstreamAvailable = false
	offlineRequest := httptest.NewRequest(http.MethodGet, "/library/nginx/manifests/latest", nil)
	offlineRequest.Header.Set("Accept", accept)
	offline := httptest.NewRecorder()
	h.Routes().ServeHTTP(offline, offlineRequest)
	if offline.Code != http.StatusServiceUnavailable {
		t.Errorf("offline status = %d, want 503", offline.Code)
	}
}

func TestContainerHandler_ManifestByTag_CachesDigestAlias(t *testing.T) {
	manifest := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`
	digest := sha256Digest([]byte(manifest))
	upstreamAvailable := true
	upstreamRequests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests++
		if !upstreamAvailable {
			http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path != "/v2/library/nginx/manifests/latest" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", digest)
		_, _ = io.WriteString(w, manifest)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	h := &ContainerHandler{proxy: proxy, registryURL: upstream.URL, proxyURL: "http://localhost:8080"}

	first := httptest.NewRecorder()
	h.Routes().ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/library/nginx/manifests/latest", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("tag status = %d, want %d; body: %s", first.Code, http.StatusOK, first.Body.String())
	}

	upstreamAvailable = false
	byDigest := httptest.NewRecorder()
	h.Routes().ServeHTTP(byDigest, httptest.NewRequest(http.MethodGet, "/library/nginx/manifests/"+digest, nil))
	if byDigest.Code != http.StatusOK {
		t.Fatalf("digest status = %d, want %d; body: %s", byDigest.Code, http.StatusOK, byDigest.Body.String())
	}
	if byDigest.Body.String() != manifest {
		t.Errorf("digest body = %q, want %q", byDigest.Body.String(), manifest)
	}
	if got := byDigest.Header().Get("Docker-Content-Digest"); got != digest {
		t.Errorf("Docker-Content-Digest = %q, want %q", got, digest)
	}
	if upstreamRequests != 1 {
		t.Errorf("upstream requests = %d, want 1", upstreamRequests)
	}
}

func TestContainerHandler_ManifestByTag_StaleHeadChecksUpstream(t *testing.T) {
	manifest := `{"schemaVersion":2}`
	oldDigest := sha256Digest([]byte(manifest))
	newDigest := "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	currentDigest := oldDigest
	upstreamRequests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests++
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", currentDigest)
		w.Header().Set("ETag", `"`+currentDigest+`"`)
		if r.Method != http.MethodHead {
			_, _ = io.WriteString(w, manifest)
		}
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	proxy.MetadataTTL = 0
	h := &ContainerHandler{proxy: proxy, registryURL: upstream.URL, proxyURL: "http://localhost:8080"}

	first := httptest.NewRecorder()
	h.Routes().ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/library/nginx/manifests/latest", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("initial status = %d, want %d", first.Code, http.StatusOK)
	}

	currentDigest = newDigest
	head := httptest.NewRecorder()
	h.Routes().ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/library/nginx/manifests/latest", nil))
	if head.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d, want %d", head.Code, http.StatusOK)
	}
	if got := head.Header().Get("Docker-Content-Digest"); got != newDigest {
		t.Errorf("Docker-Content-Digest = %q, want %q", got, newDigest)
	}
	if upstreamRequests != 2 {
		t.Errorf("upstream requests = %d, want 2", upstreamRequests)
	}
}

func TestContainerHandler_Routes_VersionCheck(t *testing.T) {
	h := NewContainerHandler(nil, "http://localhost:8080")

	handler := h.Routes()
	if handler == nil {
		t.Fatal("Routes() returned nil")
	}

	// Test /v2/ version check endpoint
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("version check: got status %d, want %d", w.Code, http.StatusOK)
	}

	if got := w.Header().Get("Docker-Distribution-Api-Version"); got != "registry/2.0" {
		t.Errorf("Docker-Distribution-Api-Version = %q, want %q", got, "registry/2.0")
	}
}
