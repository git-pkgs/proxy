package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/git-pkgs/purl"
)

const (
	pubUpstream  = "https://pub.dev"
	pubPathParts = 2 // name + version in path split by /versions/
)

// PubHandler handles pub.dev registry protocol requests.
type PubHandler struct {
	proxy       *Proxy
	upstreamURL string
	proxyURL    string
}

// NewPubHandler creates a new pub.dev protocol handler.
func NewPubHandler(proxy *Proxy, proxyURL string) *PubHandler {
	return &PubHandler{
		proxy:       proxy,
		upstreamURL: pubUpstream,
		proxyURL:    strings.TrimSuffix(proxyURL, "/"),
	}
}

// Routes returns the HTTP handler for pub requests.
func (h *PubHandler) Routes() http.Handler {
	mux := http.NewServeMux()

	// Package downloads (cache these) - use prefix since {version}.tar.gz isn't allowed
	mux.HandleFunc("GET /packages/", h.handleDownload)

	// API endpoints (proxy with URL rewriting)
	mux.HandleFunc("GET /api/packages/{name}", h.handlePackageMetadata)

	return mux
}

// handleDownload serves a package tarball, fetching and caching from upstream if needed.
func (h *PubHandler) handleDownload(w http.ResponseWriter, r *http.Request) {
	// Parse path: /packages/{name}/versions/{version}.tar.gz
	path := strings.TrimPrefix(r.URL.Path, "/packages/")
	parts := strings.Split(path, "/versions/")
	if len(parts) != pubPathParts {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	name := parts[0]
	version := strings.TrimSuffix(parts[1], ".tar.gz")

	if name == "" || version == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	filename := fmt.Sprintf("%s-%s.tar.gz", name, version)

	h.proxy.Logger.Info("pub download request",
		"name", name, "version", version)

	result, err := h.proxy.GetOrFetchArtifact(r.Context(), "pub", name, version, filename)
	if err != nil {
		h.proxy.Logger.Error("failed to get artifact", "error", err)
		http.Error(w, "failed to fetch package", http.StatusBadGateway)
		return
	}

	ServeArtifact(w, result)
}

// handlePackageMetadata proxies package metadata and rewrites archive URLs.
func (h *PubHandler) handlePackageMetadata(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "invalid package name", http.StatusBadRequest)
		return
	}

	h.proxy.Logger.Info("pub metadata request", "package", name)

	upstreamURL := fmt.Sprintf("%s/api/packages/%s", h.upstreamURL, name)

	body, _, err := h.proxy.FetchOrCacheMetadata(r.Context(), "pub", name, upstreamURL)
	if err != nil {
		if errors.Is(err, ErrUpstreamNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		h.proxy.Logger.Error("upstream request failed", "error", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}

	rewritten, err := h.rewriteMetadata(name, body)
	if err != nil {
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

// rewriteMetadata rewrites archive_url fields to point at this proxy.
// If cooldown is enabled, versions published too recently are filtered out.
func (h *PubHandler) rewriteMetadata(name string, body []byte) ([]byte, error) {
	var metadata map[string]any
	if err := json.Unmarshal(body, &metadata); err != nil {
		return nil, err
	}

	versions, ok := metadata["versions"].([]any)
	if !ok {
		return body, nil
	}

	packagePURL := purl.MakePURLString("pub", name, "")
	filtered := h.filterAndRewriteVersions(name, packagePURL, versions)
	metadata["versions"] = filtered

	h.updateLatestVersion(metadata, filtered)

	return json.Marshal(metadata)
}

// filterAndRewriteVersions applies cooldown filtering and rewrites archive URLs
// for a package's version list.
func (h *PubHandler) filterAndRewriteVersions(name, packagePURL string, versions []any) []any {
	filtered := versions[:0]
	for _, vdata := range versions {
		vmap, ok := vdata.(map[string]any)
		if !ok {
			continue
		}

		version, _ := vmap["version"].(string)
		if version == "" {
			continue
		}

		if h.shouldFilterVersion(packagePURL, name, version, vmap) {
			continue
		}

		newURL := fmt.Sprintf("%s/pub/packages/%s/versions/%s.tar.gz", h.proxyURL, name, version)
		vmap["archive_url"] = newURL
		filtered = append(filtered, vdata)

		h.proxy.Logger.Debug("rewrote archive URL",
			"package", name, "version", version, "new", newURL)
	}

	return filtered
}

// shouldFilterVersion returns true if the version should be excluded due to cooldown.
func (h *PubHandler) shouldFilterVersion(packagePURL, name, version string, vmap map[string]any) bool {
	if h.proxy.Cooldown == nil || !h.proxy.Cooldown.Enabled() {
		return false
	}

	publishedStr, ok := vmap["published"].(string)
	if !ok {
		return false
	}

	publishedAt, err := time.Parse(time.RFC3339, publishedStr)
	if err != nil {
		return false
	}

	if !h.proxy.Cooldown.IsAllowed("pub", packagePURL, publishedAt) {
		h.proxy.Logger.Info("cooldown: filtering pub version",
			"package", name, "version", version)
		return true
	}

	return false
}

// updateLatestVersion updates the latest field if the current latest version
// was removed by cooldown filtering.
func (h *PubHandler) updateLatestVersion(metadata map[string]any, filtered []any) {
	if h.proxy.Cooldown == nil || !h.proxy.Cooldown.Enabled() {
		return
	}

	latest, ok := metadata["latest"].(map[string]any)
	if !ok {
		return
	}

	latestVer, ok := latest["version"].(string)
	if !ok {
		return
	}

	for _, vdata := range filtered {
		if vmap, ok := vdata.(map[string]any); ok {
			if vmap["version"] == latestVer {
				return
			}
		}
	}

	if len(filtered) > 0 {
		metadata["latest"] = filtered[len(filtered)-1]
	}
}
