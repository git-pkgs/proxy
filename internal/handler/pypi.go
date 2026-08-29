package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
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
	minWheelParts        = 5 // name + version + python + abi + platform
	minSubmatchParts     = 2 // full match + first capture group
	minPyPIPathParts     = 3 // hash_prefix + hash + filename
	minEggParts          = 3 // name + version + python tag

	// PyPIMetadataSuffix is the PEP 658 core-metadata sidecar suffix that pip
	// appends to a distribution URL when the index advertises core metadata.
	// A sidecar resolves to the same name and version as the distribution it
	// describes, so it is cached alongside it; consumers that expect an openable
	// archive must skip these.
	PyPIMetadataSuffix = ".metadata"
)

// PyPIHandler handles PyPI registry protocol requests.
type PyPIHandler struct {
	proxy          *Proxy
	upstreamURL    string
	downloadURL    string
	downloadHrefRe *regexp.Regexp
	proxyURL       string
}

// NewPyPIHandler creates a new PyPI protocol handler.
func NewPyPIHandler(proxy *Proxy, proxyURL string) *PyPIHandler {
	return NewPyPIHandlerWithUpstreams(proxy, proxyURL, "", "")
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
	h.downloadHrefRe = regexp.MustCompile(`href="(` + regexp.QuoteMeta(h.downloadURL) + `/packages/[^"]+)"`)
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

	return mux
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

	h.proxy.Logger.Info("pypi simple request", "package", name)

	upstreamURL := fmt.Sprintf("%s/simple/%s/", h.upstreamURL, name)
	accept := selectPyPISimpleRepresentation(r.Header.Get("Accept"))
	cacheKey := pypiSimpleCacheKey(name, accept)

	body, contentType, err := h.proxy.FetchOrCacheMetadata(r.Context(), "pypi", cacheKey, upstreamURL, accept)
	if err != nil {
		if errors.Is(err, ErrUpstreamNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		h.proxy.Logger.Error("upstream request failed", "error", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}

	// When cooldown is enabled, fetch JSON metadata to get version timestamps
	var filteredVersions map[string]bool
	if h.proxy.Cooldown != nil && h.proxy.Cooldown.Enabled() {
		filteredVersions = h.fetchFilteredVersions(r, name)
	}

	var rewritten []byte
	if isJSONMediaType(contentType) {
		rewritten, err = h.rewriteSimpleJSON(body, filteredVersions)
		if err != nil {
			h.proxy.Logger.Warn("failed to rewrite pypi simple json, proxying original", "error", err)
			rewritten = body
		}
	} else {
		rewritten = h.rewriteSimpleHTML(body, filteredVersions)
	}

	w.Header().Set("Content-Type", contentType)
	ensureVaryAccept(w.Header())
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(rewritten)
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
	for _, value := range header.Values("Vary") {
		for field := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(field), "Accept") {
				return
			}
		}
	}
	header.Add("Vary", "Accept")
}

// fetchFilteredVersions fetches JSON metadata and returns a set of version strings
// that should be filtered out due to cooldown.
func (h *PyPIHandler) fetchFilteredVersions(r *http.Request, name string) map[string]bool {
	jsonURL := fmt.Sprintf("%s/pypi/%s/json", h.upstreamURL, name)

	body, _, err := h.proxy.FetchOrCacheMetadata(r.Context(), "pypi", name+"/json", jsonURL)
	if err != nil {
		return nil
	}

	var metadata map[string]any
	if err := json.Unmarshal(body, &metadata); err != nil {
		return nil
	}

	releases, ok := metadata["releases"].(map[string]any)
	if !ok {
		return nil
	}

	packagePURL := canonicalPackagePURL("pypi", name)
	filtered := make(map[string]bool)

	for version, files := range releases {
		filesArr, ok := files.([]any)
		if !ok {
			continue
		}
		publishedAt := h.newestUploadTime(filesArr)
		if !publishedAt.IsZero() && !h.proxy.Cooldown.IsAllowed("pypi", packagePURL, publishedAt) {
			filtered[version] = true
		}
	}

	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

// rewriteSimpleHTML rewrites package URLs in simple API HTML to point at this proxy.
// If filteredVersions is non-nil, links for those versions are removed entirely.
func (h *PyPIHandler) rewriteSimpleHTML(body []byte, filteredVersions map[string]bool) []byte {
	// If cooldown filtering is active, remove entire <a> tags for filtered versions
	if len(filteredVersions) > 0 {
		// Match full anchor tags: <a ...href="...">filename</a>
		linkRe := regexp.MustCompile(`<a[^>]+href="[^"]*"[^>]*>[^<]+</a>`)
		body = linkRe.ReplaceAllFunc(body, func(match []byte) []byte {
			// Extract filename from between tags
			innerRe := regexp.MustCompile(`>([^<]+)</a>`)
			innerMatch := innerRe.FindSubmatch(match)
			if len(innerMatch) < minSubmatchParts {
				return match
			}
			filename := string(innerMatch[1])
			_, version := h.parseFilename(strings.TrimSpace(filename))
			if version != "" && filteredVersions[version] {
				return nil
			}
			return match
		})
	}

	// Match href attributes pointing to packages on the configured download host.
	return h.downloadHrefRe.ReplaceAllFunc(body, func(match []byte) []byte {
		submatch := h.downloadHrefRe.FindSubmatch(match)
		if len(submatch) < minSubmatchParts {
			return match
		}

		origURL := string(submatch[1])

		newURL := h.proxyURL + "/pypi/packages" + strings.TrimPrefix(origURL, h.downloadURL)
		return []byte(fmt.Sprintf(`href="%s"`, newURL))
	})
}

func (h *PyPIHandler) rewriteSimpleJSON(body []byte, filteredVersions map[string]bool) ([]byte, error) {
	var metadata map[string]any
	if err := json.Unmarshal(body, &metadata); err != nil {
		return nil, err
	}

	files, ok := metadata["files"].([]any)
	if !ok {
		return nil, errors.New("pypi simple json response has no files array")
	}

	rewrittenFiles := make([]any, 0, len(files))
	for _, file := range files {
		entry, ok := file.(map[string]any)
		if !ok {
			rewrittenFiles = append(rewrittenFiles, file)
			continue
		}

		if filename, ok := entry["filename"].(string); ok {
			_, version := h.parseFilename(filename)
			if version != "" && filteredVersions[version] {
				continue
			}
		}

		h.rewriteURLEntry(entry)
		rewrittenFiles = append(rewrittenFiles, entry)
	}

	metadata["files"] = rewrittenFiles
	return json.Marshal(metadata)
}

// handleJSON serves the JSON API package metadata.
func (h *PyPIHandler) handleJSON(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "invalid package name", http.StatusBadRequest)
		return
	}

	h.proxy.Logger.Info("pypi json request", "package", name)

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

	h.proxy.Logger.Info("pypi version json request", "package", name, "version", version)

	upstreamURL := fmt.Sprintf("%s/pypi/%s/%s/json", h.upstreamURL, name, version)
	h.proxyAndRewriteJSON(w, r, upstreamURL, name+"/"+version)
}

// proxyAndRewriteJSON fetches JSON metadata and rewrites download URLs.
func (h *PyPIHandler) proxyAndRewriteJSON(w http.ResponseWriter, r *http.Request, upstreamURL, cacheKey string) {
	body, _, err := h.proxy.FetchOrCacheMetadata(r.Context(), "pypi", cacheKey, upstreamURL)
	if err != nil {
		if errors.Is(err, ErrUpstreamNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		h.proxy.Logger.Error("upstream request failed", "error", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}

	rewritten, err := h.rewriteJSONMetadata(body)
	if err != nil {
		h.proxy.Logger.Warn("failed to rewrite metadata, proxying original", "error", err)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(rewritten)
}

// rewriteJSONMetadata rewrites download URLs in PyPI JSON metadata.
// If cooldown is enabled, versions published too recently are filtered out.
func (h *PyPIHandler) rewriteJSONMetadata(body []byte) ([]byte, error) {
	var metadata map[string]any
	if err := json.Unmarshal(body, &metadata); err != nil {
		return nil, err
	}

	packageName, _ := extractPyPIName(metadata)
	packagePURL := ""
	if packageName != "" {
		packagePURL = canonicalPackagePURL("pypi", packageName)
	}

	h.filterAndRewriteReleases(metadata, packageName, packagePURL)
	h.filterAndRewriteURLs(metadata, packagePURL)

	return json.Marshal(metadata)
}

// filterAndRewriteReleases applies cooldown filtering and URL rewriting to the
// releases map in PyPI metadata.
func (h *PyPIHandler) filterAndRewriteReleases(metadata map[string]any, packageName, packagePURL string) {
	releases, ok := metadata["releases"].(map[string]any)
	if !ok {
		return
	}

	for version, files := range releases {
		if h.shouldFilterRelease(packagePURL, files) {
			h.proxy.Logger.Info("cooldown: filtering pypi version",
				"package", packageName, "version", version)
			delete(releases, version)
			continue
		}

		h.rewriteFileEntries(files)
	}
}

// shouldFilterRelease returns true if a release should be excluded due to cooldown.
func (h *PyPIHandler) shouldFilterRelease(packagePURL string, files any) bool {
	if h.proxy.Cooldown == nil || !h.proxy.Cooldown.Enabled() || packagePURL == "" {
		return false
	}

	filesArr, ok := files.([]any)
	if !ok {
		return false
	}

	publishedAt := h.newestUploadTime(filesArr)
	return !publishedAt.IsZero() && !h.proxy.Cooldown.IsAllowed("pypi", packagePURL, publishedAt)
}

// versionInCooldown reports whether a version is still inside the cooldown
// window. Filtering the simple index is not enough on its own: file URLs are
// recorded in lockfiles and requirements pins, so pip can reach the download
// path without ever reading the index.
//
// A release whose upload time cannot be determined is allowed through, matching
// how fetchFilteredVersions treats it.
func (h *PyPIHandler) versionInCooldown(r *http.Request, name, version string) bool {
	if h.proxy.Cooldown == nil || !h.proxy.Cooldown.Enabled() {
		return false
	}

	return h.fetchFilteredVersions(r, name)[version]
}

// rewriteFileEntries rewrites URLs in a list of file entries.
func (h *PyPIHandler) rewriteFileEntries(files any) {
	filesArr, ok := files.([]any)
	if !ok {
		return
	}

	for _, f := range filesArr {
		if fmap, ok := f.(map[string]any); ok {
			h.rewriteURLEntry(fmap)
		}
	}
}

// filterAndRewriteURLs applies cooldown filtering and URL rewriting to the
// urls array (current version files) in PyPI metadata.
func (h *PyPIHandler) filterAndRewriteURLs(metadata map[string]any, packagePURL string) {
	urls, ok := metadata["urls"].([]any)
	if !ok {
		return
	}

	if h.shouldFilterRelease(packagePURL, urls) {
		metadata["urls"] = []any{}
	}

	if urls, ok := metadata["urls"].([]any); ok {
		for _, u := range urls {
			if umap, ok := u.(map[string]any); ok {
				h.rewriteURLEntry(umap)
			}
		}
	}
}

// extractPyPIName extracts the package name from PyPI JSON metadata.
func extractPyPIName(metadata map[string]any) (string, bool) {
	info, ok := metadata["info"].(map[string]any)
	if !ok {
		return "", false
	}
	name, ok := info["name"].(string)
	return name, ok
}

// newestUploadTime returns the most recent upload_time_iso_8601 from a list of file entries.
func (h *PyPIHandler) newestUploadTime(files []any) time.Time {
	var newest time.Time
	for _, f := range files {
		fmap, ok := f.(map[string]any)
		if !ok {
			continue
		}
		ts, ok := fmap["upload_time_iso_8601"].(string)
		if !ok {
			continue
		}
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			continue
		}
		if t.After(newest) {
			newest = t
		}
	}
	return newest
}

// rewriteURLEntry rewrites a single URL entry in PyPI metadata.
func (h *PyPIHandler) rewriteURLEntry(entry map[string]any) {
	urlStr, ok := entry["url"].(string)
	if !ok {
		return
	}

	if strings.HasPrefix(urlStr, h.downloadURL+"/packages/") {
		entry["url"] = h.proxyURL + "/pypi/packages" + strings.TrimPrefix(urlStr, h.downloadURL)
	}
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

	if name != "" && h.versionInCooldown(r, name, version) {
		h.proxy.Logger.Info("cooldown: withholding pypi file",
			"name", name, "version", version, "filename", filename)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	if name == "" {
		// Can't determine name/version, use hash as identifier
		name = fmt.Sprintf("_hash_%s", hashPath(path))
		version = "0"
	}

	h.proxy.Logger.Info("pypi download request",
		"name", name, "version", version, "filename", filename)

	// The path value starts with 'packages/' (no leading slash), so add
	// the separator here.
	upstreamURL := fmt.Sprintf("%s/%s", h.downloadURL, path)

	result, err := h.proxy.GetOrFetchArtifactFromURL(r.Context(), "pypi", name, version, filename, upstreamURL)
	if err != nil {
		h.proxy.serveArtifactError(w, err, "failed to fetch package")
		return
	}

	ServeArtifact(w, result)
}

// archiveExtensions are sdist formats of the form {name}-{version}{ext}. They
// carry no trailing tags, but legacy sdist names may contain hyphens.
var archiveExtensions = []string{".tar.gz", ".tar.bz2", ".tar.xz", ".tar.Z", ".tgz", ".tar", ".zip"}

// windowsInstallerExtensions are the legacy distutils bdist_wininst and
// bdist_msi formats, which share a filename layout.
var windowsInstallerExtensions = []string{".exe", ".msi"}

// parseFilename extracts package name and version from a PyPI filename.
// Handles wheels, sdists and legacy bdist formats:
// - requests-2.31.0-py3-none-any.whl
// - requests-2.31.0.tar.gz
// - numpy-1.8.0-py2.7-macosx-10.9-x86_64.egg
// - numpy-1.8.0.win32-py2.7.exe
func (h *PyPIHandler) parseFilename(filename string) (name, version string) {
	// PEP 658/714 core-metadata sidecars are the distribution filename plus
	// ".metadata"; they describe the same name and version. Without this, pip's
	// metadata-only fetches fall back to a hash-derived package identifier.
	filename = strings.TrimSuffix(filename, PyPIMetadataSuffix)

	switch {
	case strings.HasSuffix(filename, ".whl"):
		return parseWheelFilename(strings.TrimSuffix(filename, ".whl"))
	case strings.HasSuffix(filename, ".egg"):
		return parseEggFilename(strings.TrimSuffix(filename, ".egg"))
	}

	for _, ext := range windowsInstallerExtensions {
		if strings.HasSuffix(filename, ext) {
			return parseWindowsInstallerFilename(strings.TrimSuffix(filename, ext))
		}
	}

	for _, ext := range archiveExtensions {
		if strings.HasSuffix(filename, ext) {
			return splitNameVersion(strings.TrimSuffix(filename, ext))
		}
	}

	return "", ""
}

// parseWheelFilename parses the PEP 427 layout
// {name}-{version}(-{build})?-{python}-{abi}-{platform}, base being the
// filename without its ".whl" suffix. The spec escapes every hyphen in the name
// and version to '_', so the first two fields are authoritative even when the
// optional build tag is present.
func parseWheelFilename(base string) (name, version string) {
	parts := strings.Split(base, "-")
	if len(parts) < minWheelParts {
		return "", ""
	}

	return parts[0], parts[1]
}

// parseEggFilename parses the setuptools bdist_egg layout
// {name}-{version}-py{X.Y}(-{platform})?, base being the filename without its
// ".egg" suffix. setuptools escapes hyphens in the name and version to '_', but
// eggs built by other tooling do not always, so the version is located relative
// to the interpreter field rather than assumed to be the second field.
func parseEggFilename(base string) (name, version string) {
	parts := strings.Split(base, "-")
	// Scan from the end: the trailing platform fields never look like an
	// interpreter tag, so the last match is the real one even when the package
	// name itself carries a "py{N}" component. Stop before index 1, since a tag
	// any earlier would leave no room for both a name and a version.
	for i := len(parts) - 1; i >= minEggParts-1; i-- {
		if !isEggPythonTag(parts[i]) || !isVersionField(parts[i-1]) {
			continue
		}

		return strings.Join(parts[:i-1], "-"), parts[i-1]
	}

	// No interpreter field: {name}-{version}.
	return splitNameVersion(base)
}

// parseWindowsInstallerFilename parses the distutils bdist_wininst and
// bdist_msi layout {name}-{version}.{platform}(-py{X.Y})?, base being the
// filename without its ".exe" or ".msi" suffix. The platform is joined to the
// version with a '.' rather than a '-' and may itself contain a hyphen
// ("win-amd64"), so both trailing fields are stripped before the name and
// version are split apart.
func parseWindowsInstallerFilename(base string) (name, version string) {
	if i := strings.LastIndex(base, "-py"); i >= 0 && isDottedNumber(base[i+len("-py"):]) {
		base = base[:i]
	}

	// The platform is the final '.'-separated field. Requiring it to start with
	// a non-digit keeps a dotted version from being truncated when a filename
	// carries no platform tag.
	i := strings.LastIndex(base, ".")
	if i < 0 || i+1 >= len(base) || isVersionStart(base[i+1]) {
		return "", ""
	}

	return splitFullname(base[:i])
}

// splitFullname splits the distutils fullname {name}-{version} that precedes a
// Windows installer's platform field. Unlike an sdist, a wininst fullname may
// carry a trailing build variant ("cx_Oracle-5.1.2-11g"), which belongs to
// neither the name nor the version, so the first purely numeric field wins and
// anything after it is discarded.
func splitFullname(fullname string) (name, version string) {
	parts := strings.Split(fullname, "-")
	for i := 1; i < len(parts); i++ {
		if isDottedNumber(parts[i]) {
			return strings.Join(parts[:i], "-"), parts[i]
		}
	}

	// No purely numeric field, e.g. a prerelease version like "1.0b1".
	return splitNameVersion(fullname)
}

// splitNameVersion splits a {name}-{version} pair at the last hyphen that
// starts a version, leaving hyphens inside the name intact.
func splitNameVersion(base string) (name, version string) {
	for i := len(base) - 1; i >= 0; i-- {
		if base[i] == '-' && i+1 < len(base) && isVersionStart(base[i+1]) {
			return base[:i], base[i+1:]
		}
	}

	return "", ""
}

// isEggPythonTag reports whether field is the py{X.Y} interpreter field that
// setuptools places directly after the version in an egg filename.
func isEggPythonTag(field string) bool {
	const prefix = "py"

	return len(field) > len(prefix) && strings.HasPrefix(field, prefix) && isVersionStart(field[len(prefix)])
}

// isVersionField reports whether field can be a version, i.e. it is non-empty
// and starts with a digit as every PEP 440 release segment does.
func isVersionField(field string) bool {
	return field != "" && isVersionStart(field[0])
}

// isDottedNumber reports whether s is a dotted numeric version such as "2.7".
func isDottedNumber(s string) bool {
	if s == "" || !isVersionStart(s[0]) {
		return false
	}

	for i := range len(s) {
		if !isVersionStart(s[i]) && s[i] != '.' {
			return false
		}
	}

	return true
}

func isVersionStart(c byte) bool {
	return c >= '0' && c <= '9'
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
		h.proxy.Logger.Error("upstream request failed", "error", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	ensureVaryAccept(w.Header())

	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
