package handler

import (
	"net/http"
	"strings"
)

const (
	hexUpstream = "https://repo.hex.pm"
)

// HexHandler handles Hex.pm registry protocol requests.
type HexHandler struct {
	proxy       *Proxy
	upstreamURL string
	proxyURL    string
}

// NewHexHandler creates a new Hex.pm protocol handler.
func NewHexHandler(proxy *Proxy, proxyURL string) *HexHandler {
	return &HexHandler{
		proxy:       proxy,
		upstreamURL: hexUpstream,
		proxyURL:    strings.TrimSuffix(proxyURL, "/"),
	}
}

// Routes returns the HTTP handler for Hex requests.
func (h *HexHandler) Routes() http.Handler {
	mux := http.NewServeMux()

	// Package tarballs (cache these)
	mux.HandleFunc("GET /tarballs/{filename}", h.handleDownload)

	// Registry resources (cached for offline)
	mux.HandleFunc("GET /names", h.proxyCached)
	mux.HandleFunc("GET /versions", h.proxyCached)
	mux.HandleFunc("GET /packages/{name}", h.proxyCached)

	// Public keys
	mux.HandleFunc("GET /public_key", h.proxyUpstream)

	return mux
}

// handleDownload serves a package tarball, fetching and caching from upstream if needed.
func (h *HexHandler) handleDownload(w http.ResponseWriter, r *http.Request) {
	filename := r.PathValue("filename")
	if filename == "" || !strings.HasSuffix(filename, ".tar") {
		http.Error(w, "invalid filename", http.StatusBadRequest)
		return
	}

	// Extract name and version from filename (e.g., "phoenix-1.7.10.tar")
	name, version := h.parseTarballFilename(filename)
	if name == "" || version == "" {
		http.Error(w, "could not parse tarball filename", http.StatusBadRequest)
		return
	}

	h.proxy.Logger.Info("hex download request",
		"name", name, "version", version, "filename", filename)

	result, err := h.proxy.GetOrFetchArtifact(r.Context(), "hex", name, version, filename)
	if err != nil {
		h.proxy.Logger.Error("failed to get artifact", "error", err)
		http.Error(w, "failed to fetch package", http.StatusBadGateway)
		return
	}

	ServeArtifact(w, result)
}

// parseTarballFilename extracts name and version from a hex tarball filename.
// e.g., "phoenix-1.7.10.tar" -> ("phoenix", "1.7.10")
func (h *HexHandler) parseTarballFilename(filename string) (name, version string) {
	base := strings.TrimSuffix(filename, ".tar")

	// Find the last hyphen followed by a version number
	for i := len(base) - 1; i >= 0; i-- {
		if base[i] == '-' && i+1 < len(base) && base[i+1] >= '0' && base[i+1] <= '9' {
			return base[:i], base[i+1:]
		}
	}
	return "", ""
}

// proxyCached forwards a request with metadata caching.
func (h *HexHandler) proxyCached(w http.ResponseWriter, r *http.Request) {
	cacheKey := strings.TrimPrefix(r.URL.Path, "/")
	h.proxy.ProxyCached(w, r, h.upstreamURL+r.URL.Path, "hex", cacheKey, "*/*")
}

// proxyUpstream forwards a request to hex.pm without caching.
func (h *HexHandler) proxyUpstream(w http.ResponseWriter, r *http.Request) {
	h.proxy.ProxyUpstream(w, r, h.upstreamURL+r.URL.Path, []string{"Accept"})
}
