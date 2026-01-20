package handler

import (
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	gemUpstream = "https://rubygems.org"
)

// GemHandler handles RubyGems registry protocol requests.
type GemHandler struct {
	proxy       *Proxy
	upstreamURL string
	proxyURL    string
}

// NewGemHandler creates a new RubyGems protocol handler.
func NewGemHandler(proxy *Proxy, proxyURL string) *GemHandler {
	return &GemHandler{
		proxy:       proxy,
		upstreamURL: gemUpstream,
		proxyURL:    strings.TrimSuffix(proxyURL, "/"),
	}
}

// Routes returns the HTTP handler for RubyGems requests.
func (h *GemHandler) Routes() http.Handler {
	mux := http.NewServeMux()

	// Gem downloads
	mux.HandleFunc("GET /gems/{filename}", h.handleDownload)

	// Specs indexes (compressed Ruby Marshal format)
	mux.HandleFunc("GET /specs.4.8.gz", h.proxyUpstream)
	mux.HandleFunc("GET /latest_specs.4.8.gz", h.proxyUpstream)
	mux.HandleFunc("GET /prerelease_specs.4.8.gz", h.proxyUpstream)

	// Compact index (bundler 2.x+)
	mux.HandleFunc("GET /versions", h.proxyUpstream)
	mux.HandleFunc("GET /info/{name}", h.proxyUpstream)

	// Quick index
	mux.HandleFunc("GET /quick/Marshal.4.8/{filename}", h.proxyUpstream)

	// API endpoints - use catch-all since {name}.json pattern isn't allowed
	mux.HandleFunc("GET /api/v1/gems/", h.proxyUpstream)
	mux.HandleFunc("GET /api/v1/dependencies", h.proxyUpstream)

	return mux
}

// handleDownload serves a gem file, fetching and caching from upstream if needed.
func (h *GemHandler) handleDownload(w http.ResponseWriter, r *http.Request) {
	filename := r.PathValue("filename")
	if filename == "" || !strings.HasSuffix(filename, ".gem") {
		http.Error(w, "invalid filename", http.StatusBadRequest)
		return
	}

	// Extract name and version from filename (e.g., "rails-7.1.0.gem")
	name, version := h.parseGemFilename(filename)
	if name == "" || version == "" {
		http.Error(w, "could not parse gem filename", http.StatusBadRequest)
		return
	}

	h.proxy.Logger.Info("gem download request",
		"name", name, "version", version, "filename", filename)

	result, err := h.proxy.GetOrFetchArtifact(r.Context(), "gem", name, version, filename)
	if err != nil {
		h.proxy.Logger.Error("failed to get artifact", "error", err)
		http.Error(w, "failed to fetch gem", http.StatusBadGateway)
		return
	}

	ServeArtifact(w, result)
}

// parseGemFilename extracts name and version from a gem filename.
// e.g., "rails-7.1.0.gem" -> ("rails", "7.1.0")
// e.g., "aws-sdk-s3-1.142.0.gem" -> ("aws-sdk-s3", "1.142.0")
func (h *GemHandler) parseGemFilename(filename string) (name, version string) {
	// Remove .gem extension
	base := strings.TrimSuffix(filename, ".gem")

	// Find the last hyphen followed by a version number
	// Version starts with a digit
	for i := len(base) - 1; i >= 0; i-- {
		if base[i] == '-' && i+1 < len(base) && base[i+1] >= '0' && base[i+1] <= '9' {
			return base[:i], base[i+1:]
		}
	}
	return "", ""
}

// proxyUpstream forwards a request to rubygems.org without caching.
func (h *GemHandler) proxyUpstream(w http.ResponseWriter, r *http.Request) {
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

	// Copy relevant headers
	for _, h := range []string{"Accept", "Accept-Encoding", "If-None-Match", "If-Modified-Since"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.proxy.Logger.Error("upstream request failed", "error", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Copy response headers
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}

	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func init() {
	// Register gem URL pattern with a simple URL builder
	_ = fmt.Sprintf // silence import if unused
}
