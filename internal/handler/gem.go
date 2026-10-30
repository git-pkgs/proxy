package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
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
//
// With cooldown the index and the versions API's publish times are read
// through the metadata cache, so a bundle install within metadata_ttl of the
// last one asks rubygems.org nothing, and one after it revalidates both with
// their ETags. The filtered index is kept in the rewrite cache and served
// with an ETag of its own.
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

	docs, err := h.proxy.fetchTimedMetadata(r.Context(), h.timedIndex(name), h.indexVersions)
	switch {
	case docs.doc.err != nil:
		h.proxy.writeMetadataFetchError(w, docs.doc.err)
		return
	case docs.times.err != nil:
		h.proxy.writeCooldownLookupError(w, "gem", name, docs.times.err)
		return
	case err != nil:
		h.proxy.Logger.Error("failed to read gem compact index", "gem", name, "error", err)
		http.Error(w, "failed to read upstream response", http.StatusBadGateway)
		return
	}

	packagePURL := canonicalPackagePURL("gem", name)
	m, err := h.proxy.cachedRewrite(r.Context(), "gem", h.proxyURL, "info/"+name, docs.doc.body, docs.rewriteDigest(),
		func([]byte) ([]byte, []string, error) {
			body, err := h.proxy.decodedMetadataBody(docs.doc)
			if err != nil {
				return nil, nil, err
			}
			out, kept := h.filterIndex(body, name, func(version string) bool {
				return docs.withheld(h.proxy, "gem", packagePURL, version)
			})
			return out, kept, nil
		})
	if err != nil {
		h.proxy.Logger.Error("failed to filter gem compact index", "gem", name, "error", err)
		http.Error(w, "failed to read upstream response", http.StatusBadGateway)
		return
	}

	// Served as identity, as upstream's index was: Bundler checks digest
	// headers against the bytes it receives, and asks for ranges of it.
	m = m.identity()
	contentType := docs.doc.contentType
	if contentType == "" || contentType == contentTypeJSON {
		contentType = "text/plain; charset=utf-8"
	}
	markStale(w, docs.doc.stale || docs.times.stale)
	serveRewrittenMetadataAs(w, r, m, contentType)
}

// timedIndex describes a gem's compact index and the versions API it takes
// publish times from.
func (h *GemHandler) timedIndex(name string) timedMetadata {
	return timedMetadata{
		ecosystem:   "gem",
		docKey:      "info/" + name,
		docURL:      h.upstreamURL + "/info/" + name,
		timesKey:    "api/v1/versions/" + name + ".json",
		timesURL:    fmt.Sprintf("%s/api/v1/versions/%s.json", h.upstreamURL, name),
		timesAccept: contentTypeJSON,
		parseTimes:  parseGemVersions,
	}
}

// indexVersions lists the versions in a compact index, as the versions API
// names them.
func (h *GemHandler) indexVersions(doc metadataResult) ([]string, error) {
	body, err := h.proxy.decodedMetadataBody(doc)
	if err != nil {
		return nil, err
	}
	var versions []string
	for rest := body; len(rest) > 0; {
		line, tail, _ := bytes.Cut(rest, []byte("\n"))
		rest = tail
		if version := gemIndexLineVersion(line); gemIndexListsVersion(version) {
			versions = append(versions, string(version))
		}
	}
	return versions, nil
}

// gemIndexLineVersion returns the version a compact index line starts with.
func gemIndexLineVersion(line []byte) []byte {
	if spaceIdx := bytes.IndexByte(line, ' '); spaceIdx > 0 {
		return line[:spaceIdx]
	}
	return line
}

// gemIndexListsVersion reports whether a compact index line's first field
// is a version, rather than the "---" that starts the file or an empty line.
func gemIndexListsVersion(field []byte) bool {
	return len(field) > 0 && string(field) != "---"
}

// filterIndex returns the compact index without the lines of versions
// withheld reports, and the versions it kept.
func (h *GemHandler) filterIndex(body []byte, name string, withheld func(string) bool) ([]byte, []string) {
	var kept []string
	var out bytes.Buffer
	out.Grow(len(body))
	for rest := body; len(rest) > 0; {
		line, tail, _ := bytes.Cut(rest, []byte("\n"))
		rest = tail

		version := gemIndexLineVersion(line)
		if gemIndexListsVersion(version) {
			if withheld(string(version)) {
				h.proxy.Logger.Info("cooldown: filtering gem version",
					"gem", name, "version", string(version))
				continue
			}
			kept = append(kept, string(version))
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	return out.Bytes(), kept
}

// gemVersion represents a version entry from the RubyGems versions API.
type gemVersion struct {
	Number    string `json:"number"`
	Platform  string `json:"platform"`
	CreatedAt string `json:"created_at"`
}

// parseGemVersions reads the publish times from a versions API response,
// keyed by version as the compact index writes it.
func parseGemVersions(body []byte) (publishTimes, error) {
	var versions []gemVersion
	if err := json.Unmarshal(body, &versions); err != nil {
		return nil, err
	}
	times := make(publishTimes, len(versions))
	for _, v := range versions {
		versionStr := v.Number
		if v.Platform != "" && v.Platform != "ruby" {
			versionStr = v.Number + "-" + v.Platform
		}
		createdAt, err := time.Parse(time.RFC3339, v.CreatedAt)
		if err != nil {
			createdAt = time.Time{}
		}
		times[versionStr] = createdAt
	}
	return times, nil
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
