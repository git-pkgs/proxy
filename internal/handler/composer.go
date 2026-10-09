package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"
)

const (
	composerUpstream   = "https://packagist.org"
	composerRepo       = "https://repo.packagist.org"
	composerMinified   = "composer/2.0"
	composerUnset      = "__unset"
	composerDevReset   = "~dev"
	vendorPackageParts = 2
)

var composerUnsetJSON = []byte(`"` + composerUnset + `"`)

// ComposerHandler handles Composer/Packagist registry protocol requests.
type ComposerHandler struct {
	proxy       *Proxy
	upstreamURL string
	repoURL     string
	proxyURL    string
	routes      packageRoutes
}

// NewComposerHandler creates a new Composer protocol handler.
func NewComposerHandler(proxy *Proxy, proxyURL string) *ComposerHandler {
	return &ComposerHandler{
		proxy:       proxy,
		upstreamURL: composerUpstream,
		repoURL:     composerRepo,
		proxyURL:    strings.TrimSuffix(proxyURL, "/"),
	}
}

// NewComposerHandlerWithUpstreams creates a Composer handler with custom API
// and repository upstreams.
func NewComposerHandlerWithUpstreams(proxy *Proxy, proxyURL, upstreamURL, repoURL string) *ComposerHandler {
	h := NewComposerHandler(proxy, proxyURL)
	h.upstreamURL = configuredUpstreamURL(upstreamURL, composerUpstream)
	h.repoURL = configuredUpstreamURL(repoURL, composerRepo)
	return h
}

// WithPackageRoutes sends packages matching a pattern to a dedicated Composer
// repository instead of the default one. See packageRoutes.
func (h *ComposerHandler) WithPackageRoutes(routes map[string]string) *ComposerHandler {
	h.routes = newPackageRoutes(routes)
	return h
}

// Routes returns the HTTP handler for Composer requests.
func (h *ComposerHandler) Routes() http.Handler {
	mux := http.NewServeMux()

	// Service index
	mux.HandleFunc("GET /packages.json", h.handleServiceIndex)

	// Package metadata (Composer v2 format) - use prefix since {package}.json isn't allowed
	mux.HandleFunc("GET /p2/", h.handlePackageMetadata)

	// Package downloads
	mux.HandleFunc("GET /files/{vendor}/{package}/{version}/{filename}", h.handleDownload)

	// Search and list (proxy without modification)
	mux.HandleFunc("GET /search.json", h.proxyUpstream)
	mux.HandleFunc("GET /packages/list.json", h.proxyUpstream)

	return mux
}

// handleServiceIndex returns the Composer repository service index.
func (h *ComposerHandler) handleServiceIndex(w http.ResponseWriter, r *http.Request) {
	// Return a minimal service index pointing to our proxy
	index := map[string]any{
		"packages":           map[string]any{},
		"metadata-url":       h.proxyURL + "/composer/p2/%package%.json",
		"notify-batch":       h.upstreamURL + "/downloads/",
		"search":             h.proxyURL + "/composer/search.json?q=%query%&type=%type%",
		"providers-lazy-url": h.proxyURL + "/composer/p2/%package%.json",
	}

	w.Header().Set(headerContentType, "application/json")
	_ = json.NewEncoder(w).Encode(index)
}

// sourceFor returns the repository serving a package and the key its metadata
// is cached under.
func (h *ComposerHandler) sourceFor(packageName string) (repoURL, cacheKey string) {
	if route, routed := h.routes.match(packageName); routed {
		return route.url, route.cacheKey(packageName)
	}
	return h.repoURL, packageName
}

// handlePackageMetadata proxies and rewrites package metadata.
func (h *ComposerHandler) handlePackageMetadata(w http.ResponseWriter, r *http.Request) {
	// Parse path: /p2/{vendor}/{package}.json
	path := strings.TrimPrefix(r.URL.Path, "/p2/")
	path = strings.TrimSuffix(path, ".json")
	parts := strings.SplitN(path, "/", vendorPackageParts)
	if len(parts) != vendorPackageParts || parts[0] == "" || parts[1] == "" {
		http.Error(w, "invalid package path", http.StatusBadRequest)
		return
	}
	vendor := parts[0]
	pkg := parts[1]
	packageName := vendor + "/" + pkg

	h.proxy.Logger.Info("composer metadata request", "package", packageName)

	repoURL, cacheKey := h.sourceFor(packageName)
	upstreamURL := fmt.Sprintf("%s/p2/%s/%s.json", repoURL, vendor, pkg)

	if rewritten, ok := h.proxy.storedRewrite("composer", cacheKey, h.proxyURL, cacheKey); ok {
		w.Header().Set(headerContentType, "application/json")
		_, _ = w.Write(rewritten)
		return
	}

	body, _, err := h.proxy.FetchOrCacheMetadata(r.Context(), "composer", cacheKey, upstreamURL)
	if err != nil {
		if errors.Is(err, ErrUpstreamNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		h.proxy.Logger.Error("upstream request failed", "error", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}

	rewritten, err := h.proxy.cachedRewrite(r.Context(), "composer", h.proxyURL, cacheKey, body, h.rewriteMetadata)
	if err != nil {
		if r.Context().Err() != nil {
			return // the client left while waiting on a shared rewrite
		}
		h.proxy.Logger.Warn("failed to rewrite metadata, proxying original", "error", err)
		w.Header().Set(headerContentType, "application/json")
		_, _ = w.Write(body)
		return
	}

	w.Header().Set(headerContentType, "application/json")
	_, _ = w.Write(rewritten)
}

// rewriteMetadata rewrites dist URLs in Composer metadata to point at this proxy.
// If cooldown is enabled, versions published too recently are filtered out.
//
// The document is never decoded as a whole. Packagist serves large packages
// as minified lists of thousands of versions, and expanding them into generic
// maps took many times the document's size for every rewrite. Instead each
// version list is walked in place and the response is assembled from slices
// of the original bytes: only dist values are written fresh, and minified
// metadata stays minified.
func (h *ComposerHandler) rewriteMetadata(body []byte) ([]byte, error) {
	if !json.Valid(body) {
		return nil, errors.New("composer metadata is not valid JSON")
	}
	format, _, err := lookupJSONString(body, "minified")
	if err != nil {
		return nil, err
	}
	minified := format == composerMinified

	var out bytes.Buffer
	out.Grow(len(body) + len(body)/8)
	err = rewriteJSONMembers(&out, body, func(out *bytes.Buffer, m jsonMember) (bool, error) {
		packages := m.value(body)
		if !jsonKeyIs(m.key(body), "packages") || packages[0] != '{' {
			return false, nil
		}
		return true, rewriteJSONMembers(out, packages, func(out *bytes.Buffer, p jsonMember) (bool, error) {
			versions := p.value(packages)
			if versions[0] != '[' {
				return false, nil
			}
			packageName, err := jsonKey(p.key(packages))
			if err != nil {
				return false, err
			}
			return true, h.writeVersions(out, packageName, versions, minified)
		})
	})
	if err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// writeVersions writes one package's version list without the versions in
// cooldown and with each dist pointing at this proxy.
//
// Minified lists stay minified. Each version written carries the fields that
// differ from what the client has expanded so far, which is the upstream
// entry itself unless filtered versions were dropped before it; then it also
// carries the changes those versions made. Every version gets its own dist,
// since the proxied URL names the version.
func (h *ComposerHandler) writeVersions(out *bytes.Buffer, packageName string, versions []byte, minified bool) error {
	packagePURL := canonicalPackagePURL("composer", packageName)
	upstream, written := newComposerFields(), newComposerFields()
	devReset, first := false, true
	_, routed := h.routes.match(packageName)

	out.WriteByte('[')
	err := forEachJSONElement(versions, func(entry []byte) error {
		if minified && jsonStringIs(entry, composerDevReset) {
			upstream.reset()
			written.reset()
			devReset = true
			return nil
		}
		if entry[0] != '{' {
			return nil
		}
		if !minified {
			upstream.reset()
			written.reset()
		}
		if err := upstream.apply(entry, minified); err != nil {
			return err
		}

		version := upstream.string("version")
		if h.shouldFilterVersion(packagePURL, packageName, version, upstream.get("time")) {
			return nil
		}

		if !first {
			out.WriteByte(',')
		}
		first = false
		if devReset {
			out.WriteString(`"` + composerDevReset + `",`)
			devReset = false
		}
		if routed {
			disableDefaultNotification(upstream)
		}
		h.writeVersion(out, packageName, version, upstream, written)
		return nil
	})
	out.WriteByte(']')
	return err
}

// writeVersion writes the fields of upstream that differ from written, with
// dist pointing at this proxy, and "__unset" for those written has and
// upstream lacks. written is then what the client expands this version to.
func (h *ComposerHandler) writeVersion(out *bytes.Buffer, packageName, version string, upstream, written *composerFields) {
	out.WriteByte('{')
	first := true
	writeMember := func(key, value []byte) {
		if !first {
			out.WriteByte(',')
		}
		first = false
		out.Write(key)
		out.WriteByte(':')
		out.Write(value)
	}

	for _, f := range upstream.fields {
		value := f.value
		if value == nil {
			continue
		}
		if f.name == "dist" {
			value = h.rewriteDist(packageName, version, value)
		}
		if bytes.Equal(value, written.get(f.name)) {
			continue
		}
		writeMember(f.key, value)
		written.set(f.name, f.key, value)
	}
	for _, f := range written.fields {
		if f.value != nil && upstream.get(f.name) == nil {
			writeMember(f.key, composerUnsetJSON)
			written.set(f.name, f.key, nil)
		}
	}
	out.WriteByte('}')
}

// shouldFilterVersion returns true if the version should be excluded due to cooldown.
func (h *ComposerHandler) shouldFilterVersion(packagePURL, packageName, version string, published []byte) bool {
	if h.proxy.Cooldown == nil || !h.proxy.Cooldown.Enabled() {
		return false
	}

	var timeStr string
	if err := json.Unmarshal(published, &timeStr); err != nil {
		return false
	}

	publishedAt, err := time.Parse(time.RFC3339, timeStr)
	if err != nil {
		return false
	}

	if !h.proxy.Cooldown.IsAllowed("composer", packagePURL, publishedAt) {
		h.proxy.Logger.Info("cooldown: filtering composer version",
			"package", packageName, "version", version)
		return true
	}

	return false
}

// disableDefaultNotification stops Composer from reporting installs of a routed
// package to the default repository's notify-batch URL, which would disclose
// the package name to it. Composer only falls back to notify-batch when a
// version has no notification-url of its own, and skips an empty one. Setting
// it on the expanded fields keeps it set for the versions that inherit them.
func disableDefaultNotification(fields *composerFields) {
	if fields.get("notification-url") == nil {
		fields.set("notification-url", []byte(`"notification-url"`), []byte(`""`))
	}
}

// rewriteDist returns a version's dist object with its url pointing at this
// proxy. A dist without a string url is returned unchanged.
func (h *ComposerHandler) rewriteDist(packageName, version string, dist []byte) []byte {
	urlMember, ok, err := findJSONMember(dist, "url")
	if err != nil || !ok || dist[urlMember.valStart] != '"' {
		return dist
	}
	var upstreamURL string
	if err := json.Unmarshal(urlMember.value(dist), &upstreamURL); err != nil {
		return dist
	}
	distType, _, _ := lookupJSONString(dist, "type")

	proxyURL, ok := h.proxyDistURL(packageName, version, upstreamURL, distType)
	if !ok {
		return dist
	}
	encoded, err := json.Marshal(proxyURL)
	if err != nil {
		return dist
	}
	rewritten := make([]byte, 0, len(dist)-len(urlMember.value(dist))+len(encoded))
	rewritten = append(rewritten, dist[:urlMember.valStart]...)
	rewritten = append(rewritten, encoded...)
	return append(rewritten, dist[urlMember.valEnd:]...)
}

// proxyDistURL returns the URL this proxy serves a version's archive at, or
// false when the upstream URL or package name gives nothing to build it from.
func (h *ComposerHandler) proxyDistURL(packageName, version, upstreamURL, distType string) (string, bool) {
	if upstreamURL == "" {
		return "", false
	}

	filename := "package.zip"
	if idx := strings.LastIndex(upstreamURL, "/"); idx >= 0 {
		filename = upstreamURL[idx+1:]
	}

	// GitHub zipball URLs end with a bare commit hash (no extension).
	// Append .zip so the archives library can detect the format.
	if path.Ext(filename) == "" && distType == "zip" {
		filename += ".zip"
	}

	parts := strings.SplitN(packageName, "/", vendorPackageParts)
	if len(parts) != vendorPackageParts {
		return "", false
	}
	return fmt.Sprintf("%s/composer/files/%s/%s/%s/%s",
		h.proxyURL, parts[0], parts[1], version, filename), true
}

// composerFields holds one version's fields as Composer sees them after
// expanding the minified format: raw values under their decoded names, in the
// order the names first appeared. A removed field keeps its slot with a nil
// value, so the order stays stable when it comes back.
type composerFields struct {
	fields []composerField
	index  map[string]int
}

type composerField struct {
	name       string
	key, value []byte
}

func newComposerFields() *composerFields {
	return &composerFields{index: map[string]int{}}
}

func (f *composerFields) reset() {
	f.fields = f.fields[:0]
	clear(f.index)
}

// get returns the raw value of the named field, or nil when it is not set.
func (f *composerFields) get(name string) []byte {
	if i, ok := f.index[name]; ok {
		return f.fields[i].value
	}
	return nil
}

// string returns the named field when it is a string, or "".
func (f *composerFields) string(name string) string {
	raw := f.get(name)
	if len(raw) == 0 || raw[0] != '"' {
		return ""
	}
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

// set stores value under name, written with the quoted key. A nil value
// removes the field.
func (f *composerFields) set(name string, key, value []byte) {
	if i, ok := f.index[name]; ok {
		f.fields[i].key, f.fields[i].value = key, value
		return
	}
	f.index[name] = len(f.fields)
	f.fields = append(f.fields, composerField{name: name, key: key, value: value})
}

// apply overlays the members of a version entry. In minified metadata the
// value "__unset" removes the field instead.
func (f *composerFields) apply(entry []byte, minified bool) error {
	return forEachJSONMember(entry, func(m jsonMember) error {
		key, value := m.key(entry), m.value(entry)
		if minified && jsonStringIs(value, composerUnset) {
			value = nil
		}
		if bytes.IndexByte(key, '\\') < 0 {
			if i, ok := f.index[string(key[1:len(key)-1])]; ok {
				f.fields[i].key, f.fields[i].value = key, value
				return nil
			}
		}
		name, err := jsonKey(key)
		if err != nil {
			return err
		}
		f.set(name, key, value)
		return nil
	})
}

// handleDownload serves a package file, fetching and caching from upstream if needed.
func (h *ComposerHandler) handleDownload(w http.ResponseWriter, r *http.Request) {
	vendor := r.PathValue("vendor")
	pkg := r.PathValue("package")
	version := r.PathValue("version")
	filename := r.PathValue("filename")

	packageName := vendor + "/" + pkg

	h.proxy.Logger.Info("composer download request",
		"package", packageName, "version", version, "filename", filename)

	// We need to fetch the metadata to get the actual download URL since
	// Packagist URLs include a hash. Packagist serves dev versions (e.g.
	// "3.x-dev", "dev-master") from a separate "~dev" metadata file, while
	// tagged releases live in the regular file. Try the file most likely to
	// contain this version first, then fall back to the other so that both
	// stable and dev versions resolve correctly.
	metaURLs := h.metadataURLsForVersion(vendor, pkg, version)

	h.proxy.Logger.Debug("resolving download URL",
		"package", packageName, "version", version,
		"metadata_urls", metaURLs)

	var downloadURL string
	for _, metaURL := range metaURLs {
		url, err := h.findDownloadURLFromMetadata(r.Context(), metaURL, packageName, version)
		if err != nil {
			h.proxy.Logger.Error("failed to fetch metadata", "error", err, "url", metaURL)
			http.Error(w, "failed to fetch metadata", http.StatusBadGateway)
			return
		}
		if url != "" {
			downloadURL = url
			break
		}
	}

	if downloadURL == "" {
		h.proxy.Logger.Debug("version not found in any metadata source",
			"package", packageName, "version", version,
			"tried_urls", metaURLs)
		http.Error(w, "version not found", http.StatusNotFound)
		return
	}

	h.proxy.Logger.Debug("resolved download URL",
		"package", packageName, "version", version,
		"download_url", downloadURL)

	cacheFilename := filename
	if route, routed := h.routes.match(packageName); routed {
		cacheFilename = route.cacheFilename(filename)
	}

	result, err := h.proxy.GetOrFetchArtifactFromURL(r.Context(), "composer", packageName, version, cacheFilename, downloadURL)
	if err != nil {
		h.proxy.serveArtifactError(w, err, "failed to fetch package")
		return
	}

	ServeArtifactRequest(w, r, result)
}

// isDevVersion reports whether a Composer version string refers to a
// development (unstable, branch) version rather than a tagged release.
// Composer formats these as either "dev-<branch>" (e.g. "dev-master") or
// "<alias>-dev" (e.g. "3.x-dev").
func isDevVersion(version string) bool {
	return strings.HasPrefix(version, "dev-") || strings.HasSuffix(version, "-dev")
}

// metadataURLsForVersion returns the upstream metadata URLs to consult for a
// given version, in priority order. Dev versions are served from the "~dev"
// file, tagged releases from the regular file; the other file is included as a
// fallback so an unexpected classification still resolves.
func (h *ComposerHandler) metadataURLsForVersion(vendor, pkg, version string) []string {
	repoURL, _ := h.sourceFor(vendor + "/" + pkg)
	stable := fmt.Sprintf("%s/p2/%s/%s.json", repoURL, vendor, pkg)
	dev := fmt.Sprintf("%s/p2/%s/%s~dev.json", repoURL, vendor, pkg)

	if isDevVersion(version) {
		return []string{dev, stable}
	}
	return []string{stable, dev}
}

// findDownloadURLFromMetadata fetches a metadata document and returns the dist
// URL for the given version, or an empty string if the version is not present.
// An error is returned only on transport failure; a missing document (non-200)
// or a missing version both yield an empty string so the caller can fall back.
func (h *ComposerHandler) findDownloadURLFromMetadata(ctx context.Context, metaURL, packageName, version string) (string, error) {
	h.proxy.Logger.Debug("fetching upstream metadata for download lookup",
		"url", metaURL, "package", packageName, "version", version)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metaURL, nil)
	if err != nil {
		return "", err
	}

	resp, err := h.proxy.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	h.proxy.Logger.Debug("upstream metadata response",
		"url", metaURL, "status", resp.StatusCode)

	if resp.StatusCode != http.StatusOK {
		return "", nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	url, err := composerDistURL(body, packageName, version)
	if err != nil {
		return "", err
	}
	h.proxy.Logger.Debug("download URL lookup result",
		"url", metaURL, "package", packageName, "version", version,
		"download_url", url)
	return url, nil
}

// composerDistURL returns the upstream dist URL of version from Composer
// metadata, or "" when the package has no such version or it has no dist URL.
// It walks the version list in place, expanding only as much of the minified
// format as it needs, since it runs on each archive the proxy has not cached
// yet.
func composerDistURL(body []byte, packageName, version string) (string, error) {
	format, _, err := lookupJSONString(body, "minified")
	if err != nil {
		return "", err
	}
	versions, err := lookupJSON(body, "packages", packageName)
	if err != nil || len(versions) == 0 || versions[0] != '[' {
		return "", err
	}
	minified := format == composerMinified

	fields := newComposerFields()
	var url string
	err = forEachJSONElement(versions, func(entry []byte) error {
		if minified && jsonStringIs(entry, composerDevReset) {
			fields.reset()
			return nil
		}
		if entry[0] != '{' {
			return nil
		}
		if !minified {
			fields.reset()
		}
		if err := fields.apply(entry, minified); err != nil {
			return err
		}
		if !jsonStringIs(fields.get("version"), version) {
			return nil
		}
		url, _, _ = lookupJSONString(fields.get("dist"), "url")
		if url != "" {
			return errStopScan
		}
		return nil
	})
	if errors.Is(err, errStopScan) {
		err = nil
	}
	return url, err
}

// proxyUpstream forwards a request to packagist.org without caching.
func (h *ComposerHandler) proxyUpstream(w http.ResponseWriter, r *http.Request) {
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

	resp, err := h.proxy.HTTPClient.Do(req)
	if err != nil {
		h.proxy.Logger.Error("upstream request failed", "error", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	h.proxy.relayResponse(w, r, resp, nil)
}
