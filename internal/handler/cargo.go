package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	cargoUpstream     = "https://index.crates.io"
	cargoDownloadBase = "https://static.crates.io/crates"
)

// CargoHandler handles cargo registry protocol requests.
type CargoHandler struct {
	proxy       *Proxy
	indexURL    string
	downloadURL string
	proxyURL    string
}

// NewCargoHandler creates a new cargo protocol handler.
func NewCargoHandler(proxy *Proxy, proxyURL string) *CargoHandler {
	return &CargoHandler{
		proxy:       proxy,
		indexURL:    cargoUpstream,
		downloadURL: cargoDownloadBase,
		proxyURL:    strings.TrimSuffix(proxyURL, "/"),
	}
}

// Routes returns the HTTP handler for cargo requests.
// Mount this at /cargo on your router.
func (h *CargoHandler) Routes() http.Handler {
	mux := http.NewServeMux()

	// Config endpoint
	mux.HandleFunc("GET /config.json", h.handleConfig)

	// Sparse index endpoints
	// Crate names 1-2 chars: /1/{name} or /2/{name}
	// Crate names 3 chars: /3/{first_char}/{name}
	// Crate names 4+ chars: /{first_two}/{second_two}/{name}
	mux.HandleFunc("GET /1/{name}", h.handleIndex)
	mux.HandleFunc("GET /2/{name}", h.handleIndex)
	mux.HandleFunc("GET /3/{a}/{name}", h.handleIndex)
	mux.HandleFunc("GET /{a}/{b}/{name}", h.handleIndex)

	// Download endpoint
	mux.HandleFunc("GET /crates/{name}/{version}/download", h.handleDownload)

	return mux
}

// CargoConfig is the registry configuration returned by config.json.
type CargoConfig struct {
	DL string `json:"dl"`
	API string `json:"api,omitempty"`
}

// handleConfig returns the registry configuration.
func (h *CargoHandler) handleConfig(w http.ResponseWriter, r *http.Request) {
	config := CargoConfig{
		DL: h.proxyURL + "/cargo/crates/{crate}/{version}/download",
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(config)
}

// handleIndex proxies the crate index from upstream.
func (h *CargoHandler) handleIndex(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	h.proxy.Logger.Info("cargo index request", "crate", name)

	// Build the index path
	indexPath := h.buildIndexPath(name)
	upstreamURL := fmt.Sprintf("%s/%s", h.indexURL, indexPath)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.proxy.Logger.Error("failed to fetch upstream index", "error", err)
		http.Error(w, "failed to fetch from upstream", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if resp.StatusCode != http.StatusOK {
		http.Error(w, fmt.Sprintf("upstream returned %d", resp.StatusCode), http.StatusBadGateway)
		return
	}

	// Copy headers and body
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if etag := resp.Header.Get("ETag"); etag != "" {
		w.Header().Set("ETag", etag)
	}
	if lastMod := resp.Header.Get("Last-Modified"); lastMod != "" {
		w.Header().Set("Last-Modified", lastMod)
	}

	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, resp.Body)
}

// buildIndexPath builds the sparse index path for a crate name.
func (h *CargoHandler) buildIndexPath(name string) string {
	name = strings.ToLower(name)

	switch len(name) {
	case 1:
		return fmt.Sprintf("1/%s", name)
	case 2:
		return fmt.Sprintf("2/%s", name)
	case 3:
		return fmt.Sprintf("3/%c/%s", name[0], name)
	default:
		return fmt.Sprintf("%s/%s/%s", name[0:2], name[2:4], name)
	}
}

// handleDownload serves a crate file, fetching and caching from upstream if needed.
func (h *CargoHandler) handleDownload(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	version := r.PathValue("version")

	if name == "" || version == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	filename := fmt.Sprintf("%s-%s.crate", name, version)

	h.proxy.Logger.Info("cargo download request",
		"crate", name, "version", version, "filename", filename)

	result, err := h.proxy.GetOrFetchArtifact(r.Context(), "cargo", name, version, filename)
	if err != nil {
		h.proxy.Logger.Error("failed to get artifact", "error", err)
		http.Error(w, "failed to fetch crate", http.StatusBadGateway)
		return
	}

	ServeArtifact(w, result)
}
