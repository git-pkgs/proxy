package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	npmUpstream      = "https://registry.npmjs.org"
	npmAcceptDefault = "application/vnd.npm.install-v1+json;q=1.0, application/json;q=0.8"
	scopedParts      = 2 // scope + name in scoped packages

	// npmSecurityPrefix covers the audit endpoints: /-/npm/v1/security/audits,
	// .../audits/quick and .../advisories/bulk.
	npmSecurityPrefix = "/-/npm/v1/security/"

	// npmKeysPath serves the registry signing keys `npm audit signatures` reads.
	npmKeysPath = "/-/npm/v1/keys"

	// npmSecurityMaxBody caps the audit payload. Buffering it lets an oversized
	// body be refused before the upstream request starts.
	npmSecurityMaxBody = 16 << 20
)

// NPMHandler handles npm registry protocol requests.
type NPMHandler struct {
	proxy       *Proxy
	upstreamURL string
	proxyURL    string // URL where this proxy is hosted
}

// NewNPMHandler creates a new npm protocol handler.
func NewNPMHandler(proxy *Proxy, proxyURL, upstreamURL string) *NPMHandler {
	if strings.TrimSpace(upstreamURL) == "" {
		upstreamURL = npmUpstream
	}

	return &NPMHandler{
		proxy:       proxy,
		upstreamURL: strings.TrimSuffix(upstreamURL, "/"),
		proxyURL:    strings.TrimSuffix(proxyURL, "/"),
	}
}

// Routes returns the HTTP handler for npm requests.
// Mount this at /npm on your router.
func (h *NPMHandler) Routes() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The /-/npm/v1 endpoints share the /-/ prefix with tarball paths, so
		// they are routed before the tarball dispatch below. Audits are POSTs,
		// so they also precede the GET-only gate.
		if strings.HasPrefix(r.URL.Path, npmSecurityPrefix) {
			h.handleSecurity(w, r)
			return
		}

		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if r.URL.Path == npmKeysPath {
			h.proxy.ProxyUpstream(w, r, h.upstreamURL+npmKeysPath,
				[]string{headerAccept, headerAcceptEncoding})
			return
		}

		path := strings.TrimPrefix(r.URL.Path, "/")

		// Check if this is a tarball download (contains /-/)
		if strings.Contains(path, "/-/") {
			h.handleDownload(w, r)
			return
		}

		// Otherwise it's a metadata request
		h.handlePackageMetadata(w, r)
	})
}

// handleSecurity relays the npm audit endpoints to upstream. The request body
// is the dependency tree being audited, so no two requests share a cache key,
// and the response carries no tarball URLs to rewrite.
//
// Advisories come from upstream's database, not the proxy's own vulnerability
// data, and versions withheld by cooldown are not excluded from the report.
func (h *NPMHandler) handleSecurity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		JSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	if containsPathTraversal(r.URL.Path) {
		JSONError(w, http.StatusBadRequest, "invalid path")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, npmSecurityMaxBody))
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			JSONError(w, http.StatusRequestEntityTooLarge, "audit request too large")
			return
		}
		// A client aborting mid-upload must not be told its payload was too big.
		h.proxy.Logger.Warn("npm audit request body unreadable", "path", r.URL.Path, "error", err)
		JSONError(w, http.StatusBadRequest, "could not read audit request")
		return
	}

	// EscapedPath keeps an encoded "?" or "#" from turning the rest of the path
	// into a query or fragment upstream.
	upstreamURL := h.upstreamURL + r.URL.EscapedPath()
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	h.proxy.Logger.Info("npm audit request", "path", r.URL.Path, "bytes", len(body))

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		JSONError(w, http.StatusInternalServerError, "failed to create request")
		return
	}

	// Clients may gzip the body, so the headers describing it travel with it.
	for _, header := range []string{headerContentType, headerContentEncoding, headerAccept, headerAcceptEncoding} {
		if v := r.Header.Get(header); v != "" {
			req.Header.Set(header, v)
		}
	}
	if req.Header.Get(headerContentType) == "" {
		req.Header.Set(headerContentType, contentTypeJSON)
	}
	h.proxy.applyUpstreamAuth(req)

	resp, err := h.proxy.HTTPClient.Do(req)
	if err != nil {
		h.proxy.Logger.Error("npm audit request failed", "path", r.URL.Path, "error", err)
		JSONError(w, http.StatusBadGateway, "failed to reach upstream registry")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	h.proxy.relayResponse(w, r, resp, nil)
}

// handlePackageMetadata proxies package metadata from upstream and rewrites tarball URLs.
func (h *NPMHandler) handlePackageMetadata(w http.ResponseWriter, r *http.Request) {
	packagePath := h.extractPackageName(r)
	if packagePath == "" || containsPathTraversal(packagePath) {
		JSONError(w, http.StatusBadRequest, "invalid package name")
		return
	}
	// A scoped name is always @scope/pkg; the registry has no GET for @scope alone.
	if strings.HasPrefix(packagePath, "@") && !strings.Contains(packagePath, "/") {
		JSONError(w, http.StatusBadRequest, "invalid package name")
		return
	}
	packageName, registryPath := npmMetadataPath(packagePath)
	if packageName == "" {
		JSONError(w, http.StatusBadRequest, "invalid package name")
		return
	}

	h.proxy.Logger.Info("npm metadata request", "package", packageName, "path", packagePath)

	// A version or dist-tag is its own path segment. Escaping the whole path
	// turns "lodash/4.17.21" into "lodash%2F4.17.21", which the registry
	// rejects with 405.
	upstreamURL := h.upstreamURL + "/" + registryPath

	// Prefer the smaller abbreviated packument format but include application/json
	// as a fallback so upstreams that reject the abbreviated type (e.g. JFrog
	// Artifactory, which returns 406) can still respond with full metadata.
	// When cooldown is enabled we must use full metadata exclusively because the
	// abbreviated format omits the "time" map required for version age filtering.
	// Operators can also force full metadata so clients that gate on publish
	// age (for example Yarn's npmMinimalAgeGate) keep working through the proxy.
	accept := npmAcceptDefault
	if h.proxy.NPMFullMetadata || (h.proxy.Cooldown != nil && h.proxy.Cooldown.Enabled()) {
		accept = contentTypeJSON
	}

	if rewritten, ok := h.proxy.storedRewrite("npm", packagePath, h.proxyURL, packagePath); ok {
		w.Header().Set(headerContentType, contentTypeJSON)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(rewritten)
		return
	}

	body, _, err := h.proxy.FetchOrCacheMetadata(r.Context(), "npm", packagePath, upstreamURL, accept)
	if err != nil {
		if errors.Is(err, ErrUpstreamNotFound) {
			JSONError(w, http.StatusNotFound, "package not found")
			return
		}
		h.proxy.Logger.Error("failed to fetch npm metadata", "error", err)
		JSONError(w, http.StatusBadGateway, "failed to fetch from upstream")
		return
	}

	rewritten, err := h.proxy.cachedRewrite(r.Context(), "npm", h.proxyURL, packagePath, body, func(b []byte) ([]byte, error) {
		return h.rewriteMetadata(packageName, b)
	})
	if err != nil {
		if r.Context().Err() != nil {
			return // the client left while waiting on a shared rewrite
		}
		if errors.Is(err, ErrVersionDenied) {
			JSONError(w, http.StatusForbidden, err.Error())
			return
		}
		if len(h.proxy.Denylist.Versions(canonicalPackagePURL("npm", packageName))) != 0 {
			JSONError(w, http.StatusBadGateway, "failed to filter package metadata")
			return
		}
		// If rewriting fails, just proxy the original
		h.proxy.Logger.Warn("failed to rewrite metadata, proxying original", "error", err)
		w.Header().Set(headerContentType, contentTypeJSON)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return
	}

	w.Header().Set(headerContentType, contentTypeJSON)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(rewritten)
}

// rewriteMetadata rewrites tarball URLs in npm package metadata to point at this proxy.
// If cooldown is enabled, versions published too recently are filtered out.
//
// The document is never decoded as a whole. A full packument can run to tens
// of megabytes, and decoding it into generic maps took many times that in
// memory for every rewrite. Instead the version names, time map and dist-tags
// are decoded to decide what to keep, and the response is assembled from
// slices of the original bytes: only each version's tarball URL, and the time
// map and dist-tags when filtering changed them, are written fresh. Everything
// else reaches the client byte for byte as upstream sent it.
func (h *NPMHandler) rewriteMetadata(packageName string, body []byte) ([]byte, error) {
	doc, err := indexNPMPackument(body)
	if err != nil {
		return nil, err
	}

	versionsRaw := doc.value(doc.versionsAt)
	if len(versionsRaw) == 0 || versionsRaw[0] != '{' {
		version, ok, err := lookupJSONString(body, "version")
		if err != nil {
			return nil, err
		}
		if ok {
			if h.proxy.versionDenied("npm", packageName, version) {
				return nil, ErrVersionDenied
			}
			var out bytes.Buffer
			h.writeNPMVersion(&out, packageName, version, body)
			return out.Bytes(), nil
		}
		if len(h.proxy.Denylist.Versions(canonicalPackagePURL("npm", packageName))) != 0 {
			return nil, errors.New("npm metadata has no versions object")
		}
		return body, nil // No versions to rewrite
	}

	// The filters only look at which versions exist, so they run on a map of
	// version names alongside the decoded time map and dist-tags.
	versions := map[string]any{}
	if err := forEachJSONMember(versionsRaw, func(m jsonMember) error {
		version, err := jsonKey(m.key(versionsRaw))
		versions[version] = nil
		return err
	}); err != nil {
		return nil, err
	}
	metadata := map[string]any{}
	timeMap := doc.decodeObject(doc.timeAt, metadata, "time")
	distTags := doc.decodeObject(doc.tagsAt, metadata, "dist-tags")
	timeEntries := len(timeMap)
	originalTags := maps.Clone(distTags)

	h.applyCooldownFiltering(metadata, versions, packageName)
	h.applyDenylistFiltering(metadata, versions, packageName)

	// Untouched members are copied as they are; the three the filters
	// handled are written from their filtered state, and only when it changed.
	return doc.write(func(out *bytes.Buffer, i int) error {
		switch {
		case i == doc.versionsAt:
			return h.writeNPMVersions(out, packageName, versionsRaw, versions)
		case i == doc.timeAt && timeMap != nil && len(timeMap) != timeEntries:
			return writeFilteredJSONObject(out, doc.value(i), func(key string) bool {
				_, ok := timeMap[key]
				return ok
			})
		case i == doc.tagsAt && distTags != nil && !reflect.DeepEqual(distTags, originalTags):
			encoded, err := json.Marshal(distTags)
			out.Write(encoded)
			return err
		default:
			out.Write(doc.value(i))
			return nil
		}
	})
}

// npmPackument is a packument's top-level members as offsets into its bytes,
// with the positions of the members rewriteMetadata filters, or -1 for those
// it lacks. A key that repeats is recorded at its last occurrence, the one
// JSON.parse keeps.
type npmPackument struct {
	body                       []byte
	members                    []jsonMember
	versionsAt, timeAt, tagsAt int
}

func indexNPMPackument(body []byte) (*npmPackument, error) {
	if !json.Valid(body) {
		return nil, errors.New("npm metadata is not valid JSON")
	}
	doc := &npmPackument{body: body, versionsAt: -1, timeAt: -1, tagsAt: -1}
	err := forEachJSONMember(body, func(m jsonMember) error {
		switch key := m.key(body); {
		case jsonKeyIs(key, "versions"):
			doc.versionsAt = len(doc.members)
		case jsonKeyIs(key, "time"):
			doc.timeAt = len(doc.members)
		case jsonKeyIs(key, "dist-tags"):
			doc.tagsAt = len(doc.members)
		}
		doc.members = append(doc.members, m)
		return nil
	})
	return doc, err
}

// value returns the raw value of member i, or nil when i is -1.
func (d *npmPackument) value(i int) []byte {
	if i < 0 {
		return nil
	}
	return d.members[i].value(d.body)
}

// decodeObject decodes member i into metadata[name] and returns it when it is
// an object. The filters edit the returned map in place.
func (d *npmPackument) decodeObject(i int, metadata map[string]any, name string) map[string]any {
	raw := d.value(i)
	if raw == nil {
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	metadata[name] = value
	object, _ := value.(map[string]any)
	return object
}

// write assembles the document in member order, with writeValue writing each
// member's value. Earlier duplicates of the filtered keys are dropped: they
// would carry unfiltered data, and JSON.parse ignores them anyway.
func (d *npmPackument) write(writeValue func(out *bytes.Buffer, i int) error) ([]byte, error) {
	var out bytes.Buffer
	out.Grow(len(d.body) + len(d.body)/8)
	out.WriteByte('{')
	first := true
	for i, m := range d.members {
		if i != d.versionsAt && i != d.timeAt && i != d.tagsAt {
			key := m.key(d.body)
			if jsonKeyIs(key, "versions") || jsonKeyIs(key, "time") || jsonKeyIs(key, "dist-tags") {
				continue
			}
		}
		if !first {
			out.WriteByte(',')
		}
		first = false
		out.Write(m.key(d.body))
		out.WriteByte(':')
		if err := writeValue(&out, i); err != nil {
			return nil, err
		}
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

// writeNPMVersions writes the versions object, keeping the versions still in
// keep and pointing each kept version's tarball at this proxy.
func (h *NPMHandler) writeNPMVersions(out *bytes.Buffer, packageName string, versionsRaw []byte, keep map[string]any) error {
	out.WriteByte('{')
	first := true
	err := forEachJSONMember(versionsRaw, func(m jsonMember) error {
		version, err := jsonKey(m.key(versionsRaw))
		if err != nil {
			return err
		}
		if _, ok := keep[version]; !ok {
			return nil
		}
		if !first {
			out.WriteByte(',')
		}
		first = false
		out.Write(m.key(versionsRaw))
		out.WriteByte(':')
		h.writeNPMVersion(out, packageName, version, m.value(versionsRaw))
		return nil
	})
	out.WriteByte('}')
	return err
}

// writeNPMVersion writes one version entry with its dist.tarball pointing at
// this proxy. An entry without a string tarball is written unchanged.
func (h *NPMHandler) writeNPMVersion(out *bytes.Buffer, packageName, version string, raw []byte) {
	dist, ok, err := findJSONMember(raw, "dist")
	if err != nil || !ok {
		out.Write(raw)
		return
	}
	distRaw := dist.value(raw)
	tarball, ok, err := findJSONMember(distRaw, "tarball")
	if err != nil || !ok || distRaw[tarball.valStart] != '"' {
		out.Write(raw)
		return
	}
	var oldTarball string
	if err := json.Unmarshal(tarball.value(distRaw), &oldTarball); err != nil {
		out.Write(raw)
		return
	}

	newTarball := h.proxyTarballURL(packageName, version, oldTarball)
	encoded, err := json.Marshal(newTarball)
	if err != nil {
		out.Write(raw)
		return
	}
	out.Write(raw[:dist.valStart+tarball.valStart])
	out.Write(encoded)
	out.Write(raw[dist.valStart+tarball.valEnd:])

	h.proxy.Logger.Debug("rewrote tarball URL",
		"package", packageName, "version", version,
		"old", oldTarball, "new", newTarball)
}

// writeFilteredJSONObject writes the object obj holds, keeping the members
// whose decoded key keep accepts.
func writeFilteredJSONObject(out *bytes.Buffer, obj []byte, keep func(string) bool) error {
	out.WriteByte('{')
	first := true
	err := forEachJSONMember(obj, func(m jsonMember) error {
		key, err := jsonKey(m.key(obj))
		if err != nil {
			return err
		}
		if !keep(key) {
			return nil
		}
		if !first {
			out.WriteByte(',')
		}
		first = false
		out.Write(obj[m.keyStart:m.valEnd])
		return nil
	})
	out.WriteByte('}')
	return err
}

// applyCooldownFiltering removes versions that are too recently published,
// and updates dist-tags.latest if the current latest was filtered out.
func (h *NPMHandler) applyCooldownFiltering(metadata map[string]any, versions map[string]any, packageName string) {
	if h.proxy.Cooldown == nil || !h.proxy.Cooldown.Enabled() {
		return
	}

	timeMap, _ := metadata["time"].(map[string]any)
	if timeMap == nil {
		return
	}

	packagePURL := canonicalPackagePURL("npm", packageName)

	for version := range versions {
		publishedStr, ok := timeMap[version].(string)
		if !ok {
			continue
		}
		publishedAt, err := time.Parse(time.RFC3339, publishedStr)
		if err != nil {
			continue
		}
		if !h.proxy.Cooldown.IsAllowed("npm", packagePURL, publishedAt) {
			h.proxy.Logger.Info("cooldown: filtering npm version",
				"package", packageName, "version", version,
				"published", publishedStr)
			delete(versions, version)
			delete(timeMap, version)
		}
	}

	h.updateDistTagsLatest(metadata, versions, timeMap)
}

// updateDistTagsLatest updates the dist-tags.latest field if the current latest
// version was removed by cooldown filtering.
func (h *NPMHandler) updateDistTagsLatest(metadata, versions, timeMap map[string]any) {
	distTags, ok := metadata["dist-tags"].(map[string]any)
	if !ok {
		return
	}

	latest, ok := distTags["latest"].(string)
	if !ok {
		return
	}

	if _, exists := versions[latest]; exists {
		return
	}

	if newLatest := h.findNewestVersion(versions, timeMap); newLatest != "" {
		distTags["latest"] = newLatest
	}
}

// proxyTarballURL returns the proxy URL that serves the tarball upstream
// lists at tarball for this version.
func (h *NPMHandler) proxyTarballURL(packageName, version, tarball string) string {
	filename := tarball
	if idx := strings.LastIndex(tarball, "/"); idx >= 0 {
		filename = tarball[idx+1:]
	}
	if h.extractVersionFromFilename(packageName, filename) != version {
		_, shortName, scoped := strings.Cut(packageName, "/")
		if !scoped {
			shortName = packageName
		}
		filename = shortName + "-" + version + ".tgz"
	}

	return fmt.Sprintf("%s/npm/%s/-/%s", h.proxyURL, url.PathEscape(packageName), filename)
}

// findNewestVersion returns the version string with the most recent timestamp
// from the remaining versions, using the time map.
func (h *NPMHandler) findNewestVersion(versions map[string]any, timeMap map[string]any) string {
	if timeMap == nil {
		return ""
	}

	type versionTime struct {
		version string
		t       time.Time
	}

	var vts []versionTime
	for v := range versions {
		if ts, ok := timeMap[v].(string); ok {
			if t, err := time.Parse(time.RFC3339, ts); err == nil {
				vts = append(vts, versionTime{v, t})
			}
		}
	}

	if len(vts) == 0 {
		return ""
	}

	sort.Slice(vts, func(i, j int) bool {
		return vts[i].t.After(vts[j].t)
	})

	return vts[0].version
}

// handleDownload serves a package tarball, fetching and caching from upstream if needed.
func (h *NPMHandler) handleDownload(w http.ResponseWriter, r *http.Request) {
	packageName, filename := h.parseDownloadPath(r.URL.Path)

	if packageName == "" || filename == "" {
		JSONError(w, http.StatusBadRequest, "invalid request")
		return
	}

	// Extract version from filename (e.g., "lodash-4.17.21.tgz" -> "4.17.21")
	version := h.extractVersionFromFilename(packageName, filename)
	if version == "" {
		JSONError(w, http.StatusBadRequest, "could not determine version from filename")
		return
	}

	h.proxy.Logger.Info("npm download request",
		"package", packageName, "version", version, "filename", filename)

	if h.proxy.versionDenied("npm", packageName, version) {
		JSONError(w, http.StatusForbidden, ErrVersionDenied.Error()+": "+canonicalVersionPURL("npm", packageName, version))
		return
	}
	metadata := sync.OnceValues(func() ([]byte, error) {
		upstreamURL := h.upstreamURL + "/" + url.PathEscape(packageName)
		body, _, err := h.proxy.FetchOrCacheMetadata(r.Context(), "npm", packageName, upstreamURL, contentTypeJSON)
		return body, err
	})
	if h.versionInCooldown(packageName, version, metadata) {
		h.proxy.Logger.Info("cooldown: withholding npm tarball",
			"package", packageName, "version", version)
		JSONError(w, http.StatusNotFound, "version not found")
		return
	}

	result, err := h.getTarball(r, packageName, version, filename, metadata)
	if err != nil {
		switch {
		case errors.Is(err, ErrUpstreamNotFound):
			JSONError(w, http.StatusNotFound, "package not found")
		case errors.Is(err, ErrArtifactBlocked), errors.Is(err, ErrVersionDenied):
			JSONError(w, http.StatusForbidden, err.Error())
		default:
			h.proxy.Logger.Error("failed to get artifact", "error", err)
			JSONError(w, http.StatusBadGateway, "failed to fetch package")
		}
		return
	}

	ServeArtifactRequest(w, r, result)
}

func (h *NPMHandler) getTarball(r *http.Request, packageName, version, filename string, metadata func() ([]byte, error)) (*CacheResult, error) {
	if cached, err := h.proxy.GetCachedArtifact(r.Context(), "npm", packageName, version, filename); err != nil || cached != nil {
		return cached, err
	}
	downloadURL := fmt.Sprintf("%s/%s/-/%s", h.upstreamURL, escapeNPMDownloadPackage(packageName), url.PathEscape(filename))
	body, err := metadata()
	if err == nil {
		if tarball := npmVersionTarball(body, version); tarball != "" {
			downloadURL, err = h.validateTarballURL(tarball)
			if err != nil {
				return nil, err
			}
		}
	}
	return h.proxy.GetOrFetchArtifactFromURL(r.Context(), "npm", packageName, version, filename, downloadURL)
}

// npmVersionTarball returns versions[version].dist.tarball from a packument,
// or "" if it has none. It reads that one value in place rather than decoding
// every version, since it runs on each tarball the proxy has not cached yet.
func npmVersionTarball(body []byte, version string) string {
	tarball, _, _ := lookupJSONString(body, "versions", version, "dist", "tarball")
	return tarball
}

func (h *NPMHandler) validateTarballURL(raw string) (string, error) {
	tarball, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parsing npm tarball URL: %w", err)
	}
	upstream, err := url.Parse(h.upstreamURL)
	if err != nil {
		return "", fmt.Errorf("parsing npm upstream URL: %w", err)
	}
	if tarball.User != nil || tarball.Fragment != "" ||
		tarball.Scheme != upstream.Scheme || !strings.EqualFold(tarball.Host, upstream.Host) {
		return "", errors.New("npm tarball URL does not match upstream registry")
	}
	if containsPathTraversal(tarball.Path) || strings.Contains(tarball.Path, "\\") {
		return "", errors.New("npm tarball URL contains path traversal")
	}
	basePath := strings.TrimRight(upstream.Path, "/")
	if basePath != "" && !strings.HasPrefix(tarball.Path, basePath+"/") {
		return "", errors.New("npm tarball URL is outside upstream base path")
	}
	return tarball.String(), nil
}

// versionInCooldown reports whether a version is still inside the cooldown
// window. Filtering the packument is not enough on its own: tarball URLs are
// predictable and lockfiles record them directly, so `npm ci` reaches the
// download path without ever requesting metadata.
//
// A version's publish time is immutable, so the check reads the stored
// versions row first and only falls back to the packument for a version the
// proxy has never seen, persisting the parsed time so the packument is
// fetched and parsed at most once per version. A version with no usable
// publish time is allowed through, matching how applyCooldownFiltering
// treats it.
func (h *NPMHandler) versionInCooldown(packageName, version string, metadata func() ([]byte, error)) bool {
	if h.proxy.Cooldown == nil || !h.proxy.Cooldown.Enabled() {
		return false
	}

	versionPURL := canonicalVersionPURL("npm", packageName, version)
	if ver, err := h.proxy.DB.GetVersionByPURL(versionPURL); err == nil && ver != nil && ver.PublishedAt.Valid {
		return !h.proxy.Cooldown.IsAllowed("npm", canonicalPackagePURL("npm", packageName), ver.PublishedAt.Time)
	}

	body, err := metadata()
	if err != nil {
		h.proxy.Logger.Warn("cooldown: could not fetch npm metadata for download check",
			"package", packageName, "version", version, "error", err)
		return false
	}

	published, ok, err := lookupJSONString(body, "time", version)
	if err != nil {
		h.proxy.Logger.Warn("cooldown: could not parse npm metadata for download check",
			"package", packageName, "version", version, "error", err)
		return false
	}
	if !ok {
		return false
	}

	publishedAt, err := time.Parse(time.RFC3339, published)
	if err != nil {
		return false
	}

	if err := h.proxy.DB.SetVersionPublishedAt(versionPURL, canonicalPackagePURL("npm", packageName), publishedAt); err != nil {
		h.proxy.Logger.Warn("cooldown: could not store npm publish time",
			"package", packageName, "version", version, "error", err)
	}

	return !h.proxy.Cooldown.IsAllowed("npm", canonicalPackagePURL("npm", packageName), publishedAt)
}

// npmMetadataPath returns the package name and the encoded registry path.
// Examples: lodash → lodash; lodash/4.17.21 → lodash/4.17.21;
// @babel/core → @babel%2Fcore; @babel/core/7.23.0 → @babel%2Fcore/7.23.0.
func npmMetadataPath(packagePath string) (packageName, registryPath string) {
	// GET /npm/ has no package segment; handlePackageMetadata rejects that earlier.
	if packagePath == "" {
		return "", ""
	}
	if !strings.Contains(packagePath, "/") {
		return packagePath, url.PathEscape(packagePath)
	}
	if strings.HasPrefix(packagePath, "@") {
		scope, rest, _ := strings.Cut(packagePath, "/")
		name, version, ok := strings.Cut(rest, "/")
		packageName = scope + "/" + name
		registryPath = url.PathEscape(scope) + "%2F" + url.PathEscape(name)
		if ok {
			registryPath += "/" + url.PathEscape(version)
		}
		return packageName, registryPath
	}
	name, version, _ := strings.Cut(packagePath, "/")
	return name, url.PathEscape(name) + "/" + url.PathEscape(version)
}

func escapeNPMDownloadPackage(packageName string) string {
	scope, name, scoped := strings.Cut(packageName, "/")
	if scoped && strings.HasPrefix(scope, "@") && len(scope) > 1 && name != "" && !strings.Contains(name, "/") {
		return url.PathEscape(scope) + "/" + url.PathEscape(name)
	}
	return url.PathEscape(packageName)
}

// extractPackageName extracts the package name from the request path.
// Handles both scoped (@scope/name) and unscoped (name) packages.
func (h *NPMHandler) extractPackageName(r *http.Request) string {
	path := strings.TrimPrefix(r.URL.Path, "/")

	// Remove /-/filename suffix if present
	if idx := strings.Index(path, "/-/"); idx >= 0 {
		path = path[:idx]
	}

	// URL decode the path (handles %40 -> @, %2f -> /)
	decoded, err := url.PathUnescape(path)
	if err != nil {
		return path
	}

	return decoded
}

// parseDownloadPath extracts package name and filename from a download path.
// Path format: /@scope/name/-/filename.tgz or /name/-/filename.tgz
func (h *NPMHandler) parseDownloadPath(path string) (packageName, filename string) {
	path = strings.TrimPrefix(path, "/")

	idx := strings.Index(path, "/-/")
	if idx < 0 {
		return "", ""
	}

	packageName = path[:idx]
	filename = path[idx+3:] // skip "/-/"

	// URL decode package name
	if decoded, err := url.PathUnescape(packageName); err == nil {
		packageName = decoded
	}

	return packageName, filename
}

// extractVersionFromFilename extracts version from npm tarball filename.
// e.g., "lodash-4.17.21.tgz" -> "4.17.21"
// e.g., "core-7.23.0.tgz" for @babel/core -> "7.23.0"
func (h *NPMHandler) extractVersionFromFilename(packageName, filename string) string {
	// Remove .tgz extension
	if !strings.HasSuffix(filename, ".tgz") {
		return ""
	}
	base := strings.TrimSuffix(filename, ".tgz")

	// For scoped packages, the filename uses the short name
	shortName := packageName
	if strings.Contains(packageName, "/") {
		parts := strings.SplitN(packageName, "/", scopedParts)
		shortName = parts[1]
	}

	// Expected format: {shortName}-{version}
	prefix := shortName + "-"
	if !strings.HasPrefix(base, prefix) {
		return ""
	}

	return strings.TrimPrefix(base, prefix)
}
