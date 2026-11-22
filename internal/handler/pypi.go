package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/git-pkgs/proxy/internal/metadata/pypi"
	"github.com/git-pkgs/proxy/internal/retention"
)

const (
	pypiUpstream         = "https://pypi.org"
	pypiDownloadUpstream = "https://files.pythonhosted.org"
	pypiSimpleJSON       = "application/vnd.pypi.simple.v1+json"
	pypiSimpleHTML       = "application/vnd.pypi.simple.v1+html"
	pypiSimpleLatestJSON = "application/vnd.pypi.simple.latest+json"
	pypiSimpleLatestHTML = "application/vnd.pypi.simple.latest+html"
	pypiLegacyHTML       = "text/html"
	pypiExactSpecificity = 2
	minPyPIPathParts     = 3 // hash_prefix + hash + filename

	// PyPIMetadataSuffix is the PEP 658 core-metadata sidecar suffix; see
	// pypi.MetadataSuffix.
	PyPIMetadataSuffix = pypi.MetadataSuffix
)

// PyPIHandler handles PyPI registry protocol requests.
type PyPIHandler struct {
	proxy       *Proxy
	upstreamURL string
	downloadURL string
	proxyURL    string
}

// PyPI files are stored under pkg:pypi/{name}, the name taken from the
// filename and normalized the way purl does it: lowercased, "_" turned into
// "-". Package rules use the default key mapping, so pkg:pypi/Typing_Extensions
// matches typing-extensions. Dots and runs of separators are kept, so files
// named zope.interface-* and zope_interface-* end up under two packages.
func init() {
	retention.Default.Register(retention.Spec{
		Key:              "pypi",
		CanonicalPackage: retention.DefaultCanonical("pypi"),
		CompareVersions:  retention.VersionOrder("pypi"),
	})
}

// NewPyPIHandlerWithUpstreams creates a PyPI handler with custom API and
// package download upstreams.
func NewPyPIHandlerWithUpstreams(proxy *Proxy, proxyURL, upstreamURL, downloadURL string) *PyPIHandler {
	h := &PyPIHandler{
		proxy:       proxy,
		upstreamURL: configuredUpstreamURL(upstreamURL, pypiUpstream),
		downloadURL: configuredUpstreamURL(downloadURL, pypiDownloadUpstream),
		proxyURL:    strings.TrimSuffix(proxyURL, "/"),
	}
	return h
}

// Routes returns the HTTP handler for PyPI requests.
func (h *PyPIHandler) Routes() http.Handler {
	mux := http.NewServeMux()

	// Simple API
	mux.HandleFunc("GET /simple/", h.handleSimpleIndex)
	mux.HandleFunc("GET /simple/{name}/", h.handleSimplePackage)

	// JSON API
	mux.HandleFunc("GET /pypi/{name}/json", h.handleJSON)
	mux.HandleFunc("GET /pypi/{name}/{version}/json", h.handleVersionJSON)

	// Package downloads (cache these)
	mux.HandleFunc("GET /packages/{path...}", h.handleDownload)

	return rejectQueryChars(mux)
}

// handleSimpleIndex serves the simple API index.
func (h *PyPIHandler) handleSimpleIndex(w http.ResponseWriter, r *http.Request) {
	// Just proxy the index through
	h.proxySimple(w, r, "/simple/")
}

// handleSimplePackage serves the simple API package page with rewritten links.
func (h *PyPIHandler) handleSimplePackage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "invalid package name", http.StatusBadRequest)
		return
	}

	h.proxy.Logger.Debug("pypi simple request", "package", name)

	upstreamURL := fmt.Sprintf("%s/simple/%s/", h.upstreamURL, name)
	accept := selectPyPISimpleRepresentation(r.Header.Get("Accept"))
	cacheKey := pypiSimpleCacheKey(name, accept)

	// Rewritten pages go through the rewrite cache, keyed by the upstream
	// page, its content type and the versions filtered out. Without cooldown
	// the filtered versions are known up front, so a repeat request can be
	// answered without reading the stored page.
	cooldown := h.proxy.cooldownApplies("pypi", name)
	if !cooldown {
		denied := h.addDeniedVersions(name, nil)
		if stored, contentType, ok := h.proxy.storedRewriteVariant("pypi", cacheKey, h.proxyURL, cacheKey, func(contentType string) []string {
			return simpleRewriteVariant(contentType, denied)
		}); ok {
			ensureVaryAccept(w.Header())
			serveRewrittenMetadataAs(w, r, stored, contentType)
			return
		}
	}

	upstream := h.proxy.fetchMetadataDocument(r.Context(), "pypi", cacheKey, upstreamURL, accept, nil)
	if err := upstream.err; err != nil {
		if errors.Is(err, ErrUpstreamNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if h.proxy.writeUpstreamRefusal(w, err) {
			return
		}
		h.proxy.Logger.Error("upstream request failed", "error", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	contentType := upstream.contentType

	// When cooldown applies, fetch JSON metadata to get version timestamps
	var filteredVersions map[string]bool
	if cooldown {
		var err error
		filteredVersions, err = h.fetchFilteredVersions(r, name)
		if err != nil {
			h.proxy.writeCooldownLookupError(w, "pypi", name, err)
			return
		}
	}
	filteredVersions = h.addDeniedVersions(name, filteredVersions)

	isJSON := isJSONMediaType(contentType)
	digest := variantDigest(upstream.digest, upstream.body, simpleRewriteVariant(contentType, filteredVersions)...)
	rewritten, err := h.proxy.cachedRewrite(r.Context(), "pypi", h.proxyURL, cacheKey, upstream.body, digest, func(body []byte) ([]byte, []string, error) {
		if isJSON {
			out, err := h.rewriteSimpleJSON(body, filteredVersions)
			return out, nil, err
		}
		body = h.filterSimpleHTMLLinks(body, filteredVersions)
		return h.rewriteSimpleHTML(body, filteredVersions), nil, nil
	})
	if err != nil {
		if r.Context().Err() != nil {
			return // the client left while waiting on a shared rewrite
		}
		if len(filteredVersions) != 0 {
			h.proxy.Logger.Error("failed to filter pypi simple json", "package", name, "error", err)
			http.Error(w, "failed to filter package metadata", http.StatusBadGateway)
			return
		}
		h.proxy.Logger.Warn("failed to rewrite pypi simple json, proxying original", "error", err)
		w.Header().Set(headerContentType, contentType)
		ensureVaryAccept(w.Header())
		markStale(w, upstream.stale)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(upstream.body)
		return
	}

	ensureVaryAccept(w.Header())
	markStale(w, upstream.stale)
	serveRewrittenMetadataAs(w, r, rewritten, contentType)
}

// addDeniedVersions adds the package's denylisted versions to filtered,
// allocating it when there are any and it is nil.
func (h *PyPIHandler) addDeniedVersions(name string, filtered map[string]bool) map[string]bool {
	for version := range h.proxy.Denylist.Versions(canonicalPackagePURL("pypi", name)) {
		if filtered == nil {
			filtered = make(map[string]bool)
		}
		filtered[version] = true
	}
	return filtered
}

// simpleRewriteVariant returns what a simple page rewrite depends on besides
// the upstream page: its content type, which picks the HTML or JSON rewrite,
// and the versions filtered out.
func simpleRewriteVariant(contentType string, filtered map[string]bool) []string {
	return append([]string{contentType}, slices.Sorted(maps.Keys(filtered))...)
}

func selectPyPISimpleRepresentation(accept string) string {
	if strings.TrimSpace(accept) == "" {
		return pypiLegacyHTML
	}

	type score struct {
		quality     float64
		specificity int
		matched     bool
	}

	scores := map[string]score{
		pypiSimpleJSON: {},
		pypiSimpleHTML: {},
		pypiLegacyHTML: {},
	}

	update := func(representation string, quality float64, specificity int) {
		current := scores[representation]
		if !current.matched || specificity > current.specificity ||
			(specificity == current.specificity && quality > current.quality) {
			scores[representation] = score{quality: quality, specificity: specificity, matched: true}
		}
	}

	for part := range strings.SplitSeq(accept, ",") {
		mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			continue
		}

		quality := 1.0
		if value, ok := params["q"]; ok {
			quality, err = strconv.ParseFloat(value, 64)
			if err != nil || quality < 0 || quality > 1 {
				continue
			}
		}

		switch mediaType {
		case pypiSimpleJSON, pypiSimpleLatestJSON:
			update(pypiSimpleJSON, quality, pypiExactSpecificity)
		case pypiSimpleHTML, pypiSimpleLatestHTML:
			update(pypiSimpleHTML, quality, pypiExactSpecificity)
		case pypiLegacyHTML:
			update(pypiLegacyHTML, quality, pypiExactSpecificity)
		case "application/*":
			update(pypiSimpleJSON, quality, 1)
			update(pypiSimpleHTML, quality, 1)
		case "text/*":
			update(pypiLegacyHTML, quality, 1)
		case "*/*":
			update(pypiSimpleJSON, quality, 0)
			update(pypiSimpleHTML, quality, 0)
			update(pypiLegacyHTML, quality, 0)
		}
	}

	bestMediaType := ""
	bestScore := score{}
	for _, mediaType := range []string{pypiSimpleJSON, pypiSimpleHTML, pypiLegacyHTML} {
		candidate := scores[mediaType]
		if !candidate.matched || candidate.quality == 0 {
			continue
		}
		if bestMediaType == "" || candidate.quality > bestScore.quality ||
			(candidate.quality == bestScore.quality && candidate.specificity > bestScore.specificity) {
			bestMediaType = mediaType
			bestScore = candidate
		}
	}

	if bestMediaType == "" || bestScore.specificity == 0 {
		return pypiLegacyHTML
	}
	return bestMediaType
}

func pypiSimpleCacheKey(name, mediaType string) string {
	switch mediaType {
	case pypiSimpleJSON:
		return name + "/simple/json"
	case pypiSimpleHTML:
		return name + "/simple/html"
	default:
		return name + "/simple"
	}
}

func isJSONMediaType(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

func ensureVaryAccept(header http.Header) {
	ensureVary(header, "Accept")
}

// fetchFilteredVersions fetches JSON metadata and returns a set of version
// strings that should be filtered out due to cooldown. A package the JSON API
// doesn't know has no publish times to go by and gets an empty set; any other
// failure to read them is an error.
func (h *PyPIHandler) fetchFilteredVersions(r *http.Request, name string) (map[string]bool, error) {
	times, err := h.fetchReleaseTimes(r, name)
	if err != nil {
		return nil, err
	}

	packagePURL := canonicalPackagePURL("pypi", name)
	filtered := make(map[string]bool)

	for version, publishedAt := range times {
		if !publishedAt.IsZero() && !h.proxy.cooldown().IsAllowed("pypi", packagePURL, publishedAt) {
			filtered[version] = true
			h.proxy.recordWithheldPyPI(name, version, publishedAt)
		}
	}

	if len(filtered) == 0 {
		return nil, nil
	}
	return filtered, nil
}

// fetchReleaseTimes reads the JSON API and returns each release's newest
// upload time, zero for a release without one. A package the JSON API
// doesn't know gets nil; any other failure is an error.
func (h *PyPIHandler) fetchReleaseTimes(r *http.Request, name string) (map[string]time.Time, error) {
	jsonURL := fmt.Sprintf("%s/pypi/%s/json", h.upstreamURL, name)

	body, _, err := h.proxy.FetchOrCacheMetadata(r.Context(), "pypi", name+"/json", jsonURL)
	if errors.Is(err, ErrUpstreamNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return h.parseReleaseTimes(body)
}

// parseReleaseTimes returns each release's newest upload time from a JSON
// API document, zero for a release without one.
func (h *PyPIHandler) parseReleaseTimes(body []byte) (map[string]time.Time, error) {
	return pypi.ReleaseTimes(body)
}

// urlOptions are the URLs a simple index rewrite needs; the simple index is
// filtered by the versions the caller passes.
func (h *PyPIHandler) urlOptions() pypi.Options {
	return pypi.Options{ProxyURL: h.proxyURL, DownloadURL: h.downloadURL}
}

// metadataOptions add the policy the JSON API rewrite applies itself. The
// callbacks log and record what it holds back.
func (h *PyPIHandler) metadataOptions() pypi.Options {
	return pypi.Options{
		ProxyURL:    h.proxyURL,
		DownloadURL: h.downloadURL,
		Cooldown:    h.proxy.cooldown(),
		EvaluatedAt: time.Now(),
		Denied: func(name, version string) bool {
			return h.proxy.versionDenied("pypi", name, version)
		},
		OnFiltered: func(name, version string) {
			h.proxy.Logger.Info("policy: filtering pypi version", "package", name, "version", version)
		},
		OnWithheld: h.proxy.recordWithheldPyPI,
	}
}

func (h *PyPIHandler) rewriteSimpleHTML(body []byte, filteredVersions map[string]bool) []byte {
	return pypi.RewriteSimpleHTML(body, filteredVersions, h.urlOptions())
}

func (h *PyPIHandler) rewriteDownloadHrefs(body []byte) []byte {
	return pypi.RewriteDownloadHrefs(body, h.urlOptions())
}

func (h *PyPIHandler) filterSimpleHTMLLinks(body []byte, versions map[string]bool) []byte {
	return pypi.FilterSimpleHTMLLinks(body, versions)
}

func (h *PyPIHandler) rewriteSimpleJSON(body []byte, filteredVersions map[string]bool) ([]byte, error) {
	return pypi.RewriteSimpleJSON(body, filteredVersions, h.urlOptions())
}

// handleJSON serves the JSON API package metadata.
func (h *PyPIHandler) handleJSON(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "invalid package name", http.StatusBadRequest)
		return
	}

	h.proxy.Logger.Debug("pypi json request", "package", name)

	upstreamURL := fmt.Sprintf("%s/pypi/%s/json", h.upstreamURL, name)
	h.proxyAndRewriteJSON(w, r, upstreamURL, name+"/json")
}

// handleVersionJSON serves the JSON API version metadata.
func (h *PyPIHandler) handleVersionJSON(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	version := r.PathValue("version")

	if name == "" || version == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if h.proxy.versionDenied("pypi", name, version) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	h.proxy.Logger.Debug("pypi version json request", "package", name, "version", version)

	upstreamURL := fmt.Sprintf("%s/pypi/%s/%s/json", h.upstreamURL, name, version)
	h.proxyAndRewriteJSON(w, r, upstreamURL, name+"/"+version)
}

// proxyAndRewriteJSON fetches JSON metadata and rewrites download URLs.
// Rewrites go through the rewrite cache, as for npm.
func (h *PyPIHandler) proxyAndRewriteJSON(w http.ResponseWriter, r *http.Request, upstreamURL, cacheKey string) {
	if stored, ok := h.proxy.storedRewrite("pypi", cacheKey, h.proxyURL, cacheKey); ok {
		serveRewrittenMetadata(w, r, stored)
		return
	}

	upstream := h.proxy.fetchMetadataDocument(r.Context(), "pypi", cacheKey, upstreamURL, contentTypeJSON, nil)
	if err := upstream.err; err != nil {
		if errors.Is(err, ErrUpstreamNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if h.proxy.writeUpstreamRefusal(w, err) {
			return
		}
		h.proxy.Logger.Error("upstream request failed", "error", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}

	rewritten, err := h.proxy.cachedRewrite(r.Context(), "pypi", h.proxyURL, cacheKey, upstream.body, upstream.digest, h.rewriteJSONMetadata)
	if err != nil {
		if r.Context().Err() != nil {
			return // the client left while waiting on a shared rewrite
		}
		if h.proxy.mustFilterMetadata("pypi", r.PathValue("name")) {
			h.proxy.Logger.Error("failed to filter pypi metadata", "url", upstreamURL, "error", err)
			http.Error(w, "failed to filter package metadata", http.StatusBadGateway)
			return
		}
		h.proxy.Logger.Warn("failed to rewrite metadata, proxying original", "error", err)
		markStale(w, upstream.stale)
		w.Header().Set(headerContentType, "application/json")
		_, _ = w.Write(upstream.body)
		return
	}

	markStale(w, upstream.stale)
	serveRewrittenMetadata(w, r, rewritten)
}

// rewriteJSONMetadata rewrites download URLs in PyPI JSON metadata, leaving
// out releases the denylist names or cooldown holds back.
func (h *PyPIHandler) rewriteJSONMetadata(body []byte) ([]byte, []string, error) {
	return pypi.RewriteJSON(body, h.metadataOptions())
}

// versionInCooldown reports whether a version is still inside the cooldown
// window. Filtering the simple index is not enough on its own: file URLs are
// recorded in lockfiles and requirements pins, so pip can reach the download
// path without ever reading the index.
//
// A release whose upload time cannot be determined is allowed through, matching
// how fetchFilteredVersions treats it. Failing to read the times is an error.
// The time read is stored for storedCooldown, and a newer one stored
// meanwhile decides.
func (h *PyPIHandler) versionInCooldown(r *http.Request, name, version string) (bool, error) {
	if !h.proxy.cooldownApplies("pypi", name) {
		return false, nil
	}

	jsonURL := fmt.Sprintf("%s/pypi/%s/json", h.upstreamURL, name)
	publishedAt, err := readListed(r.Context(), h.proxy, "pypi", name+"/json", jsonURL, contentTypeJSON, "",
		func(res metadataResult) (time.Time, bool, error) {
			times, err := h.parseReleaseTimes(res.body)
			published, listed := releaseTime(times, version)
			return published, listed, err
		})
	if errors.Is(err, ErrUpstreamNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if publishedAt.IsZero() {
		return false, nil
	}
	publishedAt = h.storeUploadTime(name, version, publishedAt)
	return !h.proxy.cooldown().IsAllowed("pypi", canonicalPackagePURL("pypi", name), publishedAt), nil
}

// releaseTime returns the newest upload time of the releases equal to
// version, and whether there is such a release.
func releaseTime(times map[string]time.Time, version string) (newest time.Time, listed bool) {
	canonical := pypi.CanonicalVersion(version)
	for release, published := range times {
		if pypi.CanonicalVersion(release) != canonical {
			continue
		}
		listed = true
		if published.After(newest) {
			newest = published
		}
	}
	return newest, listed
}

// storeUploadTime records a release's newest upload time under the version
// PURL the file is cached under, so storedCooldown finds it. The time only
// moves forward: a request that read an older document leaves a newer one,
// and gets that newer time back.
func (h *PyPIHandler) storeUploadTime(name, version string, publishedAt time.Time) time.Time {
	if h.proxy.DB == nil {
		return publishedAt
	}
	packagePURL, versionPURL, err := packagePURLStrings("pypi", name, version)
	if err != nil {
		return publishedAt
	}
	if err := h.proxy.DB.AdvanceVersionPublishedAt(versionPURL, packagePURL, publishedAt); err != nil {
		h.proxy.Logger.Warn("cooldown: could not store pypi upload time",
			"package", name, "version", version, "error", err)
		return publishedAt
	}
	if ver, err := h.proxy.DB.GetVersionByPURL(versionPURL); err == nil && ver != nil &&
		ver.PublishedAt.Valid && ver.PublishedAt.Time.After(publishedAt) {
		return ver.PublishedAt.Time
	}
	return publishedAt
}

// storedCooldown answers the cooldown check for a download from the upload
// time stored for its release, without reading the JSON API, and reports
// whether it could. A release's time is its newest file's, and files can be
// added to a release later, so the stored time only ever moves forward:
//   - a stored time inside the window withholds every file of the release;
//   - a stored time outside it only lets through a file that is already
//     cached. A file that is not cached may be one uploaded since, so it goes
//     to the JSON API, which also stores the release's current time.
func (h *PyPIHandler) storedCooldown(r *http.Request, name, version, filename string) (inCooldown, answered bool) {
	if h.proxy.DB == nil || !h.proxy.cooldownApplies("pypi", name) {
		return false, false
	}
	packagePURL, versionPURL, err := packagePURLStrings("pypi", name, version)
	if err != nil {
		return false, false
	}
	ver, err := h.proxy.DB.GetVersionByPURL(versionPURL)
	if err != nil || ver == nil || !ver.PublishedAt.Valid {
		return false, false
	}
	if !h.proxy.cooldown().IsAllowed("pypi", packagePURL, ver.PublishedAt.Time) {
		return true, true
	}
	cached, err := h.proxy.HasCachedArtifact(r.Context(), "pypi", name, version, filename)
	return false, err == nil && cached
}

// handleDownload serves a package file, fetching and caching from upstream if needed.
func (h *PyPIHandler) handleDownload(w http.ResponseWriter, r *http.Request) {
	path := r.PathValue("path")
	if path == "" {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	// Path format: /packages/{hash_prefix}/{hash}/{filename}
	// e.g., /packages/ab/cd/abc123.../requests-2.31.0.tar.gz
	parts := strings.Split(path, "/")
	if len(parts) < minPyPIPathParts {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	filename := parts[len(parts)-1]
	name, version := h.parseFilename(filename)

	if name != "" {
		inCooldown, answered := h.storedCooldown(r, name, version, filename)
		var err error
		if !answered {
			inCooldown, err = h.versionInCooldown(r, name, version)
		}
		if err != nil {
			if !h.proxy.serveCachedDespiteCooldownError(w, r, "pypi", name, version, filename, err) {
				h.proxy.writeCooldownLookupError(w, "pypi", name, err)
			}
			return
		}
		if inCooldown {
			h.proxy.Logger.Info("cooldown: withholding pypi file",
				"name", name, "version", version, "filename", filename)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
	}

	if name == "" {
		// Can't determine name/version, use hash as identifier
		name = fmt.Sprintf("_hash_%s", hashPath(path))
		version = "0"
	}

	h.proxy.Logger.Debug("pypi download request",
		"name", name, "version", version, "filename", filename)

	// The path value starts with 'packages/' (no leading slash), so add
	// the separator here.
	upstreamURL := fmt.Sprintf("%s/%s", h.downloadURL, path)

	result, err := h.proxy.GetOrFetchArtifactFromURL(r.Context(), "pypi", name, version, filename, upstreamURL)
	if err != nil {
		h.proxy.serveArtifactError(w, err, "failed to fetch package")
		return
	}

	ServeArtifactRequest(w, r, result)
}

func (h *PyPIHandler) parseFilename(filename string) (name, version string) {
	return pypi.FilenameIdentity(filename)
}

func hashPath(path string) string {
	h := sha256.Sum256([]byte(path))
	return hex.EncodeToString(h[:8])
}

// proxySimple proxies a simple API request.
func (h *PyPIHandler) proxySimple(w http.ResponseWriter, r *http.Request, path string) {
	upstreamURL := h.upstreamURL + path

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Accept", selectPyPISimpleRepresentation(r.Header.Get("Accept")))

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

	// The HTML root index links projects by absolute path, such as
	// /simple/<name>/, which must point at the proxy's /pypi mount instead.
	// It is large, so it is rewritten as it streams.
	if resp.StatusCode == http.StatusOK && resp.Header.Get("Content-Encoding") == "" &&
		!strings.Contains(resp.Header.Get(headerContentType), "json") {
		prefix := "/simple/"
		if u, err := url.Parse(h.upstreamURL); err == nil {
			prefix = strings.TrimSuffix(u.Path, "/") + prefix
		}
		to := h.proxyURL + "/pypi/simple/"
		resp.Body = &replacingReader{
			src: resp.Body,
			replacements: []replacement{
				{from: []byte(`href="` + prefix), to: []byte(`href="` + to)},
				{from: []byte(`href='` + prefix), to: []byte(`href='` + to)},
			},
		}
		resp.Header.Del(headerContentLength)
		resp.Header.Del("ETag") // it names the upstream body
		resp.ContentLength = -1
	}

	h.proxy.relayResponse(w, r, resp, func(dst, src http.Header) {
		copyRelayHeaders(dst, src)
		ensureVaryAccept(dst)
	})
}

const replacingReaderBufferSize = 32 << 10

// replacingReader makes each replacement in src as it is read.
type replacingReader struct {
	src          io.ReadCloser
	replacements []replacement
	in           []byte // read from src, not yet scanned
	out          []byte // scanned, not yet returned
	buf          []byte
	err          error
}

type replacement struct{ from, to []byte }

func (r *replacingReader) Read(p []byte) (int, error) {
	for len(r.out) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		if r.buf == nil {
			r.buf = make([]byte, replacingReaderBufferSize)
		}
		n, err := r.src.Read(r.buf)
		r.in = append(r.in, r.buf[:n]...)
		r.err = err
		// Keep a tail that could be the start of a match split across reads.
		keep := 0
		if err == nil {
			for _, rep := range r.replacements {
				keep = max(keep, len(rep.from)-1)
			}
		}
		scan := max(len(r.in)-keep, 0)
		out, done := r.replace(scan)
		if done < scan {
			out = append(out, r.in[done:scan]...)
			done = scan
		}
		r.out = out
		r.in = append([]byte(nil), r.in[done:]...)
	}
	n := copy(p, r.out)
	r.out = r.out[n:]
	return n, nil
}

// replace makes the replacements that start before scan in r.in, returning
// the output and how much of r.in it covers.
func (r *replacingReader) replace(scan int) (out []byte, done int) {
	// at holds each replacement's next match at or after done, or -1.
	// Only a match the last replacement consumed is searched for again,
	// so each pass over r.in stays linear.
	at := make([]int, len(r.replacements))
	for i := range at {
		at[i] = indexFrom(r.in, r.replacements[i].from, 0)
	}
	for {
		k := -1
		for i, j := range at {
			if j >= 0 && (k < 0 || j < at[k]) {
				k = i
			}
		}
		if k < 0 || at[k] >= scan {
			break
		}
		rep := &r.replacements[k]
		out = append(out, r.in[done:at[k]]...)
		out = append(out, rep.to...)
		done = at[k] + len(rep.from)
		for i, j := range at {
			if j >= 0 && j < done {
				at[i] = indexFrom(r.in, r.replacements[i].from, done)
			}
		}
	}
	return out, done
}

func (r *replacingReader) Close() error { return r.src.Close() }

// indexFrom returns the index of the first sep in b at or after from, or -1.
func indexFrom(b, sep []byte, from int) int {
	if i := bytes.Index(b[from:], sep); i >= 0 {
		return from + i
	}
	return -1
}
