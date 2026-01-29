package handler

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

const (
	// Default upstream for Fedora packages
	defaultRPMUpstream = "https://dl.fedoraproject.org/pub/fedora/linux"
)

// RPMHandler handles RPM/Yum repository protocol requests.
// It proxies requests to upstream RPM repositories and caches .rpm packages.
type RPMHandler struct {
	proxy       *Proxy
	upstreamURL string
	proxyURL    string
}

// NewRPMHandler creates a new RPM/Yum protocol handler.
func NewRPMHandler(proxy *Proxy, proxyURL string) *RPMHandler {
	return &RPMHandler{
		proxy:       proxy,
		upstreamURL: defaultRPMUpstream,
		proxyURL:    strings.TrimSuffix(proxyURL, "/"),
	}
}

// Routes returns the HTTP handler for RPM requests.
// Mount this at /rpm on your router.
func (h *RPMHandler) Routes() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		path := strings.TrimPrefix(r.URL.Path, "/")

		// Route based on path type
		switch {
		case strings.HasSuffix(path, ".rpm"):
			// Package downloads - cache these
			h.handlePackageDownload(w, r, path)
		case strings.Contains(path, "/repodata/"):
			// Repository metadata - proxy without caching (changes frequently)
			h.handleMetadata(w, r, path)
		default:
			// Other files - proxy directly
			h.proxyFile(w, r, path)
		}
	})
}

// handlePackageDownload fetches and caches .rpm packages.
// Path format varies by repo structure, e.g.:
//   - releases/39/Everything/x86_64/os/Packages/n/nginx-1.24.0-1.fc39.x86_64.rpm
//   - updates/39/Everything/x86_64/Packages/n/nginx-1.24.0-2.fc39.x86_64.rpm
func (h *RPMHandler) handlePackageDownload(w http.ResponseWriter, r *http.Request, path string) {
	// Parse the path to extract package info
	name, version, arch := h.parseRPMPath(path)
	if name == "" {
		// Can't parse, just proxy directly
		h.proxyFile(w, r, path)
		return
	}

	filename := path[strings.LastIndex(path, "/")+1:]
	downloadURL := fmt.Sprintf("%s/%s", h.upstreamURL, path)

	h.proxy.Logger.Info("rpm package download",
		"name", name, "version", version, "arch", arch, "filename", filename)

	result, err := h.proxy.GetOrFetchArtifactFromURL(
		r.Context(), "rpm", name, version, filename, downloadURL)
	if err != nil {
		h.proxy.Logger.Error("failed to get rpm package", "error", err)
		http.Error(w, "failed to fetch package", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/x-rpm")
	ServeArtifact(w, result)
}

// handleMetadata proxies repository metadata files (repomd.xml, primary.xml.gz, etc.).
// These change frequently so we don't cache them.
func (h *RPMHandler) handleMetadata(w http.ResponseWriter, r *http.Request, path string) {
	upstreamURL := fmt.Sprintf("%s/%s", h.upstreamURL, path)

	h.proxy.Logger.Debug("rpm metadata request", "path", path)

	req, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, nil)
	if err != nil {
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
	}

	// Forward relevant headers
	for _, header := range []string{"Accept", "Accept-Encoding", "If-Modified-Since", "If-None-Match"} {
		if v := r.Header.Get(header); v != "" {
			req.Header.Set(header, v)
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.proxy.Logger.Error("failed to fetch upstream metadata", "error", err)
		http.Error(w, "failed to fetch from upstream", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Copy response headers
	for _, header := range []string{"Content-Type", "Content-Length", "Last-Modified", "ETag"} {
		if v := resp.Header.Get(header); v != "" {
			w.Header().Set(header, v)
		}
	}

	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// proxyFile proxies any file directly without caching.
func (h *RPMHandler) proxyFile(w http.ResponseWriter, r *http.Request, path string) {
	upstreamURL := fmt.Sprintf("%s/%s", h.upstreamURL, path)

	req, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, nil)
	if err != nil {
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "failed to fetch from upstream", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	for key, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}

	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// rpmPackagePattern matches .rpm filenames to extract name, version, release, and arch.
// Format: {name}-{version}-{release}.{arch}.rpm
// Examples:
//   - nginx-1.24.0-1.fc39.x86_64.rpm
//   - kernel-core-6.5.5-200.fc38.x86_64.rpm
var rpmPackagePattern = regexp.MustCompile(`^(.+)-([^-]+)-([^-]+)\.([^.]+)\.rpm$`)

// parseRPMPath extracts package info from a path containing an RPM filename.
func (h *RPMHandler) parseRPMPath(path string) (name, version, arch string) {
	// Get the filename
	idx := strings.LastIndex(path, "/")
	filename := path
	if idx >= 0 {
		filename = path[idx+1:]
	}

	// Parse the filename
	matches := rpmPackagePattern.FindStringSubmatch(filename)
	if len(matches) != 5 {
		return "", "", ""
	}

	// name, version-release, arch
	return matches[1], matches[2] + "-" + matches[3], matches[4]
}
