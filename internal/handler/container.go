package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

const (
	dockerHubRegistry = "https://registry-1.docker.io"
	dockerHubAuth     = "https://auth.docker.io"
)

// ContainerHandler handles OCI/Docker container registry protocol requests.
// It implements the OCI Distribution Spec for pulling images.
// Reference: https://github.com/opencontainers/distribution-spec/blob/main/spec.md
type ContainerHandler struct {
	proxy       *Proxy
	registryURL string
	authURL     string
	proxyURL    string
}

// NewContainerHandler creates a new container registry protocol handler.
func NewContainerHandler(proxy *Proxy, proxyURL string) *ContainerHandler {
	return &ContainerHandler{
		proxy:       proxy,
		registryURL: dockerHubRegistry,
		authURL:     dockerHubAuth,
		proxyURL:    strings.TrimSuffix(proxyURL, "/"),
	}
}

// Routes returns the HTTP handler for container registry requests.
// Mount this at /v2 on your router.
func (h *ContainerHandler) Routes() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")

		// Set standard Docker registry header on all responses
		w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")

		// Handle different endpoints
		switch {
		case path == "" || path == "/":
			// Version check: GET /v2/
			h.handleVersionCheck(w, r)
		case strings.HasSuffix(path, "/blobs/"+r.URL.Query().Get("digest")) || strings.Contains(path, "/blobs/sha256:"):
			// Blob download: GET /v2/{name}/blobs/{digest}
			h.handleBlobDownload(w, r, path)
		case strings.Contains(path, "/manifests/"):
			// Manifest: GET /v2/{name}/manifests/{reference}
			h.handleManifest(w, r, path)
		case strings.Contains(path, "/tags/list"):
			// Tags list: GET /v2/{name}/tags/list
			h.handleTagsList(w, r, path)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	})
}

// handleVersionCheck responds to the /v2/ endpoint.
// This is used by clients to verify the registry supports the v2 API.
func (h *ContainerHandler) handleVersionCheck(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// handleBlobDownload fetches and caches container layer blobs.
// Path format: {name}/blobs/{digest}
// Example: library/nginx/blobs/sha256:abc123...
func (h *ContainerHandler) handleBlobDownload(w http.ResponseWriter, r *http.Request, path string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	name, digest := h.parseBlobPath(path)
	if name == "" || digest == "" {
		h.containerError(w, http.StatusBadRequest, "BLOB_UNKNOWN", "invalid blob path")
		return
	}

	h.proxy.Logger.Info("container blob request", "name", name, "digest", digest)

	// Get auth token for upstream
	token, err := h.getAuthToken(r.Context(), name, "pull")
	if err != nil {
		h.proxy.Logger.Error("failed to get auth token", "error", err)
		h.containerError(w, http.StatusUnauthorized, "UNAUTHORIZED", "failed to authenticate")
		return
	}

	// For HEAD requests, just proxy to upstream
	if r.Method == http.MethodHead {
		h.proxyBlobHead(w, r, name, digest, token)
		return
	}

	// Try to get from cache first
	filename := digest
	result, err := h.proxy.GetOrFetchArtifactFromURL(
		r.Context(),
		"oci",
		name,
		digest, // use digest as version
		filename,
		fmt.Sprintf("%s/v2/%s/blobs/%s", h.registryURL, name, digest),
	)

	if err != nil {
		// Fetch directly with auth
		h.proxyBlobWithAuth(w, r, name, digest, token)
		return
	}

	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Type", "application/octet-stream")
	ServeArtifact(w, result)
}

// handleManifest proxies manifest requests to upstream.
// Manifests change when tags are updated, so we proxy these directly.
// Path format: {name}/manifests/{reference}
func (h *ContainerHandler) handleManifest(w http.ResponseWriter, r *http.Request, path string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	name, reference := h.parseManifestPath(path)
	if name == "" || reference == "" {
		h.containerError(w, http.StatusBadRequest, "MANIFEST_UNKNOWN", "invalid manifest path")
		return
	}

	h.proxy.Logger.Info("container manifest request", "name", name, "reference", reference)

	// Get auth token
	token, err := h.getAuthToken(r.Context(), name, "pull")
	if err != nil {
		h.proxy.Logger.Error("failed to get auth token", "error", err)
		h.containerError(w, http.StatusUnauthorized, "UNAUTHORIZED", "failed to authenticate")
		return
	}

	// Proxy to upstream
	upstreamURL := fmt.Sprintf("%s/v2/%s/manifests/%s", h.registryURL, name, reference)

	req, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, nil)
	if err != nil {
		h.containerError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to create request")
		return
	}

	req.Header.Set("Authorization", "Bearer "+token)

	// Forward Accept header for content negotiation
	if accept := r.Header.Get("Accept"); accept != "" {
		req.Header.Set("Accept", accept)
	} else {
		// Default accept headers for manifests
		req.Header.Set("Accept", strings.Join([]string{
			"application/vnd.oci.image.manifest.v1+json",
			"application/vnd.oci.image.index.v1+json",
			"application/vnd.docker.distribution.manifest.v2+json",
			"application/vnd.docker.distribution.manifest.list.v2+json",
			"application/vnd.docker.distribution.manifest.v1+prettyjws",
		}, ", "))
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.proxy.Logger.Error("failed to fetch manifest", "error", err)
		h.containerError(w, http.StatusBadGateway, "INTERNAL_ERROR", "failed to fetch from upstream")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Copy relevant headers
	for _, header := range []string{"Content-Type", "Content-Length", "Docker-Content-Digest", "ETag"} {
		if v := resp.Header.Get(header); v != "" {
			w.Header().Set(header, v)
		}
	}

	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// handleTagsList proxies tag list requests to upstream.
func (h *ContainerHandler) handleTagsList(w http.ResponseWriter, r *http.Request, path string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	name := h.parseTagsListPath(path)
	if name == "" {
		h.containerError(w, http.StatusBadRequest, "NAME_UNKNOWN", "invalid repository name")
		return
	}

	// Get auth token
	token, err := h.getAuthToken(r.Context(), name, "pull")
	if err != nil {
		h.containerError(w, http.StatusUnauthorized, "UNAUTHORIZED", "failed to authenticate")
		return
	}

	upstreamURL := fmt.Sprintf("%s/v2/%s/tags/list", h.registryURL, name)
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		h.containerError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to create request")
		return
	}

	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.containerError(w, http.StatusBadGateway, "INTERNAL_ERROR", "failed to fetch from upstream")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// getAuthToken gets a bearer token for the specified repository.
// Docker Hub requires auth even for public images.
func (h *ContainerHandler) getAuthToken(_ interface{ Done() <-chan struct{} }, repository, action string) (string, error) {
	// For Docker Hub: https://auth.docker.io/token?service=registry.docker.io&scope=repository:{repo}:pull
	authURL := fmt.Sprintf("%s/token?service=registry.docker.io&scope=repository:%s:%s",
		h.authURL, repository, action)

	req, err := http.NewRequest(http.MethodGet, authURL, nil)
	if err != nil {
		return "", err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("auth failed with status %d", resp.StatusCode)
	}

	var tokenResp struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", err
	}

	if tokenResp.Token != "" {
		return tokenResp.Token, nil
	}
	return tokenResp.AccessToken, nil
}

// proxyBlobHead handles HEAD requests for blobs.
func (h *ContainerHandler) proxyBlobHead(w http.ResponseWriter, r *http.Request, name, digest, token string) {
	upstreamURL := fmt.Sprintf("%s/v2/%s/blobs/%s", h.registryURL, name, digest)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodHead, upstreamURL, nil)
	if err != nil {
		h.containerError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to create request")
		return
	}

	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.containerError(w, http.StatusBadGateway, "INTERNAL_ERROR", "failed to fetch from upstream")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	for _, header := range []string{"Content-Type", "Content-Length", "Docker-Content-Digest"} {
		if v := resp.Header.Get(header); v != "" {
			w.Header().Set(header, v)
		}
	}

	w.WriteHeader(resp.StatusCode)
}

// proxyBlobWithAuth proxies a blob download with authentication.
func (h *ContainerHandler) proxyBlobWithAuth(w http.ResponseWriter, r *http.Request, name, digest, token string) {
	upstreamURL := fmt.Sprintf("%s/v2/%s/blobs/%s", h.registryURL, name, digest)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		h.containerError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to create request")
		return
	}

	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.containerError(w, http.StatusBadGateway, "INTERNAL_ERROR", "failed to fetch from upstream")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	for _, header := range []string{"Content-Type", "Content-Length", "Docker-Content-Digest"} {
		if v := resp.Header.Get(header); v != "" {
			w.Header().Set(header, v)
		}
	}

	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// containerError writes an OCI-compliant error response.
func (h *ContainerHandler) containerError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]string{
			{"code": code, "message": message},
		},
	})
}

// blobPathPattern matches blob paths: {name}/blobs/{digest}
var blobPathPattern = regexp.MustCompile(`^(.+)/blobs/(sha256:[a-f0-9]+)$`)

// parseBlobPath extracts repository name and digest from a blob path.
func (h *ContainerHandler) parseBlobPath(path string) (name, digest string) {
	matches := blobPathPattern.FindStringSubmatch(path)
	if len(matches) != 3 {
		return "", ""
	}
	return matches[1], matches[2]
}

// manifestPathPattern matches manifest paths: {name}/manifests/{reference}
var manifestPathPattern = regexp.MustCompile(`^(.+)/manifests/(.+)$`)

// parseManifestPath extracts repository name and reference from a manifest path.
func (h *ContainerHandler) parseManifestPath(path string) (name, reference string) {
	matches := manifestPathPattern.FindStringSubmatch(path)
	if len(matches) != 3 {
		return "", ""
	}
	return matches[1], matches[2]
}

// tagsListPathPattern matches tags list paths: {name}/tags/list
var tagsListPathPattern = regexp.MustCompile(`^(.+)/tags/list$`)

// parseTagsListPath extracts repository name from a tags list path.
func (h *ContainerHandler) parseTagsListPath(path string) string {
	matches := tagsListPathPattern.FindStringSubmatch(path)
	if len(matches) != 2 {
		return ""
	}
	return matches[1]
}
