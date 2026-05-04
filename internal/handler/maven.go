package handler

import (
	"fmt"
	"net/http"
	"path"
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
		cacheKey := strings.ReplaceAll(urlPath, "/", "_")
		h.proxy.ProxyCached(w, r, h.upstreamURL+r.URL.Path, "maven", cacheKey, "*/*")
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
	if err != nil && h.shouldFallbackToPluginPortal(group, artifact, filename) {
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

func (h *MavenHandler) shouldFallbackToPluginPortal(group, artifact, filename string) bool {
	if h.pluginPortalUpstreamURL == "" {
		return false
	}
	if !strings.HasSuffix(filename, ".pom") && !strings.HasSuffix(filename, ".module") {
		return false
	}
	if !strings.HasSuffix(artifact, gradlePluginMarkerSuffix) {
		return false
	}

	markerPrefix := strings.TrimSuffix(artifact, gradlePluginMarkerSuffix)
	return markerPrefix != "" && markerPrefix == group
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
