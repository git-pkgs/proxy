package handler

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

const (
	// Default upstream for Fedora packages
	defaultRPMUpstream = "https://dl.fedoraproject.org/pub/fedora/linux"
	rpmMatchCount      = 5 // full match + name + version + release + arch
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

		if containsPathTraversal(path) {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}

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
	cacheKey := strings.ReplaceAll(path, "/", "_")
	h.proxy.ProxyCached(w, r, fmt.Sprintf("%s/%s", h.upstreamURL, path), "rpm", cacheKey, "*/*")
}

// proxyFile proxies any file directly without caching.
func (h *RPMHandler) proxyFile(w http.ResponseWriter, r *http.Request, path string) {
	h.proxy.ProxyFile(w, r, fmt.Sprintf("%s/%s", h.upstreamURL, path))
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
	if len(matches) != rpmMatchCount {
		return "", "", ""
	}

	// name, version-release, arch
	return matches[1], matches[2] + "-" + matches[3], matches[4]
}
