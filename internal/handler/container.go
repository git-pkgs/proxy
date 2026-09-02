package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

const (
	dockerHubRegistry     = "https://registry-1.docker.io"
	blobMatchCount        = 3 // full match + name + digest
	manifestMatchCount    = 3 // full match + name + reference
	tagsListMatchCount    = 2 // full match + name
	registrySelectorParts = 3 // upstream + name + repository
)

// ContainerHandler handles OCI/Docker container registry protocol requests.
// It implements the OCI Distribution Spec for pulling images.
// Reference: https://github.com/opencontainers/distribution-spec/blob/main/spec.md
type ContainerHandler struct {
	proxy           *Proxy
	registryURL     string
	proxyURL        string
	namedRegistries map[string]string
}

// NewContainerHandler creates a new container registry protocol handler.
// Named registries are selected with the repository prefix
// upstream/{name}/, leaving unprefixed requests compatible with the Docker Hub
// mirror behavior.
func NewContainerHandler(proxy *Proxy, proxyURL string, namedRegistries ...map[string]string) *ContainerHandler {
	h := &ContainerHandler{
		proxy:       proxy,
		registryURL: dockerHubRegistry,
		proxyURL:    strings.TrimSuffix(proxyURL, "/"),
	}
	if len(namedRegistries) > 0 {
		h.namedRegistries = make(map[string]string, len(namedRegistries[0]))
		for name, registryURL := range namedRegistries[0] {
			h.namedRegistries[name] = strings.TrimSuffix(registryURL, "/")
		}
	}
	return h
}

// NewContainerHandlerWithRegistry creates a container handler with a custom
// default registry and optional named registries.
func NewContainerHandlerWithRegistry(
	proxy *Proxy,
	proxyURL, registryURL string,
	namedRegistries ...map[string]string,
) *ContainerHandler {
	h := NewContainerHandler(proxy, proxyURL, namedRegistries...)
	h.registryURL = configuredUpstreamURL(registryURL, dockerHubRegistry)
	return h
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

	registryURL, upstreamName, cacheName, ok := h.registryForName(name)
	if !ok {
		h.containerError(w, http.StatusNotFound, "NAME_UNKNOWN", "unknown upstream registry")
		return
	}

	h.proxy.Logger.Info("container blob request", "name", upstreamName, "digest", digest)

	filename := digest
	cached, err := h.proxy.GetCachedArtifact(r.Context(), "oci", cacheName, digest, filename)
	if err != nil {
		h.proxy.Logger.Error("failed to check blob cache", "error", err)
		h.containerError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to check blob cache")
		return
	}
	if cached != nil {
		w.Header().Set("Docker-Content-Digest", digest)
		if cached.ContentType != "" {
			w.Header().Set("Content-Type", cached.ContentType)
		} else {
			w.Header().Set("Content-Type", "application/octet-stream")
		}
		serveArtifact(w, r.Method, cached)
		return
	}

	// For HEAD requests, just proxy to upstream
	if r.Method == http.MethodHead {
		h.proxyBlobHead(w, r, registryURL, upstreamName, digest)
		return
	}

	// Try to get from cache, or fetch from the authentication-aware upstream client.
	result, err := h.proxy.GetOrFetchArtifactFromURL(
		r.Context(),
		"oci",
		cacheName,
		digest, // use digest as version
		filename,
		fmt.Sprintf("%s/v2/%s/blobs/%s", registryURL, upstreamName, digest),
	)

	if err != nil {
		if errors.Is(err, ErrUpstreamNotFound) {
			h.containerError(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown to registry")
			return
		}
		h.proxy.Logger.Error("failed to fetch blob", "error", err)
		h.containerError(w, http.StatusBadGateway, "INTERNAL_ERROR", "failed to fetch blob")
		return
	}

	w.Header().Set("Docker-Content-Digest", digest)
	if result.ContentType != "" {
		w.Header().Set("Content-Type", result.ContentType)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	ServeArtifact(w, result)
}

// handleManifest serves immutable manifests from cache and revalidates mutable tags.
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

	registryURL, upstreamName, _, ok := h.registryForName(name)
	if !ok {
		h.containerError(w, http.StatusNotFound, "NAME_UNKNOWN", "unknown upstream registry")
		return
	}

	h.proxy.Logger.Info("container manifest request", "name", upstreamName, "reference", reference)
	h.serveManifest(w, r, registryURL, upstreamName, reference)
}

// handleTagsList caches tag list responses for offline OCI pulls.
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

	registryURL, upstreamName, _, ok := h.registryForName(name)
	if !ok {
		h.containerError(w, http.StatusNotFound, "NAME_UNKNOWN", "unknown upstream registry")
		return
	}

	h.serveTagsList(w, r, registryURL, upstreamName)
}

// proxyBlobHead handles HEAD requests for blobs.
func (h *ContainerHandler) proxyBlobHead(w http.ResponseWriter, r *http.Request, registryURL, name, digest string) {
	upstreamURL := fmt.Sprintf("%s/v2/%s/blobs/%s", registryURL, name, digest)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodHead, upstreamURL, nil)
	if err != nil {
		h.containerError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to create request")
		return
	}

	resp, err := h.proxy.HTTPClient.Do(req)
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

// registryForName resolves a client-visible OCI repository name to an upstream
// registry and its repository name. Named upstreams use upstream/{name}/ as a
// reserved prefix; all other names continue to target Docker Hub.
func (h *ContainerHandler) registryForName(name string) (registryURL, upstreamName, cacheName string, ok bool) {
	parts := strings.SplitN(name, "/", registrySelectorParts)
	if len(parts) >= 2 && parts[0] == "upstream" {
		if len(parts) != registrySelectorParts || parts[2] == "" {
			return "", "", "", false
		}
		registryURL, ok = h.namedRegistries[parts[1]]
		if !ok || registryURL == "" {
			return "", "", "", false
		}
		return registryURL, parts[2], name, true
	}
	return h.registryURL, name, name, true
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
	if len(matches) != blobMatchCount {
		return "", ""
	}
	return matches[1], matches[2]
}

// manifestPathPattern matches manifest paths: {name}/manifests/{reference}
var manifestPathPattern = regexp.MustCompile(`^(.+)/manifests/(.+)$`)

// parseManifestPath extracts repository name and reference from a manifest path.
func (h *ContainerHandler) parseManifestPath(path string) (name, reference string) {
	matches := manifestPathPattern.FindStringSubmatch(path)
	if len(matches) != manifestMatchCount {
		return "", ""
	}
	return matches[1], matches[2]
}

// tagsListPathPattern matches tags list paths: {name}/tags/list
var tagsListPathPattern = regexp.MustCompile(`^(.+)/tags/list$`)

// parseTagsListPath extracts repository name from a tags list path.
func (h *ContainerHandler) parseTagsListPath(path string) string {
	matches := tagsListPathPattern.FindStringSubmatch(path)
	if len(matches) != tagsListMatchCount {
		return ""
	}
	return matches[1]
}
