package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"
)

const (
	apkEcosystem = "alpine"
	// defaultAPKRepositoryName is the repository name used when no
	// upstream.apk repositories are configured.
	defaultAPKRepositoryName = "alpine"
	// defaultAPKUpstream is the official Alpine Linux mirror.
	defaultAPKUpstream = "https://dl-cdn.alpinelinux.org/alpine"
	apkMatchCount      = 3 // full match + name + version
)

// APKHandler handles Alpine APK repository protocol requests. Each configured
// upstream repository is mounted at /apk/{repository}/ and the remaining path
// mirrors the upstream layout ({release}/{repo}/{arch}/{file}).
//
// Repository indexes (v2 APKINDEX.tar.gz, v3 Packages.adb) and detached
// signatures are served byte-for-byte unchanged through the metadata cache so
// apk signature verification keeps working. Package files are cached in the
// shared artifact cache and stay available when the upstream is unreachable.
type APKHandler struct {
	proxy        *Proxy
	proxyURL     string
	repositories map[string]string
}

// NewAPKHandler creates an Alpine APK repository protocol handler.
// When repositories is empty, a single repository named "alpine" pointing at
// the official Alpine mirror is used.
func NewAPKHandler(proxy *Proxy, proxyURL string, repositories map[string]string) *APKHandler {
	h := &APKHandler{
		proxy:        proxy,
		proxyURL:     strings.TrimSuffix(proxyURL, "/"),
		repositories: make(map[string]string, len(repositories)),
	}
	for name, repositoryURL := range repositories {
		h.repositories[name] = strings.TrimSuffix(repositoryURL, "/")
	}
	if len(h.repositories) == 0 {
		h.repositories[defaultAPKRepositoryName] = defaultAPKUpstream
	}
	return h
}

// Routes returns the HTTP handler for APK requests.
// Mount this at /apk on your router.
func (h *APKHandler) Routes() http.Handler {
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

		repository, rest, ok := strings.Cut(path, "/")
		upstreamURL, found := h.repositories[repository]
		if !ok || rest == "" || !found {
			http.NotFound(w, r)
			return
		}

		switch {
		case isAPKIndex(rest) || isAPKSignature(rest):
			// Indexes and detached signatures are signed upstream metadata.
			// Cache them with the metadata TTL and serve the stored bytes
			// unchanged so apk verification continues to work.
			h.handleMetadata(w, r, repository, upstreamURL, rest)
		case strings.HasSuffix(rest, ".apk"):
			// Package downloads - cache these in the artifact cache.
			h.handlePackageDownload(w, r, repository, upstreamURL, rest)
		default:
			// Other files - proxy directly.
			h.proxyFile(w, r, upstreamURL, rest)
		}
	})
}

// isAPKIndex reports whether the path names a repository index:
// APKINDEX.tar.gz (apk v2) or Packages.adb (apk v3).
func isAPKIndex(path string) bool {
	base := path[strings.LastIndex(path, "/")+1:]
	return base == "APKINDEX.tar.gz" || base == "Packages.adb"
}

// isAPKSignature reports whether the path names a detached signature file.
func isAPKSignature(path string) bool {
	return strings.HasSuffix(path, ".sig") || strings.HasSuffix(path, ".rsa.pub")
}

// handlePackageDownload fetches and caches .apk packages.
// Path format: {release}/{repo}/{arch}/{name}-{version}-r{rel}.apk
// Example: v3.22/main/x86_64/busybox-1.37.0-r12.apk
//
// APK filenames do not include the architecture, so the same filename can hold
// different bytes per architecture (and per release). The full request path is
// therefore part of the cache identity.
func (h *APKHandler) handlePackageDownload(w http.ResponseWriter, r *http.Request, repository, upstreamURL, path string) {
	name, version, arch := h.parseAPKPath(path)
	if name == "" {
		// Can't parse, just proxy directly
		h.proxyFile(w, r, upstreamURL, path)
		return
	}

	downloadURL := upstreamURL + "/" + path
	cacheFilename := repository + "/" + path

	h.proxy.Logger.Info("apk package download",
		"repository", repository, "name", name, "version", version, "arch", arch)

	result, err := h.proxy.GetOrFetchArtifactFromURL(
		r.Context(), apkEcosystem, name, version, cacheFilename, downloadURL)
	if err != nil {
		h.proxy.serveArtifactError(w, err, "failed to fetch package")
		return
	}

	if result.ContentType == "" {
		result.ContentType = "application/octet-stream"
	}
	ServeArtifact(w, result)
}

// handleMetadata serves repository indexes and signatures through the
// metadata cache. Stored bytes are re-served verbatim, which keeps embedded
// and detached signatures valid.
func (h *APKHandler) handleMetadata(w http.ResponseWriter, r *http.Request, repository, upstreamURL, path string) {
	h.proxy.ProxyCached(w, r, upstreamURL+"/"+path, apkEcosystem,
		h.metadataCacheKey(repository, upstreamURL, path), "*/*")
}

// metadataCacheKey derives the metadata cache key from the repository name,
// its upstream URL, and the request path. Hashing the identity keeps distinct
// repositories from sharing cache entries (repository names may contain '_'
// and a separator-based key would be ambiguous) and drops cached entries when
// a repository is repointed at a different upstream, mirroring
// HelmHandler.indexCacheKey.
func (h *APKHandler) metadataCacheKey(repository, upstreamURL, path string) string {
	identity := repository + "\x00" + upstreamURL + "\x00" + path
	digest := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(digest[:])
}

// proxyFile proxies any file directly without caching.
func (h *APKHandler) proxyFile(w http.ResponseWriter, r *http.Request, upstreamURL, path string) {
	h.proxy.ProxyFile(w, r, upstreamURL+"/"+path)
}

// apkPackagePattern matches .apk filenames to extract name and version.
// Format: {name}-{version}-r{rel}.apk where version starts with a digit.
// Examples:
//   - busybox-1.37.0-r12.apk
//   - alpine-baselayout-data-3.7.0-r0.apk
var apkPackagePattern = regexp.MustCompile(`^(.+)-(\d[^-]*-r\d+)\.apk$`)

// parseAPKPath extracts package info from a path containing an APK filename.
// The architecture is taken from the parent directory since APK filenames do
// not include it.
func (h *APKHandler) parseAPKPath(path string) (name, version, arch string) {
	segments := strings.Split(path, "/")
	filename := segments[len(segments)-1]
	if len(segments) > 1 {
		arch = segments[len(segments)-2]
	}

	matches := apkPackagePattern.FindStringSubmatch(filename)
	if len(matches) != apkMatchCount {
		return "", "", ""
	}

	return matches[1], matches[2], arch
}
