package handler

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/git-pkgs/purl"
	"google.golang.org/protobuf/encoding/protowire"
)

const (
	hexUpstream = "https://repo.hex.pm"
)

// HexHandler handles Hex.pm registry protocol requests.
type HexHandler struct {
	proxy       *Proxy
	upstreamURL string
	proxyURL    string
}

// NewHexHandler creates a new Hex.pm protocol handler.
func NewHexHandler(proxy *Proxy, proxyURL string) *HexHandler {
	return &HexHandler{
		proxy:       proxy,
		upstreamURL: hexUpstream,
		proxyURL:    strings.TrimSuffix(proxyURL, "/"),
	}
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
	filename := r.PathValue("filename")
	if filename == "" || !strings.HasSuffix(filename, ".tar") {
		http.Error(w, "invalid filename", http.StatusBadRequest)
		return
	}

	// Extract name and version from filename (e.g., "phoenix-1.7.10.tar")
	name, version := h.parseTarballFilename(filename)
	if name == "" || version == "" {
		http.Error(w, "could not parse tarball filename", http.StatusBadRequest)
		return
	}

	h.proxy.Logger.Info("hex download request",
		"name", name, "version", version, "filename", filename)

	result, err := h.proxy.GetOrFetchArtifact(r.Context(), "hex", name, version, filename)
	if err != nil {
		h.proxy.Logger.Error("failed to get artifact", "error", err)
		http.Error(w, "failed to fetch package", http.StatusBadGateway)
		return
	}

	ServeArtifact(w, result)
}

// parseTarballFilename extracts name and version from a hex tarball filename.
// e.g., "phoenix-1.7.10.tar" -> ("phoenix", "1.7.10")
func (h *HexHandler) parseTarballFilename(filename string) (name, version string) {
	base := strings.TrimSuffix(filename, ".tar")

	// Find the last hyphen followed by a version number
	for i := len(base) - 1; i >= 0; i-- {
		if base[i] == '-' && i+1 < len(base) && base[i+1] >= '0' && base[i+1] <= '9' {
			return base[:i], base[i+1:]
		}
	}
	return "", ""
}

// hexAPIURL is the Hex HTTP API base URL for fetching package metadata with timestamps.
const hexAPIURL = "https://hex.pm"

// handlePackages proxies the /packages/{name} endpoint, applying cooldown filtering
// when enabled. Since the protobuf format has no timestamps, we fetch them from the
// Hex HTTP API concurrently.
func (h *HexHandler) handlePackages(w http.ResponseWriter, r *http.Request) {
	if h.proxy.Cooldown == nil || !h.proxy.Cooldown.Enabled() {
		h.proxyCached(w, r)
		return
	}

	name := r.PathValue("name")
	if name == "" {
		h.proxyCached(w, r)
		return
	}

	h.proxy.Logger.Info("hex package request with cooldown", "name", name)

	protoResp, filteredVersions, err := h.fetchPackageAndVersions(r, name)
	if err != nil {
		h.proxy.Logger.Error("upstream request failed", "error", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = protoResp.Body.Close() }()

	if protoResp.StatusCode != http.StatusOK {
		for k, vv := range protoResp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(protoResp.StatusCode)
		_, _ = io.Copy(w, protoResp.Body)
		return
	}

	body, err := io.ReadAll(protoResp.Body)
	if err != nil {
		http.Error(w, "failed to read response", http.StatusInternalServerError)
		return
	}

	if len(filteredVersions) == 0 {
		// No versions to filter or couldn't get timestamps, pass through
		w.Header().Set("Content-Type", protoResp.Header.Get("Content-Type"))
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(body)
		return
	}

	filtered, err := h.filterSignedPackage(body, filteredVersions)
	if err != nil {
		h.proxy.Logger.Warn("failed to filter hex package, proxying original", "error", err)
		w.Header().Set("Content-Type", protoResp.Header.Get("Content-Type"))
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(body)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Encoding", "gzip")
	_, _ = w.Write(filtered)
}

// fetchPackageAndVersions fetches the protobuf package and version timestamps concurrently.
func (h *HexHandler) fetchPackageAndVersions(r *http.Request, name string) (*http.Response, map[string]bool, error) {
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
		return nil, nil, err
	}

	if versionsRes.err != nil {
		h.proxy.Logger.Warn("failed to fetch hex version timestamps, proxying unfiltered",
			"name", name, "error", versionsRes.err)
		return protoResp, nil, nil
	}

	return protoResp, versionsRes.filtered, nil
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
	apiURL := fmt.Sprintf("%s/api/packages/%s", hexAPIURL, name)
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

	var pkg hexPackageAPI
	if err := json.NewDecoder(resp.Body).Decode(&pkg); err != nil {
		return nil, err
	}

	packagePURL := purl.MakePURLString("hex", name, "")
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
	// Decompress gzip
	gr, err := gzip.NewReader(bytes.NewReader(gzippedData))
	if err != nil {
		return nil, err
	}
	signed, err := io.ReadAll(gr)
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
