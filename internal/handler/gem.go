package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/git-pkgs/proxy/internal/retention"
)

const (
	gemUpstream = "https://rubygems.org"
)

// Gems are stored under pkg:gem/{name} with the name from the gem filename,
// and purl keeps gem names as they are, so package rules use the default
// key mapping. The spec also claims the "rubygems" spelling git-pkgs writes
// into packages.ecosystem.
func init() {
	retention.Default.Register(retention.Spec{Key: "gem", CanonicalPackage: retention.DefaultCanonical("gem")})
}

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

// NewGemHandlerWithUpstream creates a RubyGems handler with a custom upstream.
func NewGemHandlerWithUpstream(proxy *Proxy, proxyURL, upstreamURL string) *GemHandler {
	h := NewGemHandler(proxy, proxyURL)
	h.upstreamURL = configuredUpstreamURL(upstreamURL, gemUpstream)
	return h
}

// Routes returns the HTTP handler for RubyGems requests.
func (h *GemHandler) Routes() http.Handler {
	mux := http.NewServeMux()

	// Gem downloads
	mux.HandleFunc("GET /gems/{filename}", h.handleDownload)

	// Specs indexes (compressed Ruby Marshal format)
	mux.HandleFunc("GET /specs.4.8.gz", h.proxyCached)
	mux.HandleFunc("GET /latest_specs.4.8.gz", h.proxyCached)
	mux.HandleFunc("GET /prerelease_specs.4.8.gz", h.proxyCached)

	// Compact index (bundler 2.x+)
	mux.HandleFunc("GET /versions", h.proxyCached)
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
	h.proxy.handleFilenameDownload(w, r, filenameDownload{
		ecosystem:   "gem",
		upstreamURL: h.upstreamURL,
		suffix:      ".gem",
		parseErr:    "could not parse gem filename",
		fetchErr:    "failed to fetch gem",
		parse:       h.parseGemFilename,
	})
}

// parseGemFilename extracts name and version from a gem filename.
// e.g., "rails-7.1.0.gem" -> ("rails", "7.1.0")
// e.g., "aws-sdk-s3-1.142.0.gem" -> ("aws-sdk-s3", "1.142.0")
// e.g., "sorbet-static-0.5.11002-universal-darwin-22.gem" -> ("sorbet-static", "0.5.11002-universal-darwin-22")
// e.g., "devise-2fa-1.0.0.gem" -> ("devise-2fa", "1.0.0")
//
// A gem file is named {name}-{version}[-{platform}]. RubyGems writes a
// version as dot-separated alphanumeric segments starting with a digit,
// turning any "-" into ".pre.", and a platform starts with a letter (its
// CPU, or java), though it can end in a number (universal-java-17), so
// splitting at the last "-<digit>" would cut the platform. The version
// starts at the first "-<digit>" that leaves such a version and platform,
// so a name with a "-<digit>" segment of its own, such as devise-2fa, is
// kept whole. A filename no split fits is split at the first "-<digit>".
//
// Trying every "-<digit>" costs time quadratic in the filename's length, so
// a filename longer than maxGemFilenameLength is refused.
func (h *GemHandler) parseGemFilename(filename string) (name, version string) {
	if len(filename) > maxGemFilenameLength {
		return "", ""
	}
	base := strings.TrimSuffix(filename, ".gem")
	first := -1
	for i := 0; i+1 < len(base); i++ {
		if base[i] != '-' || base[i+1] < '0' || base[i+1] > '9' {
			continue
		}
		if first < 0 {
			first = i
		}
		if gemVersionAndPlatform(base[i+1:]) {
			return base[:i], base[i+1:]
		}
	}
	if first < 0 {
		return "", ""
	}
	return base[:first], base[first+1:]
}

// maxGemFilenameLength bounds the gem filenames parseGemFilename accepts.
// Real gem filenames are a small fraction of this.
const maxGemFilenameLength = 512

var (
	gemVersionPattern  = regexp.MustCompile(`^[0-9]+(\.[0-9A-Za-z]+)*$`)
	gemPlatformPattern = regexp.MustCompile(`^[A-Za-z][0-9A-Za-z_]*(-[0-9A-Za-z_]+)*$`)
)

// gemVersionAndPlatform reports whether s reads as {version}[-{platform}].
func gemVersionAndPlatform(s string) bool {
	version, platform, hasPlatform := strings.Cut(s, "-")
	return gemVersionPattern.MatchString(version) && (!hasPlatform || gemPlatformPattern.MatchString(platform))
}

// handleCompactIndex serves the compact index for a gem, filtering versions
// based on cooldown when enabled.
func (h *GemHandler) handleCompactIndex(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "invalid gem name", http.StatusBadRequest)
		return
	}
	if !h.proxy.cooldownApplies("gem", name) {
		h.proxyCached(w, r)
		return
	}

	h.proxy.Logger.Info("gem compact index request with cooldown", "name", name)

	indexResp, filteredVersions, versionsErr, err := h.fetchIndexAndVersions(r, name)
	if err != nil {
		if writeUpstreamHeld(w, err) {
			return
		}
		h.proxy.Logger.Error("upstream compact index request failed", "error", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = indexResp.Body.Close() }()

	if indexResp.StatusCode != http.StatusOK {
		h.proxy.relayResponse(w, r, indexResp, nil)
		return
	}

	if versionsErr != nil {
		h.proxy.writeCooldownLookupError(w, "gem", name, versionsErr)
		return
	}

	h.writeFilteredIndex(w, indexResp, name, filteredVersions)
}

// fetchIndexAndVersions fetches the compact index and versions API
// concurrently. It returns the index response and the set of versions to
// filter, the versions API's error, and the index fetch's error.
func (h *GemHandler) fetchIndexAndVersions(r *http.Request, name string) (*http.Response, map[string]bool, error, error) {
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
		return nil, nil, nil, err
	}
	return indexResp, versionsRes.filtered, versionsRes.err, nil
}

// fetchCompactIndex fetches the compact index from upstream.
func (h *GemHandler) fetchCompactIndex(r *http.Request, name string) (*http.Response, error) {
	indexURL := h.upstreamURL + "/info/" + name
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, indexURL, nil)
	if err != nil {
		return nil, err
	}
	// Only Accept is passed on. The client's Accept-Encoding would turn off
	// the transport's transparent decompression and leave a gzip body to
	// filter, and its validators belong to the filtered copy it was served,
	// not to upstream's file.
	if v := r.Header.Get(headerAccept); v != "" {
		req.Header.Set(headerAccept, v)
	}
	return h.proxy.HTTPClient.Do(req)
}

// gemFilteredIndexDroppedHeaders describe upstream's file as a whole, so
// they are wrong for a copy with versions removed. Bundler checks the digest
// headers against the body it receives.
var gemFilteredIndexDroppedHeaders = []string{
	headerContentLength, headerContentEncoding, headerETag, headerLastModified,
	"Digest", "Repr-Digest", "Content-Digest", "Accept-Ranges", "Content-Range",
}

// writeFilteredIndex writes the compact index response with cooldown-filtered
// versions removed. The body is read in full first, so a read error or an
// oversized index answers 502 instead of a truncated 200.
func (h *GemHandler) writeFilteredIndex(w http.ResponseWriter, resp *http.Response, name string, filtered map[string]bool) {
	body, err := h.proxy.ReadMetadata(resp.Body)
	if err != nil {
		h.proxy.Logger.Error("failed to read gem compact index", "gem", name, "error", err)
		http.Error(w, "failed to read upstream response", http.StatusBadGateway)
		return
	}

	var out bytes.Buffer
	out.Grow(len(body))
	for rest := body; len(rest) > 0; {
		line, tail, _ := bytes.Cut(rest, []byte("\n"))
		rest = tail

		version := line
		if spaceIdx := bytes.IndexByte(line, ' '); spaceIdx > 0 {
			version = line[:spaceIdx]
		}
		if filtered[string(version)] {
			h.proxy.Logger.Info("cooldown: filtering gem version",
				"gem", name, "version", string(version))
			continue
		}

		out.Write(line)
		out.WriteByte('\n')
	}

	for k, vv := range resp.Header {
		if slices.ContainsFunc(gemFilteredIndexDroppedHeaders, func(d string) bool { return strings.EqualFold(k, d) }) {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set(headerContentLength, strconv.Itoa(out.Len()))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out.Bytes())
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

	body, err := h.proxy.ReadMetadata(resp.Body)
	if err != nil {
		return nil, err
	}
	var versions []gemVersion
	if err := json.Unmarshal(body, &versions); err != nil {
		return nil, err
	}

	packagePURL := canonicalPackagePURL("gem", name)
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

// proxyCached forwards a metadata request with caching.
func (h *GemHandler) proxyCached(w http.ResponseWriter, r *http.Request) {
	upstreamURL := h.upstreamURL + r.URL.Path
	cacheKey := strings.TrimPrefix(r.URL.Path, "/")
	h.proxy.ProxyCached(w, r, upstreamURL, "gem", cacheKey, "*/*")
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
	for _, h := range []string{"Accept", headerAcceptEncoding, "If-None-Match", "If-Modified-Since"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}

	resp, err := h.proxy.HTTPClient.Do(req)
	if err != nil {
		if writeUpstreamHeld(w, err) {
			return
		}
		h.proxy.Logger.Error("upstream request failed", "error", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	h.proxy.relayResponse(w, r, resp, nil)
}

func init() {
	// Register gem URL pattern with a simple URL builder
	_ = fmt.Sprintf // silence import if unused
}
