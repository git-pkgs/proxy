package handler

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/git-pkgs/proxy/internal/metadata/hex"
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
		publishTime: h.publishTime,
	})
}

// parseTarballFilename extracts name and version from a hex tarball filename.
// e.g., "phoenix-1.7.10.tar" -> ("phoenix", "1.7.10")
// e.g., "amqp_client-4.1.5-1.tar" -> ("amqp_client", "4.1.5-1")
//
// Hex package names can't contain a hyphen, so the first one separates the
// name from a version that may carry hyphens of its own.
func (h *HexHandler) parseTarballFilename(filename string) (name, version string) {
	return hex.TarballIdentity(filename)
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
				if docs.withheld(h.proxy, "hex", name, version) {
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

// hexPackageAPI and hexRelease are the Hex API package response.
type (
	hexPackageAPI = hex.APIPackage
	hexRelease    = hex.APIRelease
)

// parseHexReleases reads the publish times from a Hex API package response.
func parseHexReleases(body []byte) (publishTimes, error) {
	return hex.ReleaseTimes(body)
}

func (h *HexHandler) publishTime(r *http.Request, name, version string) (time.Time, bool, bool, error) {
	return h.proxy.lookupPublishTime(r.Context(), h.timedPackage(name), version, h.packageVersions)
}

// signedPayload decompresses a gzipped Signed message, with the metadata
// limit applied to the decompressed size, and returns its payload.
func (h *HexHandler) signedPayload(gzippedData []byte) ([]byte, error) {
	signed, err := h.decompressSigned(gzippedData)
	if err != nil {
		return nil, err
	}
	return hex.Payload(signed)
}

// decompressSigned gunzips a Signed message with the metadata limit
// applied to the decompressed size.
func (h *HexHandler) decompressSigned(gzippedData []byte) ([]byte, error) {
	gr, err := gzip.NewReader(bytes.NewReader(gzippedData))
	if err != nil {
		return nil, err
	}
	signed, err := h.proxy.ReadMetadata(gr)
	if err != nil {
		return nil, err
	}
	_ = gr.Close()
	return signed, nil
}

// packageReleaseVersions lists the versions of the releases in a Package
// message.
func packageReleaseVersions(payload []byte) ([]string, error) {
	return hex.ReleaseVersions(payload)
}

// filterSignedPackage decompresses gzipped data, with the metadata limit
// applied, and returns it gzipped without the releases filteredVersions
// holds and without the signature, since the payload has changed.
func (h *HexHandler) filterSignedPackage(gzippedData []byte, filteredVersions map[string]bool) ([]byte, error) {
	signed, err := h.decompressSigned(gzippedData)
	if err != nil {
		return nil, err
	}
	return hex.FilterSigned(signed, filteredVersions)
}

// filterPackageReleases filters releases from a Package protobuf message
// (see hex.FilterReleases).
func filterPackageReleases(payload []byte, filteredVersions map[string]bool, seen func(string)) ([]byte, error) {
	return hex.FilterReleases(payload, filteredVersions, seen)
}

// extractReleaseVersion extracts the version string from a Release message.
func extractReleaseVersion(release []byte) string {
	return hex.ReleaseVersion(release)
}

// extractProtobufBytes extracts a bytes field from a protobuf message by field number.
func extractProtobufBytes(data []byte, fieldNum protowire.Number) ([]byte, error) {
	return hex.BytesField(data, fieldNum)
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
