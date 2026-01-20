package handler

import (
	"io"
	"net/http"
	"strings"
)

const (
	condaUpstream = "https://conda.anaconda.org"
)

// CondaHandler handles Conda/Anaconda registry protocol requests.
type CondaHandler struct {
	proxy       *Proxy
	upstreamURL string
	proxyURL    string
}

// NewCondaHandler creates a new Conda protocol handler.
func NewCondaHandler(proxy *Proxy, proxyURL string) *CondaHandler {
	return &CondaHandler{
		proxy:       proxy,
		upstreamURL: condaUpstream,
		proxyURL:    strings.TrimSuffix(proxyURL, "/"),
	}
}

// Routes returns the HTTP handler for Conda requests.
func (h *CondaHandler) Routes() http.Handler {
	mux := http.NewServeMux()

	// Channel index (repodata)
	mux.HandleFunc("GET /{channel}/{arch}/repodata.json", h.proxyUpstream)
	mux.HandleFunc("GET /{channel}/{arch}/repodata.json.bz2", h.proxyUpstream)
	mux.HandleFunc("GET /{channel}/{arch}/current_repodata.json", h.proxyUpstream)

	// Package downloads (cache these)
	mux.HandleFunc("GET /{channel}/{arch}/{filename}", h.handleDownload)

	return mux
}

// handleDownload serves a package file, fetching and caching from upstream if needed.
func (h *CondaHandler) handleDownload(w http.ResponseWriter, r *http.Request) {
	channel := r.PathValue("channel")
	arch := r.PathValue("arch")
	filename := r.PathValue("filename")

	// Only cache actual package files
	if !h.isPackageFile(filename) {
		h.proxyUpstream(w, r)
		return
	}

	name, version := h.parseFilename(filename)
	if name == "" {
		h.proxyUpstream(w, r)
		return
	}

	// Include channel in package name
	packageName := channel + "/" + name

	h.proxy.Logger.Info("conda download request",
		"channel", channel, "arch", arch, "name", name, "version", version)

	upstreamURL := h.upstreamURL + r.URL.Path

	result, err := h.proxy.GetOrFetchArtifactFromURL(r.Context(), "conda", packageName, version, filename, upstreamURL)
	if err != nil {
		h.proxy.Logger.Error("failed to get artifact", "error", err)
		http.Error(w, "failed to fetch package", http.StatusBadGateway)
		return
	}

	ServeArtifact(w, result)
}

// isPackageFile returns true if the filename is a Conda package.
func (h *CondaHandler) isPackageFile(filename string) bool {
	return strings.HasSuffix(filename, ".tar.bz2") || strings.HasSuffix(filename, ".conda")
}

// parseFilename extracts name and version from a Conda package filename.
// Conda filenames are: {name}-{version}-{build}.{ext}
// e.g., "numpy-1.24.0-py311h64a7726_0.conda"
func (h *CondaHandler) parseFilename(filename string) (name, version string) {
	// Remove extension
	base := filename
	for _, ext := range []string{".conda", ".tar.bz2"} {
		if strings.HasSuffix(base, ext) {
			base = strings.TrimSuffix(base, ext)
			break
		}
	}

	// Split by hyphens, the format is name-version-build
	// The name can contain hyphens, so we need to find version-build at the end
	parts := strings.Split(base, "-")
	if len(parts) < 3 {
		return "", ""
	}

	// Build is the last part, version is second to last
	// Everything before is the name
	build := parts[len(parts)-1]
	version = parts[len(parts)-2]
	name = strings.Join(parts[:len(parts)-2], "-")

	// Version should start with a digit
	if len(version) == 0 || version[0] < '0' || version[0] > '9' {
		return "", ""
	}

	// Include build in version for uniqueness
	_ = build

	return name, version
}

// proxyUpstream forwards a request to Anaconda without caching.
func (h *CondaHandler) proxyUpstream(w http.ResponseWriter, r *http.Request) {
	upstreamURL := h.upstreamURL + r.URL.Path

	h.proxy.Logger.Debug("proxying to upstream", "url", upstreamURL)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
	}

	// Copy accept-encoding for compression
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
