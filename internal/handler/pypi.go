package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/git-pkgs/purl"
)

const (
	pypiUpstream      = "https://pypi.org"
	minWheelParts     = 5 // name + version + python + abi + platform
	minSubmatchParts  = 2 // full match + first capture group
	minPyPIPathParts  = 3 // hash_prefix + hash + filename
	minPythonTagLen   = 2 // minimum length for a python tag (e.g., "py")
)

// PyPIHandler handles PyPI registry protocol requests.
type PyPIHandler struct {
	proxy       *Proxy
	upstreamURL string
	proxyURL    string
}

// NewPyPIHandler creates a new PyPI protocol handler.
func NewPyPIHandler(proxy *Proxy, proxyURL string) *PyPIHandler {
	return &PyPIHandler{
		proxy:       proxy,
		upstreamURL: pypiUpstream,
		proxyURL:    strings.TrimSuffix(proxyURL, "/"),
	}
}

// Routes returns the HTTP handler for PyPI requests.
func (h *PyPIHandler) Routes() http.Handler {
	mux := http.NewServeMux()

	// Simple API (used by pip)
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

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Accept", "text/html")

	resp, err := h.proxy.HTTPClient.Do(req)
	if err != nil {
		h.proxy.Logger.Error("upstream request failed", "error", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	body, err := ReadMetadata(resp.Body)
	if err != nil {
		http.Error(w, "failed to read response", http.StatusInternalServerError)
		return
	}

	// When cooldown is enabled, fetch JSON metadata to get version timestamps
	var filteredVersions map[string]bool
	if h.proxy.Cooldown != nil && h.proxy.Cooldown.Enabled() {
		filteredVersions = h.fetchFilteredVersions(r, name)
	}

	rewritten := h.rewriteSimpleHTML(body, filteredVersions)

	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(rewritten)
}

// fetchFilteredVersions fetches JSON metadata and returns a set of version strings
// that should be filtered out due to cooldown.
func (h *PyPIHandler) fetchFilteredVersions(r *http.Request, name string) map[string]bool {
	jsonURL := fmt.Sprintf("%s/pypi/%s/json", h.upstreamURL, name)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, jsonURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Accept", "application/json")

	resp, err := h.proxy.HTTPClient.Do(req)
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var metadata map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&metadata); err != nil {
		return nil
	}

	releases, ok := metadata["releases"].(map[string]any)
	if !ok {
		return nil
	}

	packagePURL := purl.MakePURLString("pypi", name, "")
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

	// Match href attributes pointing to packages
	// PyPI URLs look like: https://files.pythonhosted.org/packages/...
	re := regexp.MustCompile(`href="(https://files\.pythonhosted\.org/packages/[^"]+)"`)

	return re.ReplaceAllFunc(body, func(match []byte) []byte {
		submatch := re.FindSubmatch(match)
		if len(submatch) < minSubmatchParts {
			return match
		}

		origURL := string(submatch[1])

		u, err := url.Parse(origURL)
		if err != nil {
			return match
		}

		newURL := fmt.Sprintf("%s/pypi/packages%s", h.proxyURL, u.Path)
		return []byte(fmt.Sprintf(`href="%s"`, newURL))
	})
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
	h.proxyAndRewriteJSON(w, r, upstreamURL)
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
	h.proxyAndRewriteJSON(w, r, upstreamURL)
}

// proxyAndRewriteJSON fetches JSON metadata and rewrites download URLs.
func (h *PyPIHandler) proxyAndRewriteJSON(w http.ResponseWriter, r *http.Request, upstreamURL string) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Accept", "application/json")

	resp, err := h.proxy.HTTPClient.Do(req)
	if err != nil {
		h.proxy.Logger.Error("upstream request failed", "error", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	body, err := ReadMetadata(resp.Body)
	if err != nil {
		http.Error(w, "failed to read response", http.StatusInternalServerError)
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
		packagePURL = purl.MakePURLString("pypi", packageName, "")
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

	u, err := url.Parse(urlStr)
	if err != nil {
		return
	}

	// Only rewrite pythonhosted.org URLs
	if u.Host == "files.pythonhosted.org" {
		newURL := fmt.Sprintf("%s/pypi/packages%s", h.proxyURL, u.Path)
		entry["url"] = newURL
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

	if name == "" {
		// Can't determine name/version, use hash as identifier
		name = fmt.Sprintf("_hash_%s", hashPath(path))
		version = "0"
	}

	h.proxy.Logger.Info("pypi download request",
		"name", name, "version", version, "filename", filename)

	// Construct upstream URL; the incoming path starts with
	// '/packages' so there is no need to include it in the format
	// string
	upstreamURL := fmt.Sprintf("https://files.pythonhosted.org/%s", path)

	result, err := h.proxy.GetOrFetchArtifactFromURL(r.Context(), "pypi", name, version, filename, upstreamURL)
	if err != nil {
		h.proxy.Logger.Error("failed to get artifact", "error", err)
		http.Error(w, "failed to fetch package", http.StatusBadGateway)
		return
	}

	ServeArtifact(w, result)
}

// parseFilename extracts package name and version from a PyPI filename.
// Handles both wheels and sdists:
// - requests-2.31.0-py3-none-any.whl
// - requests-2.31.0.tar.gz
func (h *PyPIHandler) parseFilename(filename string) (name, version string) {
	// Try wheel format first: {name}-{version}(-{build})?-{python}-{abi}-{platform}.whl
	if strings.HasSuffix(filename, ".whl") {
		base := strings.TrimSuffix(filename, ".whl")
		parts := strings.Split(base, "-")
		if len(parts) >= minWheelParts {
			// Find where version ends (version followed by python tag)
			for i := 1; i < len(parts)-2; i++ {
				// Check if this looks like a python tag (py2, py3, cp39, etc)
				if isPythonTag(parts[i]) {
					name = strings.Join(parts[:i-1], "-")
					version = parts[i-1]
					return
				}
			}
		}
	}

	// Try sdist formats: {name}-{version}.tar.gz, {name}-{version}.zip
	for _, ext := range []string{".tar.gz", ".tar.bz2", ".zip", ".tar"} {
		if strings.HasSuffix(filename, ext) {
			base := strings.TrimSuffix(filename, ext)
			// Find last hyphen followed by version
			for i := len(base) - 1; i >= 0; i-- {
				if base[i] == '-' && i+1 < len(base) && isVersionStart(base[i+1]) {
					return base[:i], base[i+1:]
				}
			}
		}
	}

	return "", ""
}

func isPythonTag(s string) bool {
	if len(s) < minPythonTagLen {
		return false
	}
	// Python tags start with py, cp, pp, ip, jy
	prefixes := []string{"py", "cp", "pp", "ip", "jy"}
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
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
	req.Header.Set("Accept", "text/html")

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

	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
