package handler

import (
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	conanUpstream = "https://center.conan.io"
)

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

// Routes returns the HTTP handler for Conan requests.
func (h *ConanHandler) Routes() http.Handler {
	mux := http.NewServeMux()

	// Ping endpoint
	mux.HandleFunc("GET /v1/ping", h.handlePing)
	mux.HandleFunc("GET /v2/ping", h.handlePing)

	// Recipe file downloads (cache these)
	mux.HandleFunc("GET /v1/files/{name}/{version}/{user}/{channel}/{revision}/recipe/{filename}", h.handleRecipeFile)
	mux.HandleFunc("GET /v2/files/{name}/{version}/{user}/{channel}/{revision}/recipe/{filename}", h.handleRecipeFile)

	// Package file downloads (cache these)
	mux.HandleFunc("GET /v1/files/{name}/{version}/{user}/{channel}/{revision}/package/{pkgref}/{pkgrev}/{filename}", h.handlePackageFile)
	mux.HandleFunc("GET /v2/files/{name}/{version}/{user}/{channel}/{revision}/package/{pkgref}/{pkgrev}/{filename}", h.handlePackageFile)

	// Proxy all other endpoints (metadata, search, etc.)
	mux.HandleFunc("GET /", h.proxyUpstream)

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

	// Only cache specific files
	if !h.shouldCacheFile(filename) {
		h.proxyUpstream(w, r)
		return
	}

	// Conan package name format: name/version@user/channel
	packageName := fmt.Sprintf("%s/%s@%s/%s", name, version, user, channel)

	h.proxy.Logger.Info("conan recipe download",
		"name", name, "version", version, "user", user, "channel", channel, "filename", filename)

	upstreamURL := h.upstreamURL + r.URL.Path

	// Use revision as part of version for storage
	storageVersion := fmt.Sprintf("%s_%s", version, revision)
	storageFilename := fmt.Sprintf("recipe_%s", filename)

	result, err := h.proxy.GetOrFetchArtifactFromURL(r.Context(), "conan", packageName, storageVersion, storageFilename, upstreamURL)
	if err != nil {
		h.proxy.Logger.Error("failed to get artifact", "error", err)
		http.Error(w, "failed to fetch file", http.StatusBadGateway)
		return
	}

	ServeArtifact(w, result)
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

	// Only cache specific files
	if !h.shouldCacheFile(filename) {
		h.proxyUpstream(w, r)
		return
	}

	packageName := fmt.Sprintf("%s/%s@%s/%s", name, version, user, channel)

	h.proxy.Logger.Info("conan package download",
		"name", name, "version", version, "pkgref", pkgref, "filename", filename)

	upstreamURL := h.upstreamURL + r.URL.Path

	// Use revision and package ref as part of version for storage
	storageVersion := fmt.Sprintf("%s_%s_%s_%s", version, revision, pkgref, pkgrev)
	storageFilename := fmt.Sprintf("package_%s", filename)

	result, err := h.proxy.GetOrFetchArtifactFromURL(r.Context(), "conan", packageName, storageVersion, storageFilename, upstreamURL)
	if err != nil {
		h.proxy.Logger.Error("failed to get artifact", "error", err)
		http.Error(w, "failed to fetch file", http.StatusBadGateway)
		return
	}

	ServeArtifact(w, result)
}

// shouldCacheFile returns true if the file should be cached.
func (h *ConanHandler) shouldCacheFile(filename string) bool {
	// Cache the large archive files
	cacheFiles := []string{
		"conan_sources.tgz",
		"conan_export.tgz",
		"conan_package.tgz",
	}

	for _, f := range cacheFiles {
		if filename == f {
			return true
		}
	}
	return false
}

// proxyUpstream forwards a request to conan center without caching.
func (h *ConanHandler) proxyUpstream(w http.ResponseWriter, r *http.Request) {
	upstreamURL := h.upstreamURL + r.URL.Path
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	h.proxy.Logger.Debug("proxying to upstream", "url", upstreamURL)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
	}

	// Copy authorization header for authenticated requests
	if auth := r.Header.Get("Authorization"); auth != "" {
		req.Header.Set("Authorization", auth)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.proxy.Logger.Error("upstream request failed", "error", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Copy response headers
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}

	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
