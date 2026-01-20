package handler

import (
	"io"
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
	mux.HandleFunc("GET /src/contrib/PACKAGES", h.proxyUpstream)
	mux.HandleFunc("GET /src/contrib/PACKAGES.gz", h.proxyUpstream)
	mux.HandleFunc("GET /src/contrib/PACKAGES.rds", h.proxyUpstream)

	// Binary package indexes
	mux.HandleFunc("GET /bin/{platform}/contrib/{rversion}/PACKAGES", h.proxyUpstream)
	mux.HandleFunc("GET /bin/{platform}/contrib/{rversion}/PACKAGES.gz", h.proxyUpstream)
	mux.HandleFunc("GET /bin/{platform}/contrib/{rversion}/PACKAGES.rds", h.proxyUpstream)

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

// proxyUpstream forwards a request to CRAN without caching.
func (h *CRANHandler) proxyUpstream(w http.ResponseWriter, r *http.Request) {
	upstreamURL := h.upstreamURL + r.URL.Path

	h.proxy.Logger.Debug("proxying to upstream", "url", upstreamURL)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
	}

	if ae := r.Header.Get("Accept-Encoding"); ae != "" {
		req.Header.Set("Accept-Encoding", ae)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.proxy.Logger.Error("upstream request failed", "error", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}

	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
