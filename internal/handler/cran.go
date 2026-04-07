package handler

import (
	"net/http"
	"strings"
)

const (
	cranUpstream = "https://cloud.r-project.org"
)

// CRANHandler handles CRAN (R) registry protocol requests.
type CRANHandler struct {
	proxy       *Proxy
	upstreamURL string
	proxyURL    string
}

// NewCRANHandler creates a new CRAN protocol handler.
func NewCRANHandler(proxy *Proxy, proxyURL string) *CRANHandler {
	return &CRANHandler{
		proxy:       proxy,
		upstreamURL: cranUpstream,
		proxyURL:    strings.TrimSuffix(proxyURL, "/"),
	}
}

// Routes returns the HTTP handler for CRAN requests.
func (h *CRANHandler) Routes() http.Handler {
	mux := http.NewServeMux()

	// Package indexes
	mux.HandleFunc("GET /src/contrib/PACKAGES", h.proxyCached)
	mux.HandleFunc("GET /src/contrib/PACKAGES.gz", h.proxyCached)
	mux.HandleFunc("GET /src/contrib/PACKAGES.rds", h.proxyCached)

	// Binary package indexes
	mux.HandleFunc("GET /bin/{platform}/contrib/{rversion}/PACKAGES", h.proxyCached)
	mux.HandleFunc("GET /bin/{platform}/contrib/{rversion}/PACKAGES.gz", h.proxyCached)
	mux.HandleFunc("GET /bin/{platform}/contrib/{rversion}/PACKAGES.rds", h.proxyCached)

	// Source package downloads
	mux.HandleFunc("GET /src/contrib/{filename}", h.handleSourceDownload)
	mux.HandleFunc("GET /src/contrib/Archive/{name}/{filename}", h.handleSourceDownload)

	// Binary package downloads
	mux.HandleFunc("GET /bin/{platform}/contrib/{rversion}/{filename}", h.handleBinaryDownload)

	return mux
}

// handleSourceDownload serves a source package, fetching and caching from upstream.
func (h *CRANHandler) handleSourceDownload(w http.ResponseWriter, r *http.Request) {
	filename := r.PathValue("filename")
	archiveName := r.PathValue("name") // empty for current packages

	if !strings.HasSuffix(filename, ".tar.gz") {
		h.proxyUpstream(w, r)
		return
	}

	name, version := h.parseSourceFilename(filename)
	if name == "" {
		h.proxyUpstream(w, r)
		return
	}

	h.proxy.Logger.Info("cran source download",
		"name", name, "version", version, "archive", archiveName)

	upstreamURL := h.upstreamURL + r.URL.Path

	result, err := h.proxy.GetOrFetchArtifactFromURL(r.Context(), "cran", name, version, filename, upstreamURL)
	if err != nil {
		h.proxy.Logger.Error("failed to get artifact", "error", err)
		http.Error(w, "failed to fetch package", http.StatusBadGateway)
		return
	}

	ServeArtifact(w, result)
}

// handleBinaryDownload serves a binary package, fetching and caching from upstream.
func (h *CRANHandler) handleBinaryDownload(w http.ResponseWriter, r *http.Request) {
	platform := r.PathValue("platform")
	rversion := r.PathValue("rversion")
	filename := r.PathValue("filename")

	if !h.isBinaryPackage(filename) {
		h.proxyUpstream(w, r)
		return
	}

	name, version := h.parseBinaryFilename(filename)
	if name == "" {
		h.proxyUpstream(w, r)
		return
	}

	// Include platform and R version in stored version
	storageVersion := version + "_" + platform + "_" + rversion

	h.proxy.Logger.Info("cran binary download",
		"name", name, "version", version, "platform", platform, "rversion", rversion)

	upstreamURL := h.upstreamURL + r.URL.Path

	result, err := h.proxy.GetOrFetchArtifactFromURL(r.Context(), "cran", name, storageVersion, filename, upstreamURL)
	if err != nil {
		h.proxy.Logger.Error("failed to get artifact", "error", err)
		http.Error(w, "failed to fetch package", http.StatusBadGateway)
		return
	}

	ServeArtifact(w, result)
}

// parseSourceFilename extracts name and version from a CRAN source filename.
// Format: {name}_{version}.tar.gz
func (h *CRANHandler) parseSourceFilename(filename string) (name, version string) {
	base := strings.TrimSuffix(filename, ".tar.gz")
	idx := strings.LastIndex(base, "_")
	if idx < 0 {
		return "", ""
	}
	return base[:idx], base[idx+1:]
}

// parseBinaryFilename extracts name and version from a CRAN binary filename.
// Windows: {name}_{version}.zip
// macOS: {name}_{version}.tgz
func (h *CRANHandler) parseBinaryFilename(filename string) (name, version string) {
	base := filename
	for _, ext := range []string{".zip", ".tgz"} {
		if strings.HasSuffix(base, ext) {
			base = strings.TrimSuffix(base, ext)
			break
		}
	}

	idx := strings.LastIndex(base, "_")
	if idx < 0 {
		return "", ""
	}
	return base[:idx], base[idx+1:]
}

// isBinaryPackage returns true if the filename is a CRAN binary package.
func (h *CRANHandler) isBinaryPackage(filename string) bool {
	return strings.HasSuffix(filename, ".zip") || strings.HasSuffix(filename, ".tgz")
}

// proxyCached forwards a metadata request with caching.
func (h *CRANHandler) proxyCached(w http.ResponseWriter, r *http.Request) {
	cacheKey := strings.TrimPrefix(r.URL.Path, "/")
	cacheKey = strings.ReplaceAll(cacheKey, "/", "_")
	h.proxy.ProxyCached(w, r, h.upstreamURL+r.URL.Path, "cran", cacheKey, "*/*")
}

// proxyUpstream forwards a request to CRAN without caching.
func (h *CRANHandler) proxyUpstream(w http.ResponseWriter, r *http.Request) {
	h.proxy.ProxyUpstream(w, r, h.upstreamURL+r.URL.Path, []string{"Accept-Encoding"})
}
