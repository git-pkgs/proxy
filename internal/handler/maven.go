package handler

import (
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
)

const (
	mavenCentralUpstream       = "https://repo1.maven.org/maven2"
	gradlePluginPortalUpstream = "https://plugins.gradle.org/m2"
	minMavenParts              = 4 // group path segments + artifact + version + filename
	gradlePluginMarkerSuffix   = ".gradle.plugin"
)

// MavenHandler handles Maven repository protocol requests.
type MavenHandler struct {
	proxy                   *Proxy
	upstreamURL             string
	pluginPortalUpstreamURL string
	proxyURL                string
}

// NewMavenHandler creates a new Maven repository handler.
func NewMavenHandler(proxy *Proxy, proxyURL, upstreamURL, pluginPortalUpstreamURL string) *MavenHandler {
	if strings.TrimSpace(upstreamURL) == "" {
		upstreamURL = mavenCentralUpstream
	}
	if strings.TrimSpace(pluginPortalUpstreamURL) == "" {
		pluginPortalUpstreamURL = gradlePluginPortalUpstream
	}

	return &MavenHandler{
		proxy:                   proxy,
		upstreamURL:             strings.TrimSuffix(upstreamURL, "/"),
		pluginPortalUpstreamURL: strings.TrimSuffix(pluginPortalUpstreamURL, "/"),
		proxyURL:                strings.TrimSuffix(proxyURL, "/"),
	}
}

// Routes returns the HTTP handler for Maven requests.
func (h *MavenHandler) Routes() http.Handler {
	mux := http.NewServeMux()

	// Maven repository layout: /{group}/{artifact}/{version}/{filename}
	// e.g., /com/google/guava/guava/32.1.3-jre/guava-32.1.3-jre.jar
	mux.HandleFunc("GET /", h.handleRequest)

	return mux
}

// handleRequest routes Maven requests based on the path.
func (h *MavenHandler) handleRequest(w http.ResponseWriter, r *http.Request) {
	urlPath := strings.TrimPrefix(r.URL.Path, "/")
	if urlPath == "" {
		http.NotFound(w, r)
		return
	}

	// Check if this is an artifact file or metadata
	filename := path.Base(urlPath)

	if h.isMetadataFile(filename) {
		h.handleMetadata(w, r, urlPath, filename)
		return
	}

	if h.isArtifactFile(filename) {
		// Cache artifact files
		h.handleDownload(w, r, urlPath)
		return
	}

	// Proxy everything else (directory listings, checksums, etc.)
	h.proxyUpstream(w, r)
}

func (h *MavenHandler) handleMetadata(w http.ResponseWriter, r *http.Request, urlPath, filename string) {
	cacheKey := strings.ReplaceAll(urlPath, "/", "_")
	if !h.shouldFallbackToPluginPortal(urlPath, filename) {
		h.proxy.ProxyCached(w, r, h.upstreamURL+r.URL.Path, "maven", cacheKey, "*/*")
		return
	}

	upstreamURL := fmt.Sprintf("%s/%s", h.upstreamURL, urlPath)

	body, contentType, err := h.proxy.FetchOrCacheMetadata(r.Context(), "maven", cacheKey, upstreamURL, "*/*")
	if err != nil && h.shouldFallbackToPluginPortal(urlPath, filename) {
		pluginPortalURL := fmt.Sprintf("%s/%s", h.pluginPortalUpstreamURL, urlPath)
		h.proxy.Logger.Info("maven metadata unavailable in primary upstream, trying Gradle Plugin Portal",
			"path", urlPath, "filename", filename)
		body, contentType, err = h.proxy.FetchOrCacheMetadata(r.Context(), "maven", cacheKey, pluginPortalURL, "*/*")
	}
	if err != nil {
		if errors.Is(err, ErrUpstreamNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		h.proxy.Logger.Error("metadata fetch failed", "error", err)
		http.Error(w, "failed to fetch from upstream", http.StatusBadGateway)
		return
	}

	h.writeMetadataResponse(w, r, cacheKey, body, contentType)
}

func (h *MavenHandler) writeMetadataResponse(w http.ResponseWriter, r *http.Request, cacheKey string, body []byte, contentType string) {
	cm := h.proxy.lookupCachedMeta("maven", cacheKey)

	if cm.etag != "" {
		if match := r.Header.Get("If-None-Match"); match != "" && match == cm.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	if !cm.lastModified.IsZero() {
		if ims := r.Header.Get("If-Modified-Since"); ims != "" {
			if t, err := http.ParseTime(ims); err == nil && !cm.lastModified.After(t) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if cm.etag != "" {
		w.Header().Set("ETag", cm.etag)
	}
	if !cm.lastModified.IsZero() {
		w.Header().Set("Last-Modified", cm.lastModified.UTC().Format(http.TimeFormat))
	}
	if cm.stale {
		w.Header().Set("Warning", `110 - "Response is Stale"`)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// handleDownload serves an artifact file, fetching and caching from upstream if needed.
func (h *MavenHandler) handleDownload(w http.ResponseWriter, r *http.Request, urlPath string) {
	// Parse Maven path: group/artifact/version/filename
	// e.g., com/google/guava/guava/32.1.3-jre/guava-32.1.3-jre.jar
	group, artifact, version, filename := h.parsePath(urlPath)
	if artifact == "" {
		h.proxyUpstream(w, r)
		return
	}

	// Maven uses group:artifact as the package name
	name := fmt.Sprintf("%s:%s", group, artifact)

	h.proxy.Logger.Info("maven download request",
		"group", group, "artifact", artifact, "version", version, "filename", filename)

	upstreamURL := fmt.Sprintf("%s/%s", h.upstreamURL, urlPath)

	result, err := h.proxy.GetOrFetchArtifactFromURL(r.Context(), "maven", name, version, filename, upstreamURL)
	if err != nil && h.shouldFallbackToPluginPortal(urlPath, filename) {
		pluginPortalURL := fmt.Sprintf("%s/%s", h.pluginPortalUpstreamURL, urlPath)
		h.proxy.Logger.Info("maven artifact not found in primary upstream, trying Gradle Plugin Portal",
			"group", group, "artifact", artifact, "version", version, "filename", filename)
		result, err = h.proxy.GetOrFetchArtifactFromURL(r.Context(), "maven", name, version, filename, pluginPortalURL)
	}
	if err != nil {
		h.proxy.Logger.Error("failed to get artifact", "error", err)
		http.Error(w, "failed to fetch artifact", http.StatusBadGateway)
		return
	}

	ServeArtifact(w, result)
}

// parsePath extracts Maven coordinates from a URL path.
// e.g., "com/google/guava/guava/32.1.3-jre/guava-32.1.3-jre.jar"
// -> ("com.google.guava", "guava", "32.1.3-jre", "guava-32.1.3-jre.jar")
func (h *MavenHandler) parsePath(urlPath string) (group, artifact, version, filename string) {
	parts := strings.Split(urlPath, "/")
	if len(parts) < minMavenParts {
		return "", "", "", ""
	}

	filename = parts[len(parts)-1]
	version = parts[len(parts)-2]
	artifact = parts[len(parts)-3]
	groupParts := parts[:len(parts)-3]
	group = strings.Join(groupParts, ".")

	return group, artifact, version, filename
}

// isArtifactFile returns true if the filename looks like a Maven artifact.
func (h *MavenHandler) isArtifactFile(filename string) bool {
	// Common artifact extensions
	extensions := []string{".jar", ".war", ".ear", ".pom", ".aar", ".klib", ".module"}
	for _, ext := range extensions {
		if strings.HasSuffix(filename, ext) {
			return true
		}
	}
	return false
}

func (h *MavenHandler) shouldFallbackToPluginPortal(urlPath, filename string) bool {
	if !h.isPluginPortalFallbackFile(filename) {
		return false
	}

	group, artifact, ok := h.extractGroupAndArtifactFromPath(urlPath, filename)
	if !ok {
		return false
	}

	if !strings.HasSuffix(artifact, gradlePluginMarkerSuffix) {
		return false
	}

	markerPrefix := strings.TrimSuffix(artifact, gradlePluginMarkerSuffix)
	return markerPrefix != "" && markerPrefix == group
}

func (h *MavenHandler) isPluginPortalFallbackFile(filename string) bool {
	if filename == "maven-metadata.xml" || strings.HasPrefix(filename, "maven-metadata.xml.") {
		return true
	}
	if strings.HasSuffix(filename, ".pom") || strings.HasSuffix(filename, ".module") {
		return true
	}

	for _, suffix := range []string{".sha1", ".sha256", ".sha512", ".md5", ".asc"} {
		if !strings.HasSuffix(filename, suffix) {
			continue
		}

		base := strings.TrimSuffix(filename, suffix)
		return strings.HasSuffix(base, ".pom") || strings.HasSuffix(base, ".module")
	}

	return false
}

func (h *MavenHandler) extractGroupAndArtifactFromPath(urlPath, filename string) (group, artifact string, ok bool) {
	const (
		artifactVersionPathOffset = 3 // .../{artifact}/{version}/{filename}
		metadataPathOffset        = 2 // .../{artifact}/maven-metadata.xml
	)

	parts := strings.Split(urlPath, "/")

	groupEnd := len(parts) - artifactVersionPathOffset
	artifactIdx := len(parts) - artifactVersionPathOffset
	if filename == "maven-metadata.xml" || strings.HasPrefix(filename, "maven-metadata.xml.") {
		groupEnd = len(parts) - metadataPathOffset
		artifactIdx = len(parts) - metadataPathOffset
	}

	if groupEnd <= 0 || artifactIdx < 0 || artifactIdx >= len(parts) {
		return "", "", false
	}

	group = strings.Join(parts[:groupEnd], ".")
	artifact = parts[artifactIdx]
	return group, artifact, group != "" && artifact != ""
}

// isMetadataFile returns true if the filename is Maven metadata.
func (h *MavenHandler) isMetadataFile(filename string) bool {
	return filename == "maven-metadata.xml" ||
		strings.HasSuffix(filename, ".sha1") ||
		strings.HasSuffix(filename, ".sha256") ||
		strings.HasSuffix(filename, ".sha512") ||
		strings.HasSuffix(filename, ".md5") ||
		strings.HasSuffix(filename, ".asc")
}

// proxyUpstream forwards a request to Maven Central without caching.
func (h *MavenHandler) proxyUpstream(w http.ResponseWriter, r *http.Request) {
	h.proxy.ProxyUpstream(w, r, h.upstreamURL+r.URL.Path, nil)
}
