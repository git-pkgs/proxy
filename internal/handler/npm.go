package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/git-pkgs/purl"
)

const (
	npmUpstream = "https://registry.npmjs.org"
)

// NPMHandler handles npm registry protocol requests.
type NPMHandler struct {
	proxy       *Proxy
	upstreamURL string
	proxyURL    string // URL where this proxy is hosted
}

// NewNPMHandler creates a new npm protocol handler.
func NewNPMHandler(proxy *Proxy, proxyURL string) *NPMHandler {
	return &NPMHandler{
		proxy:       proxy,
		upstreamURL: npmUpstream,
		proxyURL:    strings.TrimSuffix(proxyURL, "/"),
	}
}

// Routes returns the HTTP handler for npm requests.
// Mount this at /npm on your router.
func (h *NPMHandler) Routes() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		path := strings.TrimPrefix(r.URL.Path, "/")

		// Check if this is a tarball download (contains /-/)
		if strings.Contains(path, "/-/") {
			h.handleDownload(w, r)
			return
		}

		// Otherwise it's a metadata request
		h.handlePackageMetadata(w, r)
	})
}

// handlePackageMetadata proxies package metadata from upstream and rewrites tarball URLs.
func (h *NPMHandler) handlePackageMetadata(w http.ResponseWriter, r *http.Request) {
	packageName := h.extractPackageName(r)
	if packageName == "" {
		JSONError(w, http.StatusBadRequest, "invalid package name")
		return
	}

	h.proxy.Logger.Info("npm metadata request", "package", packageName)

	// Fetch metadata from upstream
	upstreamURL := fmt.Sprintf("%s/%s", h.upstreamURL, url.PathEscape(packageName))

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		JSONError(w, http.StatusInternalServerError, "failed to create request")
		return
	}
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.proxy.Logger.Error("failed to fetch upstream metadata", "error", err)
		JSONError(w, http.StatusBadGateway, "failed to fetch from upstream")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		JSONError(w, http.StatusNotFound, "package not found")
		return
	}
	if resp.StatusCode != http.StatusOK {
		JSONError(w, http.StatusBadGateway, fmt.Sprintf("upstream returned %d", resp.StatusCode))
		return
	}

	// Parse and rewrite tarball URLs
	body, err := ReadMetadata(resp.Body)
	if err != nil {
		JSONError(w, http.StatusInternalServerError, "failed to read response")
		return
	}

	rewritten, err := h.rewriteMetadata(packageName, body)
	if err != nil {
		// If rewriting fails, just proxy the original
		h.proxy.Logger.Warn("failed to rewrite metadata, proxying original", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(rewritten)
}

// rewriteMetadata rewrites tarball URLs in npm package metadata to point at this proxy.
// If cooldown is enabled, versions published too recently are filtered out.
func (h *NPMHandler) rewriteMetadata(packageName string, body []byte) ([]byte, error) {
	var metadata map[string]any
	if err := json.Unmarshal(body, &metadata); err != nil {
		return nil, err
	}

	// Rewrite tarball URLs in versions
	versions, ok := metadata["versions"].(map[string]any)
	if !ok {
		return body, nil // No versions to rewrite
	}

	// Apply cooldown filtering
	if h.proxy.Cooldown != nil && h.proxy.Cooldown.Enabled() {
		timeMap, _ := metadata["time"].(map[string]any)
		packagePURL := purl.MakePURLString("npm", packageName, "")

		for version := range versions {
			if timeMap == nil {
				continue
			}
			publishedStr, ok := timeMap[version].(string)
			if !ok {
				continue
			}
			publishedAt, err := time.Parse(time.RFC3339, publishedStr)
			if err != nil {
				continue
			}
			if !h.proxy.Cooldown.IsAllowed("npm", packagePURL, publishedAt) {
				h.proxy.Logger.Info("cooldown: filtering npm version",
					"package", packageName, "version", version,
					"published", publishedStr)
				delete(versions, version)
				delete(timeMap, version)
			}
		}

		// Update dist-tags.latest if it was filtered
		if distTags, ok := metadata["dist-tags"].(map[string]any); ok {
			if latest, ok := distTags["latest"].(string); ok {
				if _, exists := versions[latest]; !exists {
					// Find newest remaining version from the time map
					newLatest := h.findNewestVersion(versions, timeMap)
					if newLatest != "" {
						distTags["latest"] = newLatest
					}
				}
			}
		}
	}

	for version, vdata := range versions {
		vmap, ok := vdata.(map[string]any)
		if !ok {
			continue
		}

		dist, ok := vmap["dist"].(map[string]any)
		if !ok {
			continue
		}

		if tarball, ok := dist["tarball"].(string); ok {
			// Extract filename from tarball URL
			filename := tarball
			if idx := strings.LastIndex(tarball, "/"); idx >= 0 {
				filename = tarball[idx+1:]
			}

			// Rewrite to our proxy URL
			escapedName := url.PathEscape(packageName)
			newTarball := fmt.Sprintf("%s/npm/%s/-/%s", h.proxyURL, escapedName, filename)
			dist["tarball"] = newTarball

			h.proxy.Logger.Debug("rewrote tarball URL",
				"package", packageName, "version", version,
				"old", tarball, "new", newTarball)
		}
	}

	return json.Marshal(metadata)
}

// findNewestVersion returns the version string with the most recent timestamp
// from the remaining versions, using the time map.
func (h *NPMHandler) findNewestVersion(versions map[string]any, timeMap map[string]any) string {
	if timeMap == nil {
		return ""
	}

	type versionTime struct {
		version string
		t       time.Time
	}

	var vts []versionTime
	for v := range versions {
		if ts, ok := timeMap[v].(string); ok {
			if t, err := time.Parse(time.RFC3339, ts); err == nil {
				vts = append(vts, versionTime{v, t})
			}
		}
	}

	if len(vts) == 0 {
		return ""
	}

	sort.Slice(vts, func(i, j int) bool {
		return vts[i].t.After(vts[j].t)
	})

	return vts[0].version
}

// handleDownload serves a package tarball, fetching and caching from upstream if needed.
func (h *NPMHandler) handleDownload(w http.ResponseWriter, r *http.Request) {
	packageName, filename := h.parseDownloadPath(r.URL.Path)

	if packageName == "" || filename == "" {
		JSONError(w, http.StatusBadRequest, "invalid request")
		return
	}

	// Extract version from filename (e.g., "lodash-4.17.21.tgz" -> "4.17.21")
	version := h.extractVersionFromFilename(packageName, filename)
	if version == "" {
		JSONError(w, http.StatusBadRequest, "could not determine version from filename")
		return
	}

	h.proxy.Logger.Info("npm download request",
		"package", packageName, "version", version, "filename", filename)

	result, err := h.proxy.GetOrFetchArtifact(r.Context(), "npm", packageName, version, filename)
	if err != nil {
		h.proxy.Logger.Error("failed to get artifact", "error", err)
		JSONError(w, http.StatusBadGateway, "failed to fetch package")
		return
	}

	ServeArtifact(w, result)
}

// extractPackageName extracts the package name from the request path.
// Handles both scoped (@scope/name) and unscoped (name) packages.
func (h *NPMHandler) extractPackageName(r *http.Request) string {
	path := strings.TrimPrefix(r.URL.Path, "/")

	// Remove /-/filename suffix if present
	if idx := strings.Index(path, "/-/"); idx >= 0 {
		path = path[:idx]
	}

	// URL decode the path (handles %40 -> @, %2f -> /)
	decoded, err := url.PathUnescape(path)
	if err != nil {
		return path
	}

	return decoded
}

// parseDownloadPath extracts package name and filename from a download path.
// Path format: /@scope/name/-/filename.tgz or /name/-/filename.tgz
func (h *NPMHandler) parseDownloadPath(path string) (packageName, filename string) {
	path = strings.TrimPrefix(path, "/")

	idx := strings.Index(path, "/-/")
	if idx < 0 {
		return "", ""
	}

	packageName = path[:idx]
	filename = path[idx+3:] // skip "/-/"

	// URL decode package name
	if decoded, err := url.PathUnescape(packageName); err == nil {
		packageName = decoded
	}

	return packageName, filename
}

// extractVersionFromFilename extracts version from npm tarball filename.
// e.g., "lodash-4.17.21.tgz" -> "4.17.21"
// e.g., "core-7.23.0.tgz" for @babel/core -> "7.23.0"
func (h *NPMHandler) extractVersionFromFilename(packageName, filename string) string {
	// Remove .tgz extension
	if !strings.HasSuffix(filename, ".tgz") {
		return ""
	}
	base := strings.TrimSuffix(filename, ".tgz")

	// For scoped packages, the filename uses the short name
	shortName := packageName
	if strings.Contains(packageName, "/") {
		parts := strings.SplitN(packageName, "/", 2)
		shortName = parts[1]
	}

	// Expected format: {shortName}-{version}
	prefix := shortName + "-"
	if !strings.HasPrefix(base, prefix) {
		return ""
	}

	return strings.TrimPrefix(base, prefix)
}
