package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	composerUpstream = "https://packagist.org"
	composerRepo     = "https://repo.packagist.org"
)

// ComposerHandler handles Composer/Packagist registry protocol requests.
type ComposerHandler struct {
	proxy       *Proxy
	upstreamURL string
	repoURL     string
	proxyURL    string
}

// NewComposerHandler creates a new Composer protocol handler.
func NewComposerHandler(proxy *Proxy, proxyURL string) *ComposerHandler {
	return &ComposerHandler{
		proxy:       proxy,
		upstreamURL: composerUpstream,
		repoURL:     composerRepo,
		proxyURL:    strings.TrimSuffix(proxyURL, "/"),
	}
}

// Routes returns the HTTP handler for Composer requests.
func (h *ComposerHandler) Routes() http.Handler {
	mux := http.NewServeMux()

	// Service index
	mux.HandleFunc("GET /packages.json", h.handleServiceIndex)

	// Package metadata (Composer v2 format) - use prefix since {package}.json isn't allowed
	mux.HandleFunc("GET /p2/", h.handlePackageMetadata)

	// Package downloads
	mux.HandleFunc("GET /files/{vendor}/{package}/{version}/{filename}", h.handleDownload)

	// Search and list (proxy without modification)
	mux.HandleFunc("GET /search.json", h.proxyUpstream)
	mux.HandleFunc("GET /packages/list.json", h.proxyUpstream)

	return mux
}

// handleServiceIndex returns the Composer repository service index.
func (h *ComposerHandler) handleServiceIndex(w http.ResponseWriter, r *http.Request) {
	// Return a minimal service index pointing to our proxy
	index := map[string]any{
		"packages":          map[string]any{},
		"metadata-url":      h.proxyURL + "/composer/p2/%package%.json",
		"notify-batch":      h.upstreamURL + "/downloads/",
		"search":            h.proxyURL + "/composer/search.json?q=%query%&type=%type%",
		"providers-lazy-url": h.proxyURL + "/composer/p2/%package%.json",
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(index)
}

// handlePackageMetadata proxies and rewrites package metadata.
func (h *ComposerHandler) handlePackageMetadata(w http.ResponseWriter, r *http.Request) {
	// Parse path: /p2/{vendor}/{package}.json
	path := strings.TrimPrefix(r.URL.Path, "/p2/")
	path = strings.TrimSuffix(path, ".json")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		http.Error(w, "invalid package path", http.StatusBadRequest)
		return
	}
	vendor := parts[0]
	pkg := parts[1]
	packageName := vendor + "/" + pkg

	h.proxy.Logger.Info("composer metadata request", "package", packageName)

	// Fetch from repo.packagist.org (Composer v2 metadata)
	upstreamURL := fmt.Sprintf("%s/p2/%s/%s.json", h.repoURL, vendor, pkg)

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

	if resp.StatusCode != http.StatusOK {
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "failed to read response", http.StatusInternalServerError)
		return
	}

	rewritten, err := h.rewriteMetadata(body)
	if err != nil {
		h.proxy.Logger.Warn("failed to rewrite metadata, proxying original", "error", err)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(rewritten)
}

// rewriteMetadata rewrites dist URLs in Composer metadata to point at this proxy.
func (h *ComposerHandler) rewriteMetadata(body []byte) ([]byte, error) {
	var metadata map[string]any
	if err := json.Unmarshal(body, &metadata); err != nil {
		return nil, err
	}

	packages, ok := metadata["packages"].(map[string]any)
	if !ok {
		return body, nil
	}

	for packageName, versions := range packages {
		versionList, ok := versions.([]any)
		if !ok {
			continue
		}

		for _, v := range versionList {
			vmap, ok := v.(map[string]any)
			if !ok {
				continue
			}

			version, _ := vmap["version"].(string)
			dist, ok := vmap["dist"].(map[string]any)
			if !ok {
				continue
			}

			// Rewrite the dist URL
			if url, ok := dist["url"].(string); ok && url != "" {
				// Extract filename from URL
				filename := "package.zip"
				if idx := strings.LastIndex(url, "/"); idx >= 0 {
					filename = url[idx+1:]
				}

				// Build new URL through our proxy
				parts := strings.SplitN(packageName, "/", 2)
				if len(parts) == 2 {
					newURL := fmt.Sprintf("%s/composer/files/%s/%s/%s/%s",
						h.proxyURL, parts[0], parts[1], version, filename)
					dist["url"] = newURL
				}
			}
		}
	}

	return json.Marshal(metadata)
}

// handleDownload serves a package file, fetching and caching from upstream if needed.
func (h *ComposerHandler) handleDownload(w http.ResponseWriter, r *http.Request) {
	vendor := r.PathValue("vendor")
	pkg := r.PathValue("package")
	version := r.PathValue("version")
	filename := r.PathValue("filename")

	packageName := vendor + "/" + pkg

	h.proxy.Logger.Info("composer download request",
		"package", packageName, "version", version, "filename", filename)

	// We need to fetch the metadata to get the actual download URL
	// since Packagist URLs include a hash
	metaURL := fmt.Sprintf("%s/p2/%s/%s.json", h.repoURL, vendor, pkg)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, metaURL, nil)
	if err != nil {
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.proxy.Logger.Error("failed to fetch metadata", "error", err)
		http.Error(w, "failed to fetch metadata", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		http.Error(w, "package not found", http.StatusNotFound)
		return
	}

	var metadata map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&metadata); err != nil {
		http.Error(w, "failed to parse metadata", http.StatusInternalServerError)
		return
	}

	// Find the download URL for this version
	downloadURL := h.findDownloadURL(metadata, packageName, version)
	if downloadURL == "" {
		http.Error(w, "version not found", http.StatusNotFound)
		return
	}

	result, err := h.proxy.GetOrFetchArtifactFromURL(r.Context(), "composer", packageName, version, filename, downloadURL)
	if err != nil {
		h.proxy.Logger.Error("failed to get artifact", "error", err)
		http.Error(w, "failed to fetch package", http.StatusBadGateway)
		return
	}

	ServeArtifact(w, result)
}

// findDownloadURL finds the dist URL for a specific version in metadata.
func (h *ComposerHandler) findDownloadURL(metadata map[string]any, packageName, version string) string {
	packages, ok := metadata["packages"].(map[string]any)
	if !ok {
		return ""
	}

	versions, ok := packages[packageName].([]any)
	if !ok {
		return ""
	}

	for _, v := range versions {
		vmap, ok := v.(map[string]any)
		if !ok {
			continue
		}

		if vmap["version"] == version {
			if dist, ok := vmap["dist"].(map[string]any); ok {
				if url, ok := dist["url"].(string); ok {
					return url
				}
			}
		}
	}

	return ""
}

// proxyUpstream forwards a request to packagist.org without caching.
func (h *ComposerHandler) proxyUpstream(w http.ResponseWriter, r *http.Request) {
	upstreamURL := h.upstreamURL + r.URL.Path
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

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
