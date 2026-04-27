package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/git-pkgs/purl"
)

const (
	composerUpstream   = "https://packagist.org"
	composerRepo       = "https://repo.packagist.org"
	vendorPackageParts = 2
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
		"packages":           map[string]any{},
		"metadata-url":       h.proxyURL + "/composer/p2/%package%.json",
		"notify-batch":       h.upstreamURL + "/downloads/",
		"search":             h.proxyURL + "/composer/search.json?q=%query%&type=%type%",
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
	parts := strings.SplitN(path, "/", vendorPackageParts)
	if len(parts) != vendorPackageParts || parts[0] == "" || parts[1] == "" {
		http.Error(w, "invalid package path", http.StatusBadRequest)
		return
	}
	vendor := parts[0]
	pkg := parts[1]
	packageName := vendor + "/" + pkg

	h.proxy.Logger.Info("composer metadata request", "package", packageName)

	upstreamURL := fmt.Sprintf("%s/p2/%s/%s.json", h.repoURL, vendor, pkg)

	body, _, err := h.proxy.FetchOrCacheMetadata(r.Context(), "composer", packageName, upstreamURL)
	if err != nil {
		if errors.Is(err, ErrUpstreamNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		h.proxy.Logger.Error("upstream request failed", "error", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
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
// If the metadata uses the minified Composer v2 format, it is expanded first so
// that every version entry contains all fields. If cooldown is enabled, versions
// published too recently are filtered out.
func (h *ComposerHandler) rewriteMetadata(body []byte) ([]byte, error) {
	var metadata map[string]any
	if err := json.Unmarshal(body, &metadata); err != nil {
		return nil, err
	}

	packages, ok := metadata["packages"].(map[string]any)
	if !ok {
		return body, nil
	}

	minified := metadata["minified"] == "composer/2.0"

	for packageName, versions := range packages {
		versionList, ok := versions.([]any)
		if !ok {
			continue
		}

		if minified {
			versionList = expandMinifiedVersions(versionList)
		}

		packages[packageName] = h.filterAndRewriteVersions(packageName, versionList)
	}

	delete(metadata, "minified")

	return json.Marshal(metadata)
}

// expandMinifiedVersions expands the Composer v2 minified format where each
// version entry only contains fields that differ from the previous entry.
// The "~dev" sentinel string resets the inheritance chain.
func expandMinifiedVersions(versionList []any) []any {
	expanded := make([]any, 0, len(versionList))
	inherited := map[string]any{}

	for _, v := range versionList {
		// The "~dev" sentinel resets the inheritance chain for dev versions.
		if s, ok := v.(string); ok && s == "~dev" {
			inherited = map[string]any{}
			continue
		}

		vmap, ok := v.(map[string]any)
		if !ok {
			continue
		}

		// Merge inherited fields into a new map, then overlay current fields.
		// Deep copy values to avoid shared references between versions.
		merged := make(map[string]any, len(inherited)+len(vmap))
		for k, val := range inherited {
			merged[k] = deepCopyValue(val)
		}
		for k, val := range vmap {
			merged[k] = val
		}

		// Update inherited state for next iteration.
		inherited = merged

		expanded = append(expanded, merged)
	}

	return expanded
}

// deepCopyValue returns a deep copy of JSON-like values (maps, slices, scalars).
func deepCopyValue(v any) any {
	switch val := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(val))
		for k, v := range val {
			m[k] = deepCopyValue(v)
		}
		return m
	case []any:
		s := make([]any, len(val))
		for i, v := range val {
			s[i] = deepCopyValue(v)
		}
		return s
	default:
		return v
	}
}

// filterAndRewriteVersions applies cooldown filtering and rewrites dist URLs
// for a single package's version list.
func (h *ComposerHandler) filterAndRewriteVersions(packageName string, versionList []any) []any {
	packagePURL := purl.MakePURLString("composer", packageName, "")

	filtered := versionList[:0]
	for _, v := range versionList {
		vmap, ok := v.(map[string]any)
		if !ok {
			continue
		}

		version, _ := vmap["version"].(string)

		if h.shouldFilterVersion(packagePURL, packageName, version, vmap) {
			continue
		}

		h.rewriteDistURL(vmap, packageName, version)
		filtered = append(filtered, v)
	}

	return filtered
}

// shouldFilterVersion returns true if the version should be excluded due to cooldown.
func (h *ComposerHandler) shouldFilterVersion(packagePURL, packageName, version string, vmap map[string]any) bool {
	if h.proxy.Cooldown == nil || !h.proxy.Cooldown.Enabled() {
		return false
	}

	timeStr, ok := vmap["time"].(string)
	if !ok {
		return false
	}

	publishedAt, err := time.Parse(time.RFC3339, timeStr)
	if err != nil {
		return false
	}

	if !h.proxy.Cooldown.IsAllowed("composer", packagePURL, publishedAt) {
		h.proxy.Logger.Info("cooldown: filtering composer version",
			"package", packageName, "version", version)
		return true
	}

	return false
}

// rewriteDistURL rewrites the dist URL in a version entry to point at this proxy.
func (h *ComposerHandler) rewriteDistURL(vmap map[string]any, packageName, version string) {
	dist, ok := vmap["dist"].(map[string]any)
	if !ok {
		return
	}

	url, ok := dist["url"].(string)
	if !ok || url == "" {
		return
	}

	filename := "package.zip"
	if idx := strings.LastIndex(url, "/"); idx >= 0 {
		filename = url[idx+1:]
	}

	// GitHub zipball URLs end with a bare commit hash (no extension).
	// Append .zip so the archives library can detect the format.
	if path.Ext(filename) == "" {
		if distType, _ := dist["type"].(string); distType == "zip" {
			filename += ".zip"
		}
	}

	parts := strings.SplitN(packageName, "/", vendorPackageParts)
	if len(parts) == vendorPackageParts {
		newURL := fmt.Sprintf("%s/composer/files/%s/%s/%s/%s",
			h.proxyURL, parts[0], parts[1], version, filename)
		dist["url"] = newURL
	}
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

	resp, err := h.proxy.HTTPClient.Do(req)
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

	resp, err := h.proxy.HTTPClient.Do(req)
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
