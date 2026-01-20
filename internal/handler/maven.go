package handler

import (
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
)

const (
	mavenUpstream = "https://repo1.maven.org/maven2"
)

// MavenHandler handles Maven repository protocol requests.
type MavenHandler struct {
	proxy       *Proxy
	upstreamURL string
	proxyURL    string
}

// NewMavenHandler creates a new Maven repository handler.
func NewMavenHandler(proxy *Proxy, proxyURL string) *MavenHandler {
	return &MavenHandler{
		proxy:       proxy,
		upstreamURL: mavenUpstream,
		proxyURL:    strings.TrimSuffix(proxyURL, "/"),
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
		// Proxy metadata without caching
		h.proxyUpstream(w, r)
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
	if len(parts) < 4 {
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
	extensions := []string{".jar", ".war", ".ear", ".pom", ".aar", ".klib"}
	for _, ext := range extensions {
		if strings.HasSuffix(filename, ext) {
			return true
		}
	}
	return false
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
	upstreamURL := h.upstreamURL + r.URL.Path

	h.proxy.Logger.Debug("proxying to upstream", "url", upstreamURL)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
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
