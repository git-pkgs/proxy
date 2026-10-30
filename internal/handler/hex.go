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
//
// Both are read through the metadata cache, so within metadata_ttl a package
// costs no request to repo.hex.pm or to the rate limited Hex API, and after
// it both are revalidated with their ETags. The filtered package is kept in
// the rewrite cache and served with an ETag of its own.
func (h *HexHandler) handlePackages(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" || !h.proxy.cooldownApplies("hex", name) {
		h.proxyCached(w, r)
		return
	}

	h.proxy.Logger.Info("hex package request with cooldown", "name", name)

	docs, err := h.proxy.fetchTimedMetadata(r.Context(), h.timedPackage(name), h.packageVersions)
	switch {
	case docs.doc.err != nil:
		h.proxy.writeMetadataFetchError(w, docs.doc.err)
		return
	case docs.times.err != nil:
		h.proxy.writeCooldownLookupError(w, "hex", name, docs.times.err)
		return
	case err != nil:
		h.proxy.Logger.Error("failed to read hex package", "name", name, "error", err)
		http.Error(w, "failed to filter package metadata", http.StatusBadGateway)
		return
	}

	packagePURL := canonicalPackagePURL("hex", name)
	m, err := h.proxy.cachedRewrite(r.Context(), "hex", h.proxyURL, "packages/"+name, docs.doc.body, docs.rewriteDigest(),
		func([]byte) ([]byte, []string, error) {
			body, err := h.proxy.decodedMetadataBody(docs.doc)
			if err != nil {
				return nil, nil, err
			}
			versions, err := h.packageVersions(docs.doc)
			if err != nil {
				return nil, nil, err
			}
			filtered := make(map[string]bool)
			var kept []string
			for _, version := range versions {
				if docs.withheld(h.proxy, "hex", packagePURL, version) {
					filtered[version] = true
					h.proxy.Logger.Info("cooldown: filtering hex version", "package", name, "version", version)
					continue
				}
				kept = append(kept, version)
			}
			if len(filtered) == 0 {
				// Nothing in cooldown: upstream's bytes, signature included.
				return body, kept, nil
			}
			out, err := h.filterSignedPackage(body, filtered)
			return out, kept, err
		})
	if err != nil {
		h.proxy.Logger.Error("failed to filter hex package", "name", name, "error", err)
		http.Error(w, "failed to filter package metadata", http.StatusBadGateway)
		return
	}

	markStale(w, docs.doc.stale || docs.times.stale)
	serveHexPackage(w, r, m)
}

// serveHexPackage writes a package with its validators, or 304 when they
// show the client's copy is current. The registry file is gzip itself, and
// Hex clients gunzip it however it is labelled, so it is never compressed
// again.
func serveHexPackage(w http.ResponseWriter, r *http.Request, m rewrittenMetadata) {
	w.Header().Set(headerETag, m.etag)
	if !m.lastModified.IsZero() {
		w.Header().Set(headerLastModified, m.lastModified.UTC().Format(http.TimeFormat))
	}
	if notModified(r, m.etag, m.lastModified) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set(headerContentType, "application/octet-stream")
	w.Header().Set(headerContentEncoding, "gzip")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(m.content())
}

// timedPackage describes a package's registry file and the Hex API entry
// it takes publish times from.
func (h *HexHandler) timedPackage(name string) timedMetadata {
	return timedMetadata{
		ecosystem:   "hex",
		docKey:      "packages/" + name,
		docURL:      h.upstreamURL + "/packages/" + name,
		timesKey:    hashedCacheKey(h.apiURL, "api/packages/"+name),
		timesURL:    fmt.Sprintf("%s/api/packages/%s", h.apiURL, name),
		timesAccept: contentTypeJSON,
		parseTimes:  parseHexReleases,
	}
}

// packageVersions lists the releases in a gzipped Signed package.
func (h *HexHandler) packageVersions(doc metadataResult) ([]string, error) {
	body, err := h.proxy.decodedMetadataBody(doc)
	if err != nil {
		return nil, err
	}
	payload, err := h.signedPayload(body)
	if err != nil {
		return nil, err
	}
	return packageReleaseVersions(payload)
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

// parseHexReleases reads the publish times from a Hex API package response.
func parseHexReleases(body []byte) (publishTimes, error) {
	var pkg hexPackageAPI
	if err := json.Unmarshal(body, &pkg); err != nil {
		return nil, err
	}
	times := make(publishTimes, len(pkg.Releases))
	for _, release := range pkg.Releases {
		insertedAt, err := time.Parse(time.RFC3339Nano, release.InsertedAt)
		if err != nil {
			insertedAt = time.Time{}
		}
		times[release.Version] = insertedAt
	}
	return times, nil
}

// signedPayload decompresses a gzipped Signed message, with the metadata
// limit applied to the decompressed size, and returns its payload.
func (h *HexHandler) signedPayload(gzippedData []byte) ([]byte, error) {
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
	return payload, nil
}

// packageReleaseVersions lists the versions of the releases in a Package
// message.
func packageReleaseVersions(payload []byte) ([]string, error) {
	var versions []string
	_, err := filterPackageReleases(payload, nil, func(version string) {
		versions = append(versions, version)
	})
	return versions, err
}

// filterSignedPackage decompresses gzipped data, decodes the Signed protobuf wrapper,
// filters releases from the Package payload, and re-encodes as gzipped protobuf
// (without the original signature since the payload has changed).
func (h *HexHandler) filterSignedPackage(gzippedData []byte, filteredVersions map[string]bool) ([]byte, error) {
	payload, err := h.signedPayload(gzippedData)
	if err != nil {
		return nil, err
	}

	// Filter releases from the Package message
	filteredPayload, err := filterPackageReleases(payload, filteredVersions, nil)
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

// filterPackageReleases filters releases from a Package protobuf message,
// calling seen, when not nil, with the version of each release.
// Package: field 1 = releases (repeated), field 2 = name, field 3 = repository
func filterPackageReleases(payload []byte, filteredVersions map[string]bool, seen func(string)) ([]byte, error) {
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
				if seen != nil {
					seen(version)
				}
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
