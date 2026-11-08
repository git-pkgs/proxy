package handler

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/git-pkgs/proxy/internal/retention"
	"github.com/git-pkgs/purl"
)

const (
	// Default upstream for Fedora packages
	defaultRPMUpstream = "https://dl.fedoraproject.org/pub/fedora/linux"
	rpmMatchCount      = 5 // full match + name + version + release + arch
)

// Packages are stored as pkg:rpm/{name}, without a vendor and with the name's
// case kept. Package rules may name a vendor the way PURLs usually do,
// pkg:rpm/fedora/curl; it is dropped.
func init() {
	retention.Default.Register(retention.Spec{Key: "rpm", CanonicalPackage: rpmRetentionKey})
}

// rpmRetentionKey maps a package rule to the stored package PURL. A name with
// a slash, or a namespace with more than one segment, can never match a stored
// package and is refused.
func rpmRetentionKey(p *purl.PURL) (string, bool) {
	if p == nil || p.Type != "rpm" || strings.Contains(p.Namespace, "/") || strings.Contains(p.Name, "/") {
		return "", false
	}
	canonical := purl.MakePURLString("rpm", p.Name, "")
	return canonical, canonical != ""
}

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

// NewRPMHandlerWithUpstream creates an RPM handler with a custom upstream.
func NewRPMHandlerWithUpstream(proxy *Proxy, proxyURL, upstreamURL string) *RPMHandler {
	h := NewRPMHandler(proxy, proxyURL)
	h.upstreamURL = configuredUpstreamURL(upstreamURL, defaultRPMUpstream)
	return h
}

// Routes returns the HTTP handler for RPM requests.
// Mount this at /rpm on your router.
func (h *RPMHandler) Routes() http.Handler {
	return rejectQueryChars(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			// Repository metadata
			h.handleMetadata(w, r, path)
		default:
			// Other files - proxy directly
			h.proxyFile(w, r, path)
		}
	}))
}

// handlePackageDownload fetches and caches .rpm packages.
// Path format varies by repo structure, e.g.:
//   - releases/39/Everything/x86_64/os/Packages/n/nginx-1.24.0-1.fc39.x86_64.rpm
//   - updates/39/Everything/x86_64/Packages/n/nginx-1.24.0-2.fc39.x86_64.rpm
//
// The cache key is the full path, since repositories under one upstream
// (Rocky and Alma on one mirror, say) can ship different bytes under one
// filename.
func (h *RPMHandler) handlePackageDownload(w http.ResponseWriter, r *http.Request, path string) {
	// Parse the path to extract package info
	name, version, arch := h.parseRPMPath(path)
	if name == "" {
		// Can't parse, just proxy directly
		h.proxyFile(w, r, path)
		return
	}

	downloadURL := fmt.Sprintf("%s/%s", h.upstreamURL, path)

	h.proxy.Logger.Info("rpm package download",
		"name", name, "version", version, "arch", arch, "path", path)

	result, err := h.proxy.GetOrFetchArtifactFromURL(
		r.Context(), "rpm", name, version, path, downloadURL)
	if err != nil {
		h.proxy.serveArtifactError(w, err, "failed to fetch package")
		return
	}

	w.Header().Set(headerContentType, "application/x-rpm")
	ServeArtifactRequest(w, r, result)
}

// handleMetadata proxies repository metadata files (repomd.xml,
// primary.xml.gz, etc.). repomd.xml changes with every repository update and
// follows the metadata TTL. The files it points at are usually named after
// their own checksum and so never change; those are kept for good.
func (h *RPMHandler) handleMetadata(w http.ResponseWriter, r *http.Request, path string) {
	upstreamURL := fmt.Sprintf("%s/%s", h.upstreamURL, path)
	if rpmContentAddressed(path) {
		h.proxy.proxyImmutable(w, r, upstreamURL, "rpm", h.metadataCacheKey(path), "*/*")
		return
	}
	h.proxy.ProxyCached(w, r, upstreamURL, "rpm", h.metadataCacheKey(path), "*/*")
}

// rpmChecksumPrefix matches a repodata filename that createrepo prefixed
// with the file's checksum: <hex>-primary.xml.gz. The hex lengths are those
// of the checksum types createrepo_c offers: SHA-1, SHA-224, SHA-256,
// SHA-384 and SHA-512. Repositories built with --simple-md-filenames use
// plain names, which can be replaced in place and follow the TTL.
var rpmChecksumPrefix = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{56}|[0-9a-f]{64}|[0-9a-f]{96}|[0-9a-f]{128})-[^/]+$`)

// rpmContentAddressed reports whether a repodata path names a file after
// its checksum, so its content never changes.
func rpmContentAddressed(path string) bool {
	return rpmChecksumPrefix.MatchString(path[strings.LastIndex(path, "/")+1:])
}

// metadataCacheKey is the metadata cache key of a repository path.
func (h *RPMHandler) metadataCacheKey(path string) string {
	return hashedCacheKey(h.upstreamURL, path)
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
