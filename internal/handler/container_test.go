package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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

func TestContainerHandler_NamedOCIRegistryServesHelmArtifacts(t *testing.T) {
	digest := "sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abcd"
	manifest := `{"schemaVersion":2,"config":{"mediaType":"application/vnd.cncf.helm.config.v1+json"},"layers":[{"mediaType":"application/vnd.cncf.helm.chart.content.v1.tar+gzip","digest":"` + digest + `"}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/owner/demo/manifests/1.0.0":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", digest)
			_, _ = io.WriteString(w, manifest)
		case "/v2/owner/demo/blobs/" + digest:
			w.Header().Set("Content-Type", "application/vnd.cncf.helm.chart.content.v1.tar+gzip")
			_, _ = io.WriteString(w, "chart archive")
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

func TestContainerHandler_BlobDownload_DiscoversBearerChallenge(t *testing.T) {
	digest := "sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abcd"
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
			_, _ = io.WriteString(w, "upstream blob")
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
		if got := w.Body.String(); got != "upstream blob" {
			t.Errorf("body = %q, want %q", got, "upstream blob")
		}
	}

	if tokenRequests != 1 {
		t.Errorf("token requests = %d, want 1", tokenRequests)
	}
	if registryRequests != 2 {
		t.Errorf("registry requests = %d, want 2", registryRequests)
	}
}

func TestContainerHandler_CachedImagePullSurvivesRegistryAndTokenOutages(t *testing.T) {
	digest := "sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abcd"
	manifest := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`
	blob := "cached image blob"
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
			w.Header().Set("Docker-Content-Digest", digest)
			_, _ = io.WriteString(w, manifest)
		case "/v2/library/nginx/blobs/" + digest:
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
		{path: "/library/nginx/blobs/" + digest, body: blob},
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
		name string
		path string
		body string
	}{
		{name: "tag manifest", path: "/library/nginx/manifests/latest", body: manifest},
		{name: "digest manifest", path: "/library/nginx/manifests/" + digest, body: manifest},
		{name: "blob", path: "/library/nginx/blobs/" + digest, body: blob},
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
			if got := response.Header().Get("Docker-Content-Digest"); got != digest {
				t.Errorf("Docker-Content-Digest = %q, want %q", got, digest)
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
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	manifest := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`
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

func TestContainerHandler_ManifestByTag_UsesStaleCacheOnUpstreamFailure(t *testing.T) {
	digest := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	manifest := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json"}`
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
	digest := "sha256:abababababababababababababababababababababababababababababababab"
	manifest := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json"}`
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

func TestContainerHandler_ManifestVariantCacheHonorsSpecificAcceptExclusions(t *testing.T) {
	digest := "sha256:cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
	upstreamAvailable := true
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !upstreamAvailable {
			http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
		w.Header().Set("Docker-Content-Digest", digest)
		_, _ = io.WriteString(w, `{"schemaVersion":2}`)
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

func TestContainerHandler_ManifestByTag_CachesDigestAlias(t *testing.T) {
	digest := "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	manifest := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`
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
	oldDigest := "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	newDigest := "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	currentDigest := oldDigest
	upstreamRequests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests++
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", currentDigest)
		w.Header().Set("ETag", `"`+currentDigest+`"`)
		if r.Method != http.MethodHead {
			_, _ = io.WriteString(w, `{"schemaVersion":2}`)
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
