package handler

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/git-pkgs/proxy/internal/retention"
	"google.golang.org/protobuf/encoding/protowire"
)

const (
	hexUpstream = "https://repo.hex.pm"
)

// Tarballs are stored under pkg:hex/{name} with the name from the tarball
// filename, and purl keeps hex names as they are, so package rules use the
// default key mapping.
func init() {
	retention.Default.Register(retention.Spec{Key: "hex", CanonicalPackage: retention.DefaultCanonical("hex")})
}

// HexHandler handles Hex.pm registry protocol requests.
type HexHandler struct {
	proxy       *Proxy
	upstreamURL string
	apiURL      string
	proxyURL    string
}

// NewHexHandler creates a new Hex.pm protocol handler.
func NewHexHandler(proxy *Proxy, proxyURL string) *HexHandler {
	return &HexHandler{
		proxy:       proxy,
		upstreamURL: hexUpstream,
		apiURL:      hexAPIURL,
		proxyURL:    strings.TrimSuffix(proxyURL, "/"),
	}
}

// NewHexHandlerWithUpstreams creates a Hex handler with custom repository and
// API upstreams.
func NewHexHandlerWithUpstreams(proxy *Proxy, proxyURL, upstreamURL, apiURL string) *HexHandler {
	h := NewHexHandler(proxy, proxyURL)
	h.upstreamURL = configuredUpstreamURL(upstreamURL, hexUpstream)
	h.apiURL = configuredUpstreamURL(apiURL, hexAPIURL)
	return h
}

// Routes returns the HTTP handler for Hex requests.
func (h *HexHandler) Routes() http.Handler {
	mux := http.NewServeMux()

	// Package tarballs (cache these)
	mux.HandleFunc("GET /tarballs/{filename}", h.handleDownload)

	// Registry resources (cached for offline)
	mux.HandleFunc("GET /names", h.proxyCached)
	mux.HandleFunc("GET /versions", h.proxyCached)
	mux.HandleFunc("GET /packages/{name}", h.handlePackages)

	// Public keys
	mux.HandleFunc("GET /public_key", h.proxyUpstream)

	return mux
}

// handleDownload serves a package tarball, fetching and caching from upstream if needed.
func (h *HexHandler) handleDownload(w http.ResponseWriter, r *http.Request) {
	h.proxy.handleFilenameDownload(w, r, filenameDownload{
		ecosystem:   "hex",
		upstreamURL: h.upstreamURL,
		suffix:      ".tar",
		parseErr:    "could not parse tarball filename",
		fetchErr:    "failed to fetch package",
		parse:       h.parseTarballFilename,
	})
}

// parseTarballFilename extracts name and version from a hex tarball filename.
// e.g., "phoenix-1.7.10.tar" -> ("phoenix", "1.7.10")
// e.g., "amqp_client-4.1.5-1.tar" -> ("amqp_client", "4.1.5-1")
//
// Hex package names can't contain a hyphen, so the first one separates the
// name from a version that may carry hyphens of its own.
func (h *HexHandler) parseTarballFilename(filename string) (name, version string) {
	name, version, ok := strings.Cut(strings.TrimSuffix(filename, ".tar"), "-")
	if !ok || name == "" || version == "" {
		return "", ""
	}
	return name, version
}

// hexAPIURL is the Hex HTTP API base URL for fetching package metadata with timestamps.
const hexAPIURL = "https://hex.pm"

// handlePackages proxies the /packages/{name} endpoint, applying cooldown filtering
// when enabled. Since the protobuf format has no timestamps, we fetch them from the
// Hex HTTP API concurrently.
func (h *HexHandler) handlePackages(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" || !h.proxy.cooldownApplies("hex", name) {
		h.proxyCached(w, r)
		return
	}

	h.proxy.Logger.Info("hex package request with cooldown", "name", name)

	protoResp, filteredVersions, versionsErr, err := h.fetchPackageAndVersions(r, name)
	if err != nil {
		if writeUpstreamHeld(w, err) {
			return
		}
		h.proxy.Logger.Error("upstream request failed", "error", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = protoResp.Body.Close() }()

	if protoResp.StatusCode != http.StatusOK {
		h.proxy.relayResponse(w, r, protoResp, nil)
		return
	}

	if versionsErr != nil {
		h.proxy.writeCooldownLookupError(w, "hex", name, versionsErr)
		return
	}

	body, err := h.proxy.ReadMetadata(protoResp.Body)
	if err != nil {
		h.proxy.Logger.Error("failed to read hex package", "name", name, "error", err)
		http.Error(w, "failed to read response", http.StatusBadGateway)
		return
	}

	if len(filteredVersions) == 0 {
		// No versions in cooldown, pass through
		w.Header().Set(headerContentType, protoResp.Header.Get(headerContentType))
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(body)
		return
	}

	filtered, err := h.filterSignedPackage(body, filteredVersions)
	if err != nil {
		h.proxy.Logger.Error("failed to filter hex package", "name", name, "error", err)
		http.Error(w, "failed to filter package metadata", http.StatusBadGateway)
		return
	}

	w.Header().Set(headerContentType, "application/octet-stream")
	w.Header().Set("Content-Encoding", "gzip")
	_, _ = w.Write(filtered)
}

// fetchPackageAndVersions fetches the protobuf package and version timestamps
// concurrently. It returns the package response and the set of versions to
// filter, the Hex API's error, and the package fetch's error.
func (h *HexHandler) fetchPackageAndVersions(r *http.Request, name string) (*http.Response, map[string]bool, error, error) {
	type versionsResult struct {
		filtered map[string]bool
		err      error
	}

	versionsCh := make(chan versionsResult, 1)
	go func() {
		filtered, err := h.fetchFilteredVersions(r, name)
		versionsCh <- versionsResult{filtered: filtered, err: err}
	}()

	protoResp, err := h.fetchUpstreamPackage(r, name)

	versionsRes := <-versionsCh

	if err != nil {
		return nil, nil, nil, err
	}
	return protoResp, versionsRes.filtered, versionsRes.err, nil
}

// fetchUpstreamPackage fetches the protobuf package from upstream.
func (h *HexHandler) fetchUpstreamPackage(r *http.Request, name string) (*http.Response, error) {
	upstreamURL := h.upstreamURL + "/packages/" + name
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		return nil, err
	}
	return h.proxy.HTTPClient.Do(req)
}

// hexRelease represents a version entry from the Hex API.
type hexRelease struct {
	Version    string `json:"version"`
	InsertedAt string `json:"inserted_at"`
}

// hexPackageAPI represents the Hex API response for a package.
type hexPackageAPI struct {
	Releases []hexRelease `json:"releases"`
}

// fetchFilteredVersions fetches the Hex API and returns a set of version
// strings that should be filtered out by cooldown.
func (h *HexHandler) fetchFilteredVersions(r *http.Request, name string) (map[string]bool, error) {
	apiURL := fmt.Sprintf("%s/api/packages/%s", h.apiURL, name)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := h.proxy.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hex API returned %d", resp.StatusCode)
	}

	body, err := h.proxy.ReadMetadata(resp.Body)
	if err != nil {
		return nil, err
	}
	var pkg hexPackageAPI
	if err := json.Unmarshal(body, &pkg); err != nil {
		return nil, err
	}

	packagePURL := canonicalPackagePURL("hex", name)
	filtered := make(map[string]bool)

	for _, release := range pkg.Releases {
		insertedAt, err := time.Parse(time.RFC3339Nano, release.InsertedAt)
		if err != nil {
			continue
		}

		if !h.proxy.Cooldown.IsAllowed("hex", packagePURL, insertedAt) {
			filtered[release.Version] = true
			h.proxy.Logger.Info("cooldown: filtering hex version",
				"package", name, "version", release.Version,
				"published", release.InsertedAt)
		}
	}

	return filtered, nil
}

// filterSignedPackage decompresses gzipped data, decodes the Signed protobuf wrapper,
// filters releases from the Package payload, and re-encodes as gzipped protobuf
// (without the original signature since the payload has changed).
func (h *HexHandler) filterSignedPackage(gzippedData []byte, filteredVersions map[string]bool) ([]byte, error) {
	// Decompress gzip, with the metadata limit applied to the decompressed size
	gr, err := gzip.NewReader(bytes.NewReader(gzippedData))
	if err != nil {
		return nil, err
	}
	signed, err := h.proxy.ReadMetadata(gr)
	if err != nil {
		return nil, err
	}
	_ = gr.Close()

	// Parse Signed message: field 1 = payload (bytes), field 2 = signature (bytes)
	payload, err := extractProtobufBytes(signed, 1)
	if err != nil {
		return nil, fmt.Errorf("extracting payload: %w", err)
	}

	// Filter releases from the Package message
	filteredPayload, err := filterPackageReleases(payload, filteredVersions)
	if err != nil {
		return nil, fmt.Errorf("filtering releases: %w", err)
	}

	// Re-encode Signed message with modified payload and no signature
	var newSigned []byte
	newSigned = protowire.AppendTag(newSigned, 1, protowire.BytesType)
	newSigned = protowire.AppendBytes(newSigned, filteredPayload)

	// Gzip compress
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(newSigned); err != nil {
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// filterPackageReleases filters releases from a Package protobuf message.
// Package: field 1 = releases (repeated), field 2 = name, field 3 = repository
func filterPackageReleases(payload []byte, filteredVersions map[string]bool) ([]byte, error) {
	var result []byte
	data := payload

	for len(data) > 0 {
		num, wtype, n := protowire.ConsumeTag(data)
		if n < 0 {
			return nil, fmt.Errorf("invalid protobuf tag")
		}

		tagBytes := data[:n]
		data = data[n:]

		var fieldBytes []byte
		switch wtype {
		case protowire.BytesType:
			v, vn := protowire.ConsumeBytes(data)
			if vn < 0 {
				return nil, fmt.Errorf("invalid protobuf bytes field")
			}
			fieldBytes = data[:vn]
			data = data[vn:]

			if num == 1 { // releases field
				version := extractReleaseVersion(v)
				if filteredVersions[version] {
					continue // skip this release
				}
			}
		case protowire.VarintType:
			_, vn := protowire.ConsumeVarint(data)
			if vn < 0 {
				return nil, fmt.Errorf("invalid protobuf varint")
			}
			fieldBytes = data[:vn]
			data = data[vn:]
		default:
			return nil, fmt.Errorf("unexpected wire type %d", wtype)
		}

		result = append(result, tagBytes...)
		result = append(result, fieldBytes...)
	}

	return result, nil
}

// extractReleaseVersion extracts the version string from a Release protobuf message.
// Release: field 1 = version (string)
func extractReleaseVersion(release []byte) string {
	data := release
	for len(data) > 0 {
		num, wtype, n := protowire.ConsumeTag(data)
		if n < 0 {
			return ""
		}
		data = data[n:]

		switch wtype {
		case protowire.BytesType:
			v, vn := protowire.ConsumeBytes(data)
			if vn < 0 {
				return ""
			}
			if num == 1 {
				return string(v)
			}
			data = data[vn:]
		case protowire.VarintType:
			_, vn := protowire.ConsumeVarint(data)
			if vn < 0 {
				return ""
			}
			data = data[vn:]
		default:
			return ""
		}
	}
	return ""
}

// extractProtobufBytes extracts a bytes field from a protobuf message by field number.
func extractProtobufBytes(data []byte, fieldNum protowire.Number) ([]byte, error) {
	for len(data) > 0 {
		num, wtype, n := protowire.ConsumeTag(data)
		if n < 0 {
			return nil, fmt.Errorf("invalid protobuf tag")
		}
		data = data[n:]

		switch wtype {
		case protowire.BytesType:
			v, vn := protowire.ConsumeBytes(data)
			if vn < 0 {
				return nil, fmt.Errorf("invalid protobuf bytes")
			}
			if num == fieldNum {
				return v, nil
			}
			data = data[vn:]
		case protowire.VarintType:
			_, vn := protowire.ConsumeVarint(data)
			if vn < 0 {
				return nil, fmt.Errorf("invalid protobuf varint")
			}
			data = data[vn:]
		default:
			return nil, fmt.Errorf("unexpected wire type %d", wtype)
		}
	}
	return nil, fmt.Errorf("field %d not found", fieldNum)
}

// proxyCached forwards a request with metadata caching.
func (h *HexHandler) proxyCached(w http.ResponseWriter, r *http.Request) {
	cacheKey := strings.TrimPrefix(r.URL.Path, "/")
	h.proxy.ProxyCached(w, r, h.upstreamURL+r.URL.Path, "hex", cacheKey, "*/*")
}

// proxyUpstream forwards a request to hex.pm without caching.
func (h *HexHandler) proxyUpstream(w http.ResponseWriter, r *http.Request) {
	h.proxy.ProxyUpstream(w, r, h.upstreamURL+r.URL.Path, []string{"Accept"})
}
