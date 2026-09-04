package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"
)

const (
	genericEcosystem = "generic"
	// genericAcceptAny is always sent for metadata so every client shares the
	// same cached representation, regardless of its Accept header.
	genericAcceptAny = "*/*"
	// githubReleaseAssetMatchCount is the full match plus owner, repository,
	// tag and asset filename.
	githubReleaseAssetMatchCount = 5
)

// githubReleaseAssetPattern matches the path of a GitHub release asset
// download, {owner}/{repo}/releases/download/{tag}/{asset}. A tag pins the
// asset to one release, so these downloads are cached in the artifact cache
// and served without revalidation once fetched.
var githubReleaseAssetPattern = regexp.MustCompile(`^([^/]+)/([^/]+)/releases/download/([^/]+)/([^/]+)$`)

// GenericHandler proxies plain HTTP downloads from configured upstream base
// URLs. Each configured upstream is mounted at /generic/{name}/ and the
// remaining request path (and query string) is appended to the upstream URL.
//
// Only configured upstreams are reachable, so the proxy is not an open HTTP
// proxy. The handler is the caching layer behind tools that download from
// fixed URL shapes, such as mise's aqua backend fetching GitHub release
// assets, and is pointed at by URL-rewriting settings on the client.
//
// Release-asset paths ({owner}/{repo}/releases/download/{tag}/{asset}) are
// version-pinned and cached in the shared artifact cache, so they keep being
// served when the upstream is unreachable. Every other path is served through
// the metadata cache: fresh within the metadata TTL, revalidated with the
// upstream's validators after that, and served stale when the upstream fails
// or refuses the request. That covers API responses such as
// api.github.com/repos/{owner}/{repo}/releases/tags/{tag}.
type GenericHandler struct {
	proxy        *Proxy
	repositories map[string]string
}

// NewGenericHandler creates a generic HTTP download proxy handler.
func NewGenericHandler(proxy *Proxy, repositories map[string]string) *GenericHandler {
	h := &GenericHandler{
		proxy:        proxy,
		repositories: make(map[string]string, len(repositories)),
	}
	for name, upstreamURL := range repositories {
		h.repositories[name] = strings.TrimSuffix(upstreamURL, "/")
	}
	return h
}

// Routes returns the HTTP handler for generic download requests.
// Mount this at /generic on your router.
func (h *GenericHandler) Routes() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		path := strings.TrimPrefix(r.URL.Path, "/")

		if containsPathTraversal(path) {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}

		repository, rest, ok := strings.Cut(path, "/")
		upstreamURL, found := h.repositories[repository]
		if !ok || rest == "" || !found {
			http.NotFound(w, r)
			return
		}

		if asset, ok := parseGitHubReleaseAsset(rest); ok {
			h.handleReleaseAsset(w, r, repository, upstreamURL, rest, asset)
			return
		}

		h.handleMetadata(w, r, repository, upstreamURL, rest)
	})
}

// githubReleaseAsset is the identity of a version-pinned release download.
type githubReleaseAsset struct {
	owner    string
	repo     string
	tag      string
	filename string
}

// parseGitHubReleaseAsset extracts the release identity from a path shaped
// like {owner}/{repo}/releases/download/{tag}/{asset}.
func parseGitHubReleaseAsset(path string) (githubReleaseAsset, bool) {
	matches := githubReleaseAssetPattern.FindStringSubmatch(path)
	if len(matches) != githubReleaseAssetMatchCount {
		return githubReleaseAsset{}, false
	}
	return githubReleaseAsset{
		owner:    matches[1],
		repo:     matches[2],
		tag:      matches[3],
		filename: matches[4],
	}, true
}

// handleReleaseAsset fetches and caches a version-pinned release asset in the
// artifact cache. The configured upstream name is part of the cache identity
// so two upstreams serving the same path never share bytes.
func (h *GenericHandler) handleReleaseAsset(w http.ResponseWriter, r *http.Request, repository, upstreamURL, path string, asset githubReleaseAsset) {
	name := asset.owner + "/" + asset.repo
	downloadURL := upstreamURL + "/" + path
	cacheFilename := repository + "/" + asset.filename

	h.proxy.Logger.Info("generic release asset download",
		"repository", repository, "name", name, "version", asset.tag, "filename", asset.filename)

	result, err := h.proxy.GetOrFetchArtifactFromURL(
		r.Context(), genericEcosystem, name, asset.tag, cacheFilename, downloadURL)
	if err != nil {
		h.proxy.serveArtifactError(w, err, "failed to fetch release asset")
		return
	}

	if result.Artifact.MediaType == "" {
		result.Artifact.MediaType = "application/octet-stream"
	}
	serveArtifact(w, r.Method, result)
}

// handleMetadata serves any other path through the metadata cache. The query
// string is forwarded and is part of the cache identity. A fixed Accept header
// keeps all clients on one cached representation.
func (h *GenericHandler) handleMetadata(w http.ResponseWriter, r *http.Request, repository, upstreamURL, path string) {
	target := upstreamURL + "/" + path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	h.proxy.ProxyCached(w, r, target, genericEcosystem,
		h.metadataCacheKey(repository, upstreamURL, path, r.URL.RawQuery), genericAcceptAny)
}

// metadataCacheKey derives the metadata cache key from the upstream name, its
// URL, the request path and query. Hashing the identity keeps distinct
// upstreams from sharing entries and drops cached entries when an upstream is
// repointed, mirroring APKHandler.metadataCacheKey.
func (h *GenericHandler) metadataCacheKey(repository, upstreamURL, path, query string) string {
	identity := repository + "\x00" + upstreamURL + "\x00" + path + "\x00" + query
	digest := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(digest[:])
}
