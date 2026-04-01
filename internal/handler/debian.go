package handler

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

const (
	debianUpstream = "http://deb.debian.org/debian"
	debMatchCount  = 4 // full match + name + version + arch
)

// DebianHandler handles APT/Debian repository protocol requests.
// It proxies requests to upstream Debian/Ubuntu repositories and caches .deb packages.
type DebianHandler struct {
	proxy       *Proxy
	upstreamURL string
	proxyURL    string
}

// NewDebianHandler creates a new Debian/APT protocol handler.
func NewDebianHandler(proxy *Proxy, proxyURL string) *DebianHandler {
	return &DebianHandler{
		proxy:       proxy,
		upstreamURL: debianUpstream,
		proxyURL:    strings.TrimSuffix(proxyURL, "/"),
	}
}

// Routes returns the HTTP handler for Debian requests.
// Mount this at /debian on your router.
func (h *DebianHandler) Routes() http.Handler {
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
		case strings.HasPrefix(path, "pool/"):
			// Package downloads - cache these
			h.handlePackageDownload(w, r, path)
		case strings.HasPrefix(path, "dists/"):
			// Repository metadata - proxy without caching (changes frequently)
			h.handleMetadata(w, r, path)
		default:
			// Other files (like README, etc.) - proxy directly
			h.proxyFile(w, r, path)
		}
	})
}

// handlePackageDownload fetches and caches .deb packages from the pool.
// Pool path format: pool/{component}/{prefix}/{name}/{filename}
// Example: pool/main/n/nginx/nginx_1.18.0-6_amd64.deb
func (h *DebianHandler) handlePackageDownload(w http.ResponseWriter, r *http.Request, path string) {
	// Parse the path to extract package info
	name, version, arch := h.parsePoolPath(path)
	if name == "" {
		// Can't parse, just proxy directly
		h.proxyFile(w, r, path)
		return
	}

	filename := path[strings.LastIndex(path, "/")+1:]
	downloadURL := fmt.Sprintf("%s/%s", h.upstreamURL, path)

	h.proxy.Logger.Info("debian package download",
		"name", name, "version", version, "arch", arch, "filename", filename)

	result, err := h.proxy.GetOrFetchArtifactFromURL(
		r.Context(), "deb", name, version, filename, downloadURL)
	if err != nil {
		h.proxy.Logger.Error("failed to get debian package", "error", err)
		http.Error(w, "failed to fetch package", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/vnd.debian.binary-package")
	ServeArtifact(w, result)
}

// handleMetadata proxies repository metadata files.
// These change frequently so we don't cache them.
func (h *DebianHandler) handleMetadata(w http.ResponseWriter, r *http.Request, path string) {
	cacheKey := strings.ReplaceAll(path, "/", "_")
	h.proxy.ProxyCached(w, r, fmt.Sprintf("%s/%s", h.upstreamURL, path), "debian", cacheKey, "*/*")
}

// proxyFile proxies any file directly without caching.
func (h *DebianHandler) proxyFile(w http.ResponseWriter, r *http.Request, path string) {
	h.proxy.ProxyFile(w, r, fmt.Sprintf("%s/%s", h.upstreamURL, path))
}

// debPackagePattern matches .deb filenames to extract name, version, and arch.
// Format: {name}_{version}_{arch}.deb
var debPackagePattern = regexp.MustCompile(`^(.+)_([^_]+)_([^_]+)\.deb$`)

// parsePoolPath extracts package info from a pool path.
// Example: pool/main/n/nginx/nginx_1.18.0-6_amd64.deb
func (h *DebianHandler) parsePoolPath(path string) (name, version, arch string) {
	// Get the filename
	idx := strings.LastIndex(path, "/")
	if idx < 0 {
		return "", "", ""
	}
	filename := path[idx+1:]

	// Parse the filename
	matches := debPackagePattern.FindStringSubmatch(filename)
	if len(matches) != debMatchCount {
		return "", "", ""
	}

	return matches[1], matches[2], matches[3]
}
