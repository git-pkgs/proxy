package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

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
