package handler

import (
	"net/http"
	"strings"

	"github.com/git-pkgs/proxy/internal/retention"
)

const (
	conanUpstream = "https://center.conan.io"
)

// Recipe and package archives are stored under pkg:conan/{name}, with the
// version as the version and the revisions and any user/channel in the
// filename, and purl keeps conan names as they are. A package rule therefore
// covers every version, revision and user/channel of a recipe, and uses the
// default key mapping.
func init() {
	retention.Default.Register(retention.Spec{Key: "conan", CanonicalPackage: retention.DefaultCanonical("conan")})
}

// ConanHandler handles Conan registry protocol requests.
type ConanHandler struct {
	proxy       *Proxy
	upstreamURL string
	proxyURL    string
}

// NewConanHandler creates a new Conan protocol handler.
func NewConanHandler(proxy *Proxy, proxyURL string) *ConanHandler {
	return &ConanHandler{
		proxy:       proxy,
		upstreamURL: conanUpstream,
		proxyURL:    strings.TrimSuffix(proxyURL, "/"),
	}
}

// NewConanHandlerWithUpstream creates a Conan handler with a custom upstream.
func NewConanHandlerWithUpstream(proxy *Proxy, proxyURL, upstreamURL string) *ConanHandler {
	h := NewConanHandler(proxy, proxyURL)
	h.upstreamURL = configuredUpstreamURL(upstreamURL, conanUpstream)
	return h
}

// Routes returns the HTTP handler for Conan requests.
func (h *ConanHandler) Routes() http.Handler {
	mux := http.NewServeMux()

	// Ping endpoint
	mux.HandleFunc("GET /v1/ping", h.handlePing)
	mux.HandleFunc("GET /v2/ping", h.handlePing)

	// Recipe and package file downloads, as built by the Conan 2 client
	// (ClientV2Router.recipe_file and package_file). The v1 protocol hands out
	// signed absolute URLs on the upstream host, so its downloads never pass
	// through here.
	mux.HandleFunc("GET /v2/conans/{name}/{version}/{user}/{channel}/revisions/{revision}/files/{filename}", h.handleRecipeFile)
	mux.HandleFunc("GET /v2/conans/{name}/{version}/{user}/{channel}/revisions/{revision}/packages/{pkgref}/revisions/{pkgrev}/files/{filename}", h.handlePackageFile)

	// Proxy all other endpoints (metadata, search, etc.) with caching
	mux.HandleFunc("GET /", h.proxyCached)

	return mux
}

// handlePing responds to Conan ping requests.
func (h *ConanHandler) handlePing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Conan-Server-Capabilities", "revisions")
	w.WriteHeader(http.StatusOK)
}

// handleRecipeFile serves a recipe file, fetching and caching from upstream if needed.
func (h *ConanHandler) handleRecipeFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	version := r.PathValue("version")
	user := r.PathValue("user")
	channel := r.PathValue("channel")
	revision := r.PathValue("revision")
	filename := r.PathValue("filename")

	// Only the archives are artifacts; the rest goes through the metadata cache.
	if !h.shouldCacheFile(filename) {
		h.proxyCached(w, r)
		return
	}

	h.proxy.Logger.Info("conan recipe download",
		"name", name, "version", version, "user", user, "channel", channel, "filename", filename)

	upstreamURL := h.upstreamURL + r.URL.Path
	storageFilename := conanStorageFilename(user, channel, revision+"_"+filename)

	result, err := h.proxy.GetOrFetchArtifactFromURL(r.Context(), "conan", name, version, storageFilename, upstreamURL)
	if err != nil {
		h.proxy.serveArtifactError(w, err, "failed to fetch file")
		return
	}

	ServeArtifactRequest(w, r, result)
}

// handlePackageFile serves a package file, fetching and caching from upstream if needed.
func (h *ConanHandler) handlePackageFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	version := r.PathValue("version")
	user := r.PathValue("user")
	channel := r.PathValue("channel")
	revision := r.PathValue("revision")
	pkgref := r.PathValue("pkgref")
	pkgrev := r.PathValue("pkgrev")
	filename := r.PathValue("filename")

	// Only the archives are artifacts; the rest goes through the metadata cache.
	if !h.shouldCacheFile(filename) {
		h.proxyCached(w, r)
		return
	}

	h.proxy.Logger.Info("conan package download",
		"name", name, "version", version, "pkgref", pkgref, "filename", filename)

	upstreamURL := h.upstreamURL + r.URL.Path
	storageFilename := conanStorageFilename(user, channel, revision+"_"+pkgref+"_"+pkgrev+"_"+filename)

	result, err := h.proxy.GetOrFetchArtifactFromURL(r.Context(), "conan", name, version, storageFilename, upstreamURL)
	if err != nil {
		h.proxy.serveArtifactError(w, err, "failed to fetch file")
		return
	}

	ServeArtifactRequest(w, r, result)
}

// conanStorageFilename names a cached archive. Archives are stored under
// pkg:conan/{name}@{version}, so the file name carries the revisions and, when
// set, user and channel, to keep every recipe and binary apart.
func conanStorageFilename(user, channel, file string) string {
	if user == "_" && channel == "_" {
		return file
	}
	return user + "@" + channel + "_" + file
}

// shouldCacheFile returns true if the file should be cached.
func (h *ConanHandler) shouldCacheFile(filename string) bool {
	// Cache the large archive files. Since Conan 2.25 they can also be xz or
	// zstd (core.upload:compression_format).
	cacheFiles := []string{
		"conan_sources.tgz", "conan_sources.txz", "conan_sources.tzst",
		"conan_export.tgz", "conan_export.txz", "conan_export.tzst",
		"conan_package.tgz", "conan_package.txz", "conan_package.tzst",
	}

	for _, f := range cacheFiles {
		if filename == f {
			return true
		}
	}
	return false
}

// proxyCached forwards a request with metadata caching.
func (h *ConanHandler) proxyCached(w http.ResponseWriter, r *http.Request) {
	cacheKey := strings.TrimPrefix(r.URL.Path, "/")
	cacheKey = strings.ReplaceAll(cacheKey, "/", "_")
	if r.URL.RawQuery != "" {
		cacheKey += "_" + r.URL.RawQuery
	}
	upstreamURL := h.upstreamURL + r.URL.Path
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}
	h.proxy.ProxyCached(w, r, upstreamURL, "conan", cacheKey, "*/*")
}
