package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/git-pkgs/purl"
)

const (
	pubUpstream = "https://pub.dev"
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
	if len(parts) != 2 {
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

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Accept", "application/json")

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

	body, err := ReadMetadata(resp.Body)
	if err != nil {
		http.Error(w, "failed to read response", http.StatusInternalServerError)
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

	// Rewrite archive URLs in versions
	versions, ok := metadata["versions"].([]any)
	if !ok {
		return body, nil
	}

	packagePURL := purl.MakePURLString("pub", name, "")

	// Filter and rewrite versions
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

		// Apply cooldown filtering
		if h.proxy.Cooldown != nil && h.proxy.Cooldown.Enabled() {
			if publishedStr, ok := vmap["published"].(string); ok {
				if publishedAt, err := time.Parse(time.RFC3339, publishedStr); err == nil {
					if !h.proxy.Cooldown.IsAllowed("pub", packagePURL, publishedAt) {
						h.proxy.Logger.Info("cooldown: filtering pub version",
							"package", name, "version", version)
						continue
					}
				}
			}
		}

		// Rewrite archive_url
		newURL := fmt.Sprintf("%s/pub/packages/%s/versions/%s.tar.gz", h.proxyURL, name, version)
		vmap["archive_url"] = newURL
		filtered = append(filtered, vdata)

		h.proxy.Logger.Debug("rewrote archive URL",
			"package", name, "version", version, "new", newURL)
	}

	metadata["versions"] = filtered

	// Update latest if it points to a filtered version
	if h.proxy.Cooldown != nil && h.proxy.Cooldown.Enabled() {
		if latest, ok := metadata["latest"].(map[string]any); ok {
			if latestVer, ok := latest["version"].(string); ok {
				found := false
				for _, vdata := range filtered {
					if vmap, ok := vdata.(map[string]any); ok {
						if vmap["version"] == latestVer {
							found = true
							break
						}
					}
				}
				if !found && len(filtered) > 0 {
					// Use the last entry (most recent remaining)
					metadata["latest"] = filtered[len(filtered)-1]
				}
			}
		}
	}

	return json.Marshal(metadata)
}
