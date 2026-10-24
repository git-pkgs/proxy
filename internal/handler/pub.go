package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/retention"
)

const (
	pubUpstream   = "https://pub.dev"
	pubPathParts  = 2 // name + version in path split by /versions/
	pubArchiveExt = ".tar.gz"
)

// pubPackageName matches pub package names, which have to be Dart identifiers.
var pubPackageName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Packages are stored under pkg:pub/{name} with the name from the download
// path, and purl keeps pub names as they are, so package rules use the
// default key mapping.
func init() {
	retention.Default.Register(retention.Spec{Key: "pub", CanonicalPackage: retention.DefaultCanonical("pub")})
}

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

// NewPubHandlerWithUpstream creates a pub handler with a custom upstream.
func NewPubHandlerWithUpstream(proxy *Proxy, proxyURL, upstreamURL string) *PubHandler {
	h := NewPubHandler(proxy, proxyURL)
	h.upstreamURL = configuredUpstreamURL(upstreamURL, pubUpstream)
	return h
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
	// Only the archive is cached. Without the suffix the upstream path is the
	// HTML version page on pub.dev, which must not land under the archive key.
	version, ok := strings.CutSuffix(parts[1], pubArchiveExt)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	if !pubPackageName.MatchString(name) || version == "" || strings.Contains(version, "/") {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	filename := name + "-" + version + pubArchiveExt

	h.proxy.Logger.Info("pub download request",
		"name", name, "version", version)

	downloadURL := fmt.Sprintf("%s/packages/%s/versions/%s%s",
		h.upstreamURL, url.PathEscape(name), url.PathEscape(version), pubArchiveExt)
	h.discardVersionPage(name, version, filename)
	result, err := h.proxy.GetOrFetchArtifactFromURL(
		r.Context(), "pub", name, version, filename, downloadURL,
	)
	if err != nil {
		h.proxy.serveArtifactError(w, err, "failed to fetch package")
		return
	}

	ServeArtifactRequest(w, r, result)
}

// discardVersionPage clears a cached archive that was fetched from a path
// without the archive suffix. Proxy versions before the suffix check cached
// pub.dev's HTML version page under the archive's name that way.
func (h *PubHandler) discardVersionPage(name, version, filename string) {
	_, versionPURL, err := packagePURLStrings("pub", name, version)
	if err != nil {
		return
	}
	record, err := h.proxy.DB.GetArtifact(versionPURL, filename)
	if err != nil {
		h.proxy.Logger.Warn("failed to read pub cache record",
			"purl", versionPURL, "filename", filename, "error", err)
		return
	}
	if record == nil || !record.StoragePath.Valid || record.Source == database.SourceLocal ||
		strings.HasSuffix(record.UpstreamURL, pubArchiveExt) {
		return
	}
	h.proxy.Logger.Warn("cached pub archive was fetched without the archive suffix, discarding",
		"purl", versionPURL, "filename", filename, "upstream_url", record.UpstreamURL)
	if err := h.proxy.DB.DiscardArtifact(versionPURL, filename, record.StoragePath.String); err != nil {
		h.proxy.Logger.Warn("failed to discard pub cache record",
			"purl", versionPURL, "filename", filename, "error", err)
	}
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
		if writeUpstreamHeld(w, err) {
			return
		}
		h.proxy.Logger.Error("upstream request failed", "error", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}

	rewritten, err := h.rewriteMetadata(name, body)
	if err != nil {
		if h.proxy.mustFilterMetadata("pub", name) {
			h.proxy.Logger.Error("failed to filter pub metadata", "package", name, "error", err)
			http.Error(w, "failed to filter package metadata", http.StatusBadGateway)
			return
		}
		h.proxy.Logger.Warn("failed to rewrite metadata, proxying original", "error", err)
		w.Header().Set(headerContentType, "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return
	}

	w.Header().Set(headerContentType, "application/json")
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

	packagePURL := canonicalPackagePURL("pub", name)
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
