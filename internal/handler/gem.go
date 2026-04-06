package handler

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/git-pkgs/purl"
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
	mux.HandleFunc("GET /info/{name}", h.handleCompactIndex)

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

// handleCompactIndex serves the compact index for a gem, filtering versions
// based on cooldown when enabled.
func (h *GemHandler) handleCompactIndex(w http.ResponseWriter, r *http.Request) {
	if h.proxy.Cooldown == nil || !h.proxy.Cooldown.Enabled() {
		h.proxyUpstream(w, r)
		return
	}

	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "invalid gem name", http.StatusBadRequest)
		return
	}

	h.proxy.Logger.Info("gem compact index request with cooldown", "name", name)

	indexResp, filteredVersions, err := h.fetchIndexAndVersions(r, name)
	if err != nil {
		h.proxy.Logger.Error("upstream compact index request failed", "error", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = indexResp.Body.Close() }()

	if indexResp.StatusCode != http.StatusOK {
		copyResponseHeaders(w, indexResp.Header)
		w.WriteHeader(indexResp.StatusCode)
		_, _ = io.Copy(w, indexResp.Body)
		return
	}

	if filteredVersions == nil {
		h.proxy.Logger.Warn("failed to fetch version timestamps, proxying unfiltered", "name", name)
		copyResponseHeaders(w, indexResp.Header)
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, indexResp.Body)
		return
	}

	h.writeFilteredIndex(w, indexResp, name, filteredVersions)
}

// fetchIndexAndVersions fetches the compact index and versions API concurrently.
// Returns the index response, a set of versions to filter (nil if versions API failed),
// and an error if the index fetch itself failed.
func (h *GemHandler) fetchIndexAndVersions(r *http.Request, name string) (*http.Response, map[string]bool, error) {
	type versionsResult struct {
		filtered map[string]bool
		err      error
	}

	versionsCh := make(chan versionsResult, 1)
	go func() {
		filtered, err := h.fetchFilteredVersions(r, name)
		versionsCh <- versionsResult{filtered: filtered, err: err}
	}()

	indexResp, err := h.fetchCompactIndex(r, name)

	versionsRes := <-versionsCh

	if err != nil {
		return nil, nil, err
	}

	if versionsRes.err != nil {
		return indexResp, nil, nil
	}

	return indexResp, versionsRes.filtered, nil
}

// fetchCompactIndex fetches the compact index from upstream.
func (h *GemHandler) fetchCompactIndex(r *http.Request, name string) (*http.Response, error) {
	indexURL := h.upstreamURL + "/info/" + name
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, indexURL, nil)
	if err != nil {
		return nil, err
	}
	for _, hdr := range []string{"Accept", "Accept-Encoding", "If-None-Match", "If-Modified-Since"} {
		if v := r.Header.Get(hdr); v != "" {
			req.Header.Set(hdr, v)
		}
	}
	return h.proxy.HTTPClient.Do(req)
}

// writeFilteredIndex writes the compact index response with cooldown-filtered versions removed.
func (h *GemHandler) writeFilteredIndex(w http.ResponseWriter, resp *http.Response, name string, filtered map[string]bool) {
	for k, vv := range resp.Header {
		if strings.EqualFold(k, "Content-Length") {
			continue // length will change after filtering
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(http.StatusOK)

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()

		if line == "---" {
			_, _ = fmt.Fprintln(w, line)
			continue
		}

		version := line
		if spaceIdx := strings.IndexByte(line, ' '); spaceIdx > 0 {
			version = line[:spaceIdx]
		}

		if filtered[version] {
			h.proxy.Logger.Info("cooldown: filtering gem version",
				"gem", name, "version", version)
			continue
		}

		_, _ = fmt.Fprintln(w, line)
	}
}

// copyResponseHeaders copies HTTP headers from a response to a writer.
func copyResponseHeaders(w http.ResponseWriter, headers http.Header) {
	for k, vv := range headers {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
}

// gemVersion represents a version entry from the RubyGems versions API.
type gemVersion struct {
	Number    string `json:"number"`
	Platform  string `json:"platform"`
	CreatedAt string `json:"created_at"`
}

// fetchFilteredVersions fetches the versions API and returns a set of version
// strings that should be filtered out by cooldown.
func (h *GemHandler) fetchFilteredVersions(r *http.Request, name string) (map[string]bool, error) {
	versionsURL := fmt.Sprintf("%s/api/v1/versions/%s.json", h.upstreamURL, name)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, versionsURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := h.proxy.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("versions API returned %d", resp.StatusCode)
	}

	var versions []gemVersion
	if err := json.NewDecoder(resp.Body).Decode(&versions); err != nil {
		return nil, err
	}

	packagePURL := purl.MakePURLString("gem", name, "")
	filtered := make(map[string]bool)

	for _, v := range versions {
		createdAt, err := time.Parse(time.RFC3339, v.CreatedAt)
		if err != nil {
			continue
		}

		if !h.proxy.Cooldown.IsAllowed("gem", packagePURL, createdAt) {
			// Build version string matching compact index format
			versionStr := v.Number
			if v.Platform != "" && v.Platform != "ruby" {
				versionStr = v.Number + "-" + v.Platform
			}
			filtered[versionStr] = true
		}
	}

	return filtered, nil
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

	resp, err := h.proxy.HTTPClient.Do(req)
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
