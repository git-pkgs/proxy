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
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/git-pkgs/integrity"
	"github.com/git-pkgs/proxy/internal/retention"
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

// npm tarballs are stored under pkg:npm/{name} as purl builds it, so package
// rules use the default key mapping.
func init() {
	retention.Default.Register(retention.Spec{Key: "npm", CanonicalPackage: retention.DefaultCanonical("npm")})
}

// NPMHandler handles npm registry protocol requests.
type NPMHandler struct {
	proxy       *Proxy
	upstreamURL string
	proxyURL    string // URL where this proxy is hosted
	routes      packageRoutes
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

// WithPackageRoutes sends packages matching a pattern to a dedicated npm
// registry instead of the default one. See packageRoutes.
func (h *NPMHandler) WithPackageRoutes(routes map[string]string) *NPMHandler {
	h.routes = newPackageRoutes(routes)
	return h
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

	resp, err := h.proxy.HTTPClient.Do(req)
	if err != nil {
		if writeUpstreamHeld(w, err) {
			return
		}
		h.proxy.Logger.Error("npm audit request failed", "path", r.URL.Path, "error", err)
		JSONError(w, http.StatusBadGateway, "failed to reach upstream registry")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	h.proxy.relayResponse(w, r, resp, nil)
}

// sourceFor returns the registry serving a package and the key its metadata is
// cached under.
func (h *NPMHandler) sourceFor(packageName string) (registryURL, cacheKey string) {
	if route, routed := h.routes.match(packageName); routed {
		return route.url, route.cacheKey(packageName)
	}
	return h.upstreamURL, packageName
}

// handlePackageMetadata proxies package metadata from upstream and rewrites tarball URLs.
func (h *NPMHandler) handlePackageMetadata(w http.ResponseWriter, r *http.Request) {
	packagePath := h.extractPackageName(r)
	if packagePath == "" || containsPathTraversal(packagePath) {
		JSONError(w, http.StatusBadRequest, "invalid package name")
		return
	}
	packageName, registryPath := npmMetadataPath(packagePath)
	if packageName == "" {
		JSONError(w, http.StatusBadRequest, "invalid package name")
		return
	}

	h.proxy.Logger.Info("npm metadata request", "package", packageName, "path", packagePath)

	registryURL, cacheKey := h.sourceFor(packageName)
	// A version or dist-tag is its own path segment. Escaping the whole path
	// turns "lodash/4.17.21" into "lodash%2F4.17.21", which the registry
	// rejects with 405.
	upstreamURL := registryURL + "/" + registryPath
	metadataCacheKey := npmMetadataCacheKey(cacheKey, packagePath, packageName)

	accept := h.packumentAccept()

	specifier, versionLookup := npmVersionSpecifier(packagePath, packageName)
	if h.serveLocalMetadata(w, r, npmMetadataRequest{packageName, specifier, versionLookup, upstreamURL, accept, metadataCacheKey}) {
		return
	}

	if stored, ok := h.proxy.storedRewrite("npm", metadataCacheKey, h.proxyURL, metadataCacheKey); ok {
		serveRewrittenMetadata(w, r, stored)
		return
	}

	if versionLookup && h.npmVersionMetadataUsesFilteredPackument(packageName) {
		h.handleFilteredNPMVersionMetadata(w, r, packageName, specifier, accept, registryURL, cacheKey, metadataCacheKey)
		return
	}

	upstream := h.proxy.fetchMetadataDocument(r.Context(), "npm", metadataCacheKey, upstreamURL, accept)
	if err := upstream.err; err != nil {
		if errors.Is(err, ErrUpstreamNotFound) {
			if versionLookup {
				JSONError(w, http.StatusNotFound, ErrVersionNotFound.Error())
			} else {
				JSONError(w, http.StatusNotFound, ErrPackageNotFound.Error())
			}
			return
		}
		if writeUpstreamHeld(w, err) {
			return
		}
		h.proxy.Logger.Error("failed to fetch npm metadata", "error", err)
		JSONError(w, http.StatusBadGateway, "failed to fetch from upstream")
		return
	}

	rewritten, err := h.proxy.cachedRewrite(r.Context(), "npm", h.proxyURL, metadataCacheKey, upstream.body, upstream.digest, func(b []byte) ([]byte, []string, error) {
		return h.rewriteMetadataKeeping(packageName, b)
	})
	if err != nil {
		if r.Context().Err() != nil {
			return // the client left while waiting on a shared rewrite
		}
		if errors.Is(err, ErrVersionDenied) {
			JSONError(w, http.StatusForbidden, err.Error())
			return
		}
		if errors.Is(err, ErrVersionNotFound) {
			JSONError(w, http.StatusNotFound, ErrVersionNotFound.Error())
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
		_, _ = w.Write(upstream.body)
		return
	}

	serveRewrittenMetadata(w, r, rewritten)
}

// packumentAccept returns the Accept header packuments are requested with.
// It prefers the smaller abbreviated packument format but includes
// application/json as a fallback so upstreams that reject the abbreviated type
// (e.g. JFrog Artifactory, which returns 406) can still respond with full
// metadata. When cooldown is enabled we must use full metadata exclusively
// because the abbreviated format omits the "time" map required for version age
// filtering. Operators can also force full metadata so clients that gate on
// publish age (for example Yarn's npmMinimalAgeGate) keep working through the
// proxy.
//
// Every request for a packument uses it, since they share one cache row: a
// request with another Accept would replace the document the others are
// served with one in the other format.
func (h *NPMHandler) packumentAccept() string {
	if h.proxy.NPMFullMetadata || h.proxy.cooldownEnabled() {
		return contentTypeJSON
	}
	return npmAcceptDefault
}

// handleFilteredNPMVersionMetadata serves /pkg/<version> and /pkg/<tag> from the
// filtered packument so cooldown and dist-tags match the root metadata response.
func (h *NPMHandler) handleFilteredNPMVersionMetadata(w http.ResponseWriter, r *http.Request, packageName, specifier, accept, registryURL, cacheKey, metadataCacheKey string) {
	_, packumentPath := npmMetadataPath(packageName)
	packument, _, err := h.proxy.FetchOrCacheMetadata(r.Context(), "npm", cacheKey, registryURL+"/"+packumentPath, accept)
	if err != nil {
		if errors.Is(err, ErrUpstreamNotFound) {
			JSONError(w, http.StatusNotFound, ErrPackageNotFound.Error())
			return
		}
		if writeUpstreamHeld(w, err) {
			return
		}
		h.proxy.Logger.Error("failed to fetch npm metadata", "error", err)
		JSONError(w, http.StatusBadGateway, "failed to fetch from upstream")
		return
	}

	rewritten, err := h.proxy.cachedRewrite(r.Context(), "npm", h.proxyURL, metadataCacheKey, packument, "", func(b []byte) ([]byte, []string, error) {
		out, err := h.rewriteNPMVersionFromPackument(packageName, specifier, b)
		return out, nil, err
	})
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		if errors.Is(err, ErrVersionDenied) {
			JSONError(w, http.StatusForbidden, err.Error())
			return
		}
		if errors.Is(err, ErrVersionNotFound) {
			JSONError(w, http.StatusNotFound, ErrVersionNotFound.Error())
			return
		}
		if len(h.proxy.Denylist.Versions(canonicalPackagePURL("npm", packageName))) != 0 {
			JSONError(w, http.StatusBadGateway, "failed to filter package metadata")
			return
		}
		h.proxy.Logger.Warn("failed to rewrite npm version metadata", "error", err)
		JSONError(w, http.StatusBadGateway, "failed to filter package metadata")
		return
	}

	w.Header().Set(headerContentType, contentTypeJSON)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(rewritten.body)
}

// npmMetadataCacheKey qualifies routed-package metadata cache entries with the
// requested version or dist-tag when the client asks for a single version.
func npmMetadataCacheKey(cacheKey, packagePath, packageName string) string {
	specifier, versionLookup := npmVersionSpecifier(packagePath, packageName)
	if !versionLookup {
		return cacheKey
	}
	return cacheKey + "/" + specifier
}

// npmVersionSpecifier returns the version or dist-tag segment after the package
// name in a metadata path, for example "4.17.21" or "latest".
func npmVersionSpecifier(packagePath, packageName string) (string, bool) {
	if packagePath == packageName {
		return "", false
	}
	specifier, ok := strings.CutPrefix(packagePath, packageName+"/")
	return specifier, ok
}

func (h *NPMHandler) npmVersionMetadataUsesFilteredPackument(packageName string) bool {
	if h.proxy.Cooldown != nil && h.proxy.Cooldown.Enabled() {
		return true
	}
	if h.proxy.Denylist != nil {
		return len(h.proxy.Denylist.Versions(canonicalPackagePURL("npm", packageName))) != 0
	}
	return false
}

// rewriteNPMVersionFromPackument picks one version entry out of an already
// filtered packument and rewrites its tarball URL.
func (h *NPMHandler) rewriteNPMVersionFromPackument(packageName, specifier string, packument []byte) ([]byte, error) {
	filtered, err := h.rewriteMetadata(packageName, packument)
	if err != nil {
		return nil, err
	}

	target := specifier
	if resolved, ok, err := lookupJSONString(filtered, "dist-tags", specifier); err != nil {
		return nil, err
	} else if ok {
		target = resolved
	}

	if h.proxy.versionDenied("npm", packageName, target) {
		return nil, ErrVersionDenied
	}

	versionRaw, err := lookupJSON(filtered, "versions", target)
	if err != nil {
		return nil, err
	}
	if len(versionRaw) == 0 {
		return nil, ErrVersionNotFound
	}

	var out bytes.Buffer
	h.writeNPMVersion(&out, packageName, target, versionRaw)
	return out.Bytes(), nil
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
	out, _, err := h.rewriteMetadataKeeping(packageName, body)
	return out, err
}

// rewriteMetadataKeeping is rewriteMetadata that also reports the versions it
// kept.
func (h *NPMHandler) rewriteMetadataKeeping(packageName string, body []byte) ([]byte, []string, error) {
	return h.rewriteMetadataWithLocal(packageName, body, nil)
}

// rewriteMetadataWithLocal is rewriteMetadataKeeping that also lists the
// pushed versions in local, in place of any upstream entry with the same
// version. Cooldown and the denylist only filter upstream versions, and
// dist-tags stay as upstream has them.
func (h *NPMHandler) rewriteMetadataWithLocal(packageName string, body []byte, local []npmLocalEntry) ([]byte, []string, error) {
	doc, err := indexNPMPackument(body)
	if err != nil {
		return nil, nil, err
	}

	versionsRaw := doc.value(doc.versionsAt)
	if len(versionsRaw) == 0 || versionsRaw[0] != '{' {
		version, ok, err := lookupJSONString(body, "version")
		if err != nil {
			return nil, nil, err
		}
		if ok {
			if h.proxy.versionDenied("npm", packageName, version) {
				return nil, nil, ErrVersionDenied
			}
			var out bytes.Buffer
			if h.writeNPMVersion(&out, packageName, version, body) {
				h.logTarballRewrites(packageName, 1)
			}
			return out.Bytes(), []string{version}, nil
		}
		if len(h.proxy.Denylist.Versions(canonicalPackagePURL("npm", packageName))) != 0 {
			return nil, nil, errors.New("npm metadata has no versions object")
		}
		return body, nil, nil // No versions to rewrite
	}

	// The filters only look at which versions exist, so they run on a map of
	// version names alongside the decoded time map and dist-tags.
	versions := map[string]any{}
	if err := forEachJSONMember(versionsRaw, func(m jsonMember) error {
		version, err := jsonKey(m.key(versionsRaw))
		versions[version] = nil
		return err
	}); err != nil {
		return nil, nil, err
	}
	metadata := map[string]any{}
	timeMap := doc.decodeObject(doc.timeAt, metadata, "time")
	distTags := doc.decodeObject(doc.tagsAt, metadata, "dist-tags")
	timeEntries := len(timeMap)
	originalTags := maps.Clone(distTags)

	h.applyCooldownFiltering(metadata, versions, packageName)
	h.applyDenylistFiltering(metadata, versions, packageName)
	for _, e := range local {
		delete(versions, e.version)
	}

	// Untouched members are copied as they are; the three the filters
	// handled are written from their filtered state, and only when it changed.
	out, err := doc.write(func(out *bytes.Buffer, i int) error {
		switch {
		case i == doc.versionsAt:
			return h.writeNPMVersions(out, packageName, versionsRaw, versions, local)
		case i == doc.timeAt && timeMap != nil && (len(timeMap) != timeEntries || len(local) > 0):
			pushedAt := make([][2]string, 0, len(local))
			for _, e := range local {
				pushedAt = append(pushedAt, [2]string{e.version, e.time})
			}
			return writeFilteredJSONObjectWith(out, doc.value(i), func(key string) bool {
				_, ok := timeMap[key]
				_, pushed := findNPMLocalEntry(local, key)
				return ok && !pushed
			}, pushedAt)
		case i == doc.tagsAt && distTags != nil && !reflect.DeepEqual(distTags, originalTags):
			encoded, err := json.Marshal(distTags)
			out.Write(encoded)
			return err
		default:
			out.Write(doc.value(i))
			return nil
		}
	})
	kept := slices.Collect(maps.Keys(versions))
	for _, e := range local {
		kept = append(kept, e.version)
	}
	return out, kept, err
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
// keep and pointing each kept version's tarball at this proxy, followed by the
// pushed versions in local.
func (h *NPMHandler) writeNPMVersions(out *bytes.Buffer, packageName string, versionsRaw []byte, keep map[string]any, local []npmLocalEntry) error {
	out.WriteByte('{')
	first := true
	rewritten := 0
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
		if h.writeNPMVersion(out, packageName, version, m.value(versionsRaw)) {
			rewritten++
		}
		return nil
	})
	h.logTarballRewrites(packageName, rewritten)
	for _, e := range local {
		if !first {
			out.WriteByte(',')
		}
		first = false
		key, _ := json.Marshal(e.version)
		out.Write(key)
		out.WriteByte(':')
		out.Write(e.entry)
	}
	out.WriteByte('}')
	return err
}

// writeNPMVersion writes one version entry with its dist.tarball pointing at
// this proxy, and reports whether it rewrote the tarball. An entry without a
// string tarball is written unchanged.
func (h *NPMHandler) writeNPMVersion(out *bytes.Buffer, packageName, version string, raw []byte) bool {
	dist, ok, err := findJSONMember(raw, "dist")
	if err != nil || !ok {
		out.Write(raw)
		return false
	}
	distRaw := dist.value(raw)
	tarball, ok, err := findJSONMember(distRaw, "tarball")
	if err != nil || !ok || distRaw[tarball.valStart] != '"' {
		out.Write(raw)
		return false
	}
	var oldTarball string
	if err := json.Unmarshal(tarball.value(distRaw), &oldTarball); err != nil {
		out.Write(raw)
		return false
	}

	newTarball := h.proxyTarballURL(packageName, version, oldTarball)
	encoded, err := json.Marshal(newTarball)
	if err != nil {
		out.Write(raw)
		return false
	}
	out.Write(raw[:dist.valStart+tarball.valStart])
	out.Write(encoded)
	out.Write(raw[dist.valStart+tarball.valEnd:])
	return true
}

// logTarballRewrites logs once per document how many tarball URLs it
// rewrote. A line per version flooded debug logs: packages such as eslint
// list hundreds of versions, and with cooldown on every request rewrites
// the document again.
func (h *NPMHandler) logTarballRewrites(packageName string, count int) {
	if count > 0 {
		h.proxy.Logger.Debug("rewrote tarball URLs", "package", packageName, "count", count)
	}
}

// writeFilteredJSONObjectWith keeps members whose decoded keys match keep,
// then appends extra string members.
func writeFilteredJSONObjectWith(out *bytes.Buffer, obj []byte, keep func(string) bool, extra [][2]string) error {
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
	for _, member := range extra {
		if !first {
			out.WriteByte(',')
		}
		first = false
		key, _ := json.Marshal(member[0])
		value, _ := json.Marshal(member[1])
		out.Write(key)
		out.WriteByte(':')
		out.Write(value)
	}
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
	// Pushed versions are listed without cooldown, so they are served without
	// it too, even when the upstream published the same version recently.
	local, err := h.proxy.GetLocalArtifact(r.Context(), "npm", packageName, version, filename)
	if err != nil {
		h.proxy.serveArtifactError(w, err, "failed to read pushed package")
		return
	}
	if local != nil {
		ServeArtifactRequest(w, r, local)
		return
	}
	if h.proxy.hostedOnly("npm", packageName) {
		JSONError(w, http.StatusNotFound, ErrVersionNotFound.Error())
		return
	}
	registryURL, cacheKey := h.sourceFor(packageName)
	metadata := sync.OnceValues(func() ([]byte, error) {
		upstreamURL := registryURL + "/" + url.PathEscape(packageName)
		body, _, err := h.proxy.FetchOrCacheMetadata(r.Context(), "npm", cacheKey, upstreamURL, h.packumentAccept())
		return body, err
	})
	if h.versionInCooldown(packageName, version, metadata) {
		h.proxy.Logger.Info("cooldown: withholding npm tarball",
			"package", packageName, "version", version)
		JSONError(w, http.StatusNotFound, ErrVersionNotFound.Error())
		return
	}

	result, err := h.getTarball(r, registryURL, packageName, version, filename, metadata)
	if err != nil {
		switch {
		case errors.Is(err, ErrUpstreamNotFound):
			JSONError(w, http.StatusNotFound, ErrPackageNotFound.Error())
		case errors.Is(err, ErrArtifactBlocked), errors.Is(err, ErrVersionDenied):
			JSONError(w, http.StatusForbidden, err.Error())
		default:
			if writeUpstreamHeld(w, err) {
				return
			}
			h.proxy.Logger.Error("failed to get artifact", "error", err)
			JSONError(w, http.StatusBadGateway, "failed to fetch package")
		}
		return
	}

	ServeArtifactRequest(w, r, result)
}

func (h *NPMHandler) getTarball(r *http.Request, registryURL, packageName, version, filename string, metadata func() ([]byte, error)) (*CacheResult, error) {
	cacheFilename := h.routes.cacheFilename(packageName, filename)
	if cached, err := h.proxy.GetCachedArtifact(r.Context(), "npm", packageName, version, cacheFilename); err != nil || cached != nil {
		return cached, err
	}
	downloadURL := fmt.Sprintf("%s/%s/-/%s", registryURL, escapeNPMDownloadPackage(packageName), url.PathEscape(filename))
	var sri string
	body, err := metadata()
	if err == nil {
		if tarball := npmVersionTarball(body, version); tarball != "" {
			downloadURL, err = validateTarballURL(tarball, registryURL)
			if err != nil {
				return nil, err
			}
		}
		sri = npmVersionIntegrity(body, version)
	}
	return h.proxy.GetOrFetchArtifactFromURLWithIntegrity(r.Context(), "npm", packageName, version, cacheFilename, downloadURL, sri)
}

// npmVersionIntegrity returns the integrity a packument declares for a
// version's tarball: dist.integrity when it is a Subresource Integrity string
// the proxy can verify, or else the SHA-1 in dist.shasum, which is all
// versions published before integrity existed carry, as "sha1-<hex>". It
// returns "" when neither is usable, and the tarball is fetched unverified as
// before.
func npmVersionIntegrity(body []byte, version string) string {
	sri, _, _ := lookupJSONString(body, "versions", version, "dist", "integrity")
	if sri != "" {
		if _, err := integrity.ParseSRI(sri); err == nil {
			return sri
		}
	}
	shasum, _, _ := lookupJSONString(body, "versions", version, "dist", "shasum")
	return sha1Checksum(shasum)
}

// npmVersionTarball returns versions[version].dist.tarball from a packument,
// or "" if it has none. It reads that one value in place rather than decoding
// every version, since it runs on each tarball the proxy has not cached yet.
func npmVersionTarball(body []byte, version string) string {
	tarball, _, _ := lookupJSONString(body, "versions", version, "dist", "tarball")
	return tarball
}

func validateTarballURL(raw, registryURL string) (string, error) {
	tarball, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parsing npm tarball URL: %w", err)
	}
	upstream, err := url.Parse(registryURL)
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
//
// The versions row is keyed by the plain version PURL, which does not say
// which registry the version came from, so routed packages leave it alone:
// their publish time always comes from the route's own metadata, cached under
// the route. Otherwise a time read from the default registry would be trusted
// for a private version of the same name, a private time would hold back the
// public version, and moving a route to another upstream would keep the old
// one's times.
func (h *NPMHandler) versionInCooldown(packageName, version string, metadata func() ([]byte, error)) bool {
	if h.proxy.Cooldown == nil || !h.proxy.Cooldown.Enabled() {
		return false
	}

	_, routed := h.routes.match(packageName)
	versionPURL := canonicalVersionPURL("npm", packageName, version)
	if !routed {
		if ver, err := h.proxy.DB.GetVersionByPURL(versionPURL); err == nil && ver != nil && ver.PublishedAt.Valid {
			return !h.proxy.Cooldown.IsAllowed("npm", canonicalPackagePURL("npm", packageName), ver.PublishedAt.Time)
		}
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

	if !routed {
		if err := h.proxy.DB.SetVersionPublishedAt(versionPURL, canonicalPackagePURL("npm", packageName), publishedAt); err != nil {
			h.proxy.Logger.Warn("cooldown: could not store npm publish time",
				"package", packageName, "version", version, "error", err)
		}
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
