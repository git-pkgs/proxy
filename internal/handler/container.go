package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

const (
	dockerHubRegistry     = "https://registry-1.docker.io"
	blobMatchCount        = 3 // full match + name + digest
	manifestMatchCount    = 3 // full match + name + reference
	tagsListMatchCount    = 2 // full match + name
	registrySelectorParts = 3 // upstream + name + repository

	// namespaceQueryParam is the query parameter containerd appends to mirror
	// requests to name the registry the image reference points at.
	namespaceQueryParam = "ns"
	// defaultNamespaceRoute marks namespace hosts served by the default registry.
	defaultNamespaceRoute = ""
)

// dockerHubNamespaces are the registry URLs clients use for Docker Hub.
var dockerHubNamespaces = []string{"https://docker.io", "https://index.docker.io", "https://registry-1.docker.io"} //nolint:gochecknoglobals // fixed alias list

// schemeDefaultPorts are the ports an image reference may leave out.
var schemeDefaultPorts = map[string]string{"https": "443", "http": "80"} //nolint:gochecknoglobals // fixed table

// dockerHubNamespaceKeys are the ns lookup keys of the Docker Hub aliases.
var dockerHubNamespaceKeys = dockerHubKeys() //nolint:gochecknoglobals // fixed table

func dockerHubKeys() []string {
	var keys []string
	for _, registryURL := range dockerHubNamespaces {
		hostKeys, _ := namespaceKeysForURL(registryURL)
		keys = append(keys, hostKeys...)
	}
	return keys
}

func isDockerHubKey(key string) bool {
	return slices.Contains(dockerHubNamespaceKeys, key)
}

// ContainerHandler handles OCI/Docker container registry protocol requests.
// It implements the OCI Distribution Spec for pulling images.
// Reference: https://github.com/opencontainers/distribution-spec/blob/main/spec.md
type ContainerHandler struct {
	proxy           *Proxy
	registryURL     string
	proxyURL        string
	namedRegistries map[string]string
	registries      []containerRegistry
	// namespaces maps normalized registry hosts from containerd's ns query
	// parameter to a named upstream, or to defaultNamespaceRoute.
	namespaces map[string]string
}

type containerRegistry struct {
	repositoryPrefix string
	registryURL      string
}

// NewContainerHandler creates a new container registry protocol handler.
// Named registries are selected with the repository prefix
// upstream/{name}/, leaving unprefixed requests compatible with the Docker Hub
// mirror behavior.
func NewContainerHandler(proxy *Proxy, proxyURL string, namedRegistries ...map[string]string) *ContainerHandler {
	return newContainerHandler(proxy, proxyURL, dockerHubRegistry, namedRegistries...)
}

// NewContainerHandlerWithRegistry creates a container handler with a custom
// default registry and optional named registries.
func NewContainerHandlerWithRegistry(
	proxy *Proxy,
	proxyURL, registryURL string,
	namedRegistries ...map[string]string,
) *ContainerHandler {
	return newContainerHandler(proxy, proxyURL, configuredUpstreamURL(registryURL, dockerHubRegistry), namedRegistries...)
}

func newContainerHandler(
	proxy *Proxy,
	proxyURL, registryURL string,
	namedRegistries ...map[string]string,
) *ContainerHandler {
	h := &ContainerHandler{
		proxy:       proxy,
		registryURL: registryURL,
		proxyURL:    strings.TrimSuffix(proxyURL, "/"),
	}
	if len(namedRegistries) > 0 {
		h.namedRegistries = make(map[string]string, len(namedRegistries[0]))
		for name, registryURL := range namedRegistries[0] {
			h.namedRegistries[name] = strings.TrimSuffix(registryURL, "/")
		}
	}
	// The index needs the final default registry URL, so build it last.
	h.buildNamespaceIndex()
	return h
}

// buildNamespaceIndex maps the registry hosts containerd may send in the ns
// query parameter to the configured routes. The index is closed-world: a host
// is only ever looked up, never dialed. Docker Hub aliases and the default
// registry's host select the default route; hosts of upstream.oci entries
// select their named upstream. Registry URLs with a path are skipped because
// ns names the registry at the root of that host, not a repository mounted
// below it. On collisions the default route wins, then the alphabetically
// first upstream name.
func (h *ContainerHandler) buildNamespaceIndex() {
	h.namespaces = make(map[string]string)
	for _, registryURL := range dockerHubNamespaces {
		h.indexNamespace(defaultNamespaceRoute, registryURL)
	}
	if !h.indexNamespace(defaultNamespaceRoute, h.registryURL) {
		h.warn("host of the default OCI registry is not indexed for ns lookups: URL has a path; Docker Hub aliases still select it",
			"url", redactedURL(h.registryURL))
	}
	for _, name := range slices.Sorted(maps.Keys(h.namedRegistries)) {
		if !h.indexNamespace(name, h.namedRegistries[name]) {
			h.warn("OCI upstream is not reachable through the ns query parameter: URL has a path",
				"upstream", name, "url", redactedURL(h.namedRegistries[name]))
		}
	}
}

// indexNamespace maps the ns lookup keys of registryURL to route. Keys that
// another route already owns stay with that route and are reported once. It
// reports false when the URL is not a bare registry root.
func (h *ContainerHandler) indexNamespace(route, registryURL string) bool {
	keys, ok := namespaceKeysForURL(registryURL)
	if !ok {
		return false
	}
	var taken []string
	for _, key := range keys {
		owner, exists := h.namespaces[key]
		switch {
		case !exists:
			h.namespaces[key] = route
		case owner != route:
			if owner == defaultNamespaceRoute {
				owner = "default registry"
			}
			taken = append(taken, key+"="+owner)
		}
	}
	if len(taken) > 0 {
		h.warn("OCI upstream shares a registry host with another route; unprefixed ns requests for it go to the other route",
			"upstream", route, "hosts", strings.Join(taken, ", "))
	}
	return true
}

// namespaceKeysForURL returns the ns lookup keys of a registry URL. The
// scheme-default port is optional in image references, so such a URL is
// indexed both without and with the port. Any other port is kept as is, which
// keeps https://host:80 and https://host apart. It reports false for URLs
// that are not a bare registry root; namespaceKeysForHost serves those.
func namespaceKeysForURL(registryURL string) ([]string, bool) {
	parsed, err := url.Parse(registryURL)
	if err != nil || parsed.Host == "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, false
	}
	return namespaceKeysForHost(parsed), true
}

// namespaceKeysForHost returns the lookup keys of a URL's host, ignoring its
// path.
func namespaceKeysForHost(parsed *url.URL) []string {
	host, port := splitNamespaceHost(parsed.Host)
	defaultPort := schemeDefaultPorts[strings.ToLower(parsed.Scheme)]
	switch {
	case port != "" && port != defaultPort:
		return []string{namespaceKey(host, port)}
	case defaultPort == "":
		return []string{namespaceKey(host, "")}
	default:
		return []string{namespaceKey(host, ""), namespaceKey(host, defaultPort)}
	}
}

// namespaceKeyForRequest normalizes the ns value of a request. containerd
// sends the registry host of the image reference, with or without a port, so
// the value is matched as sent apart from case and IPv6 bracket form.
func namespaceKeyForRequest(namespace string) string {
	host, port := splitNamespaceHost(namespace)
	return namespaceKey(host, port)
}

// splitNamespaceHost splits host[:port], accepting bracketed and bare IPv6
// hosts without a port.
func splitNamespaceHost(hostport string) (host, port string) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return strings.TrimSuffix(strings.TrimPrefix(hostport, "["), "]"), ""
	}
	return host, port
}

// namespaceKey builds a lookup key from a host and an optional port: the host
// lowercased, IPv6 literals in brackets.
func namespaceKey(host, port string) string {
	host = strings.ToLower(host)
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port == "" {
		return host
	}
	return host + ":" + port
}

// redactedURL returns a registry URL for logging with any password masked.
func redactedURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "<invalid url>"
	}
	return parsed.Redacted()
}

func (h *ContainerHandler) warn(msg string, args ...any) {
	if h.proxy != nil && h.proxy.Logger != nil {
		h.proxy.Logger.Warn(msg, args...)
	}
}

// RegisterRegistry routes a repository and its descendants to a specific OCI
// registry. The longest matching repository prefix wins.
func (h *ContainerHandler) RegisterRegistry(repositoryPrefix, registryURL string) {
	h.registries = append(h.registries, containerRegistry{
		repositoryPrefix: strings.Trim(repositoryPrefix, "/"),
		registryURL:      strings.TrimSuffix(registryURL, "/"),
	})
}

// BlockRegistry prevents a repository and its descendants from falling back to
// the default OCI registry. A more specific registered repository still wins.
func (h *ContainerHandler) BlockRegistry(repositoryPrefix string) {
	h.RegisterRegistry(repositoryPrefix, "")
}

func (h *ContainerHandler) registryURLFor(name string) string {
	registryURL := h.registryURL
	matchLength := 0
	for _, registry := range h.registries {
		if name != registry.repositoryPrefix && !strings.HasPrefix(name, registry.repositoryPrefix+"/") {
			continue
		}
		if len(registry.repositoryPrefix) > matchLength {
			registryURL = registry.registryURL
			matchLength = len(registry.repositoryPrefix)
		}
	}
	return registryURL
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

	registryURL, upstreamName, cacheName, ok := h.registryForRequest(r, name)
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
		if cached.Artifact.MediaType == "" {
			cached.Artifact.MediaType = "application/octet-stream"
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
	result, err := h.proxy.GetOrFetchArtifactFromURLWithDigest(
		r.Context(),
		"oci",
		cacheName,
		digest, // use digest as version
		filename,
		fmt.Sprintf("%s/v2/%s/blobs/%s", registryURL, upstreamName, digest),
		digest,
	)

	if err != nil {
		if errors.Is(err, ErrUpstreamNotFound) {
			h.containerError(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown to registry")
			return
		}
		if errors.Is(err, ErrArtifactBlocked) || errors.Is(err, ErrVersionDenied) {
			h.containerError(w, http.StatusForbidden, "DENIED", err.Error())
			return
		}
		if errors.Is(err, ErrArtifactDigestMismatch) {
			h.proxy.Logger.Error("upstream blob failed digest verification", "error", err)
			h.containerError(w, http.StatusBadGateway, "DIGEST_INVALID", "blob digest verification failed")
			return
		}
		h.proxy.Logger.Error("failed to fetch blob", "error", err)
		h.containerError(w, http.StatusBadGateway, "INTERNAL_ERROR", "failed to fetch blob")
		return
	}

	w.Header().Set("Docker-Content-Digest", digest)
	if result.Artifact.MediaType == "" {
		result.Artifact.MediaType = "application/octet-stream"
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

	registryURL, upstreamName, _, ok := h.registryForRequest(r, name)
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

	registryURL, upstreamName, _, ok := h.registryForRequest(r, name)
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

	h.proxy.relayResponse(w, r, resp, func(dst, src http.Header) {
		for _, header := range []string{headerContentType, headerContentLength, "Docker-Content-Digest", headerETag, headerLastModified} {
			if v := src.Get(header); v != "" {
				dst.Set(header, v)
			}
		}
		if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices && dst.Get("Docker-Content-Digest") == "" {
			dst.Set("Docker-Content-Digest", digest)
		}
	})
}

// registryForRequest resolves the repository name of a request. containerd
// mirror requests name the target registry in the ns query parameter; without
// it the name is routed by registryForName.
func (h *ContainerHandler) registryForRequest(r *http.Request, name string) (registryURL, upstreamName, cacheName string, ok bool) {
	namespaces := r.URL.Query()[namespaceQueryParam]
	switch {
	case len(namespaces) == 0 || (len(namespaces) == 1 && namespaces[0] == ""):
		return h.registryForName(name)
	case len(namespaces) > 1:
		return "", "", "", false
	}
	return h.registryForNamespace(namespaces[0], name)
}

// registryForNamespace resolves a repository name verbatim against the registry
// named by ns. Cache names match the unprefixed and upstream/{name}/ routes,
// so all routes to one registry share blobs.
//
// Per-registry containerd mirrors with override_path address the reserved
// upstream/{name}/ prefix and still send ns. Such requests are served like
// the prefix route without ns unless ns contradicts the prefix, see
// namespaceNamesPrefixUpstream.
func (h *ContainerHandler) registryForNamespace(namespace, name string) (registryURL, upstreamName, cacheName string, ok bool) {
	if strings.HasPrefix(name, "upstream/") {
		if !h.namespaceNamesPrefixUpstream(namespace, name) {
			return "", "", "", false
		}
		return h.registryForName(name)
	}
	route, ok := h.namespaces[namespaceKeyForRequest(namespace)]
	if !ok {
		return "", "", "", false
	}
	if route == defaultNamespaceRoute {
		// ns explicitly names the default registry, so repository prefix
		// routes such as Homebrew's do not apply.
		if h.registryURL == "" {
			return "", "", "", false
		}
		return h.registryURL, name, name, true
	}
	registryURL = h.namedRegistries[route]
	if registryURL == "" {
		return "", "", "", false
	}
	return registryURL, name, "upstream/" + route + "/" + name, true
}

// namespaceNamesPrefixUpstream decides whether an upstream/{name}/ request
// may carry the given ns. The prefix already picks the upstream and the
// cache entries, so ns cannot change where content comes from; the check
// only refuses an ns that contradicts the prefix, meaning a host this proxy
// knows that belongs to another route. It passes for the upstream's own
// host (the URL's path is irrelevant), for a host this proxy does not know,
// which is a per-registry mirror entry for a registry the upstream mirrors,
// say an Artifactory remote for ghcr.io, and always for Docker Hub: its
// repository names have two path components, so docker.io/upstream/... is
// never a real image and a Docker Hub mirror behind any prefix stays
// reachable.
func (h *ContainerHandler) namespaceNamesPrefixUpstream(namespace, name string) bool {
	rest, _ := strings.CutPrefix(name, "upstream/")
	upstream, _, _ := strings.Cut(rest, "/")
	parsed, err := url.Parse(h.namedRegistries[upstream])
	if err != nil || parsed.Host == "" {
		return false
	}
	key := namespaceKeyForRequest(namespace)
	if isDockerHubKey(key) || slices.Contains(namespaceKeysForHost(parsed), key) {
		return true
	}
	_, known := h.namespaces[key]
	return !known
}

// registryForName resolves a client-visible OCI repository name to an upstream
// registry and its repository name. Named upstreams use upstream/{name}/ as a
// reserved prefix. Other names are matched against registered repository
// prefixes, falling back to Docker Hub when no prefix matches.
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
	registryURL = h.registryURLFor(name)
	if registryURL == "" {
		return "", "", "", false
	}
	return registryURL, name, name, true
}

// containerError writes an OCI-compliant error response.
func (h *ContainerHandler) containerError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set(headerContentType, "application/json")
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
