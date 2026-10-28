package handler

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/git-pkgs/integrity"
	"github.com/git-pkgs/proxy/internal/jsonscan"
	"github.com/git-pkgs/proxy/internal/metadata/npm"
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

	// npmSearchPath is what `npm search` queries, with the terms in the query
	// string. npmPingPath is what `npm ping` checks.
	npmSearchPath = "/-/v1/search"
	npmPingPath   = "/-/ping"

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

		switch r.URL.Path {
		case npmKeysPath:
			h.proxy.ProxyUpstream(w, r, h.upstreamURL+npmKeysPath,
				[]string{headerAccept, headerAcceptEncoding})
			return
		case npmSearchPath, npmPingPath:
			// Every query has its own answer, so these are relayed uncached
			// and keep their query string.
			upstreamURL := h.upstreamURL + r.URL.Path
			if r.URL.RawQuery != "" {
				upstreamURL += "?" + r.URL.RawQuery
			}
			h.proxy.ProxyUpstream(w, r, upstreamURL,
				[]string{headerAccept, headerAcceptEncoding})
			return
		}

		if strings.HasPrefix(r.URL.Path, npmPackagePrefix) && strings.HasSuffix(r.URL.Path, npmDistTagsSuffix) {
			h.handleDistTags(w, r)
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

	h.proxy.Logger.Debug("npm metadata request", "package", packageName, "path", packagePath)

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

	var validate func([]byte) error
	if !versionLookup {
		validate = h.packumentValidator()
	}
	upstream := h.proxy.fetchMetadataDocument(r.Context(), "npm", metadataCacheKey, upstreamURL, accept, validate)
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
		if h.proxy.mustFilterMetadata("npm", packageName) {
			h.proxy.Logger.Error("failed to filter npm metadata", "package", packageName, "error", err)
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

// errAbbreviatedPackument rejects an abbreviated packument where the full one
// is needed.
var errAbbreviatedPackument = errors.New("abbreviated packument where full metadata is required")

// packumentValidator returns the check a packument must pass to be served or
// cached, or nil when any document will do. Packuments share one cache row
// whatever format they were fetched in, so one cached in abbreviated form
// before cooldown or full metadata was turned on would otherwise still be
// served, and used for the cooldown tarball check, while it stayed fresh or
// as a stale copy when upstream failed. It has no "time" map, so cooldown
// would let every version through. Rejecting it makes the lookup fetch the
// full document instead.
func (h *NPMHandler) packumentValidator() func([]byte) error {
	if h.packumentAccept() != contentTypeJSON {
		return nil
	}
	return requireFullPackument
}

// requireFullPackument rejects an abbreviated packument (see
// npm.Abbreviated). A full document without a time map, as some registries
// serve, is let through as before, and so is anything that is not a JSON
// object, leaving it to the usual handling.
func requireFullPackument(body []byte) error {
	if npm.Abbreviated(body) {
		return errAbbreviatedPackument
	}
	return nil
}

// handleFilteredNPMVersionMetadata serves /pkg/<version> and /pkg/<tag> from the
// filtered packument so cooldown and dist-tags match the root metadata response.
func (h *NPMHandler) handleFilteredNPMVersionMetadata(w http.ResponseWriter, r *http.Request, packageName, specifier, accept, registryURL, cacheKey, metadataCacheKey string) {
	_, packumentPath := npmMetadataPath(packageName)
	packument, _, _, err := h.proxy.fetchOrCacheMetadata(r.Context(), "npm", cacheKey, registryURL+"/"+packumentPath, "", h.packumentValidator(), accept)
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
		if h.proxy.mustFilterMetadata("npm", packageName) {
			h.proxy.Logger.Error("failed to filter npm metadata", "package", packageName, "error", err)
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
	out, err := npm.Version(packageName, specifier, packument, h.npmRewriteOptions(packageName, nil))
	return out, npmRewriteError(err)
}

// rewriteMetadataKeeping rewrites tarball URLs in npm package metadata to
// point at this proxy, filters the versions cooldown and the denylist
// withhold, and reports the versions it kept. See npm.Rewrite.
func (h *NPMHandler) rewriteMetadataKeeping(packageName string, body []byte) ([]byte, []string, error) {
	return h.rewriteMetadataWithLocal(packageName, body, nil)
}

// rewriteMetadataWithLocal is rewriteMetadataKeeping that also lists the
// pushed versions in local, in place of any upstream entry with the same
// version. Cooldown and the denylist only filter upstream versions, and
// dist-tags stay as upstream has them.
func (h *NPMHandler) rewriteMetadataWithLocal(packageName string, body []byte, local []npmLocalEntry) ([]byte, []string, error) {
	out, kept, err := npm.Rewrite(packageName, body, h.npmRewriteOptions(packageName, local))
	return out, kept, npmRewriteError(err)
}

// npmRewriteOptions gathers what a rewrite of packageName's metadata depends
// on from the proxy: its URL, the cooldown and denylist policy, the time
// cooldown is evaluated at, and the pushed versions to list. The callbacks
// log what the rewrite did.
func (h *NPMHandler) npmRewriteOptions(packageName string, local []npmLocalEntry) npm.Options {
	return npm.Options{
		ProxyURL:       h.proxyURL,
		Cooldown:       h.proxy.Cooldown,
		DeniedVersions: h.proxy.Denylist.Versions(canonicalPackagePURL("npm", packageName)),
		EvaluatedAt:    time.Now(),
		LocalVersions:  local,
		// Logged once per document: a line per version flooded debug logs,
		// since packages such as eslint list hundreds of versions and with
		// cooldown on every request rewrites the document again.
		OnTarballsRewritten: func(count int) {
			h.proxy.Logger.Debug("rewrote tarball URLs", "package", packageName, "count", count)
		},
		OnCooldownFilter: func(version, publishedAt string) {
			h.proxy.Logger.Info("cooldown: filtering npm version", "package", packageName, "version", version,
				"published", publishedAt)
		},
	}
}

// npmRewriteError maps the rewrite's errors to the handler's, which the
// routes answer with 403 and 404.
func npmRewriteError(err error) error {
	switch {
	case errors.Is(err, npm.ErrVersionDenied):
		return ErrVersionDenied
	case errors.Is(err, npm.ErrVersionNotFound):
		return ErrVersionNotFound
	default:
		return err
	}
}

func (h *NPMHandler) proxyTarballURL(packageName, version, tarball string) string {
	return npm.TarballURL(h.proxyURL, packageName, version, tarball)
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

	h.proxy.Logger.Debug("npm download request",
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
		body, _, _, err := h.proxy.fetchOrCacheMetadata(r.Context(), "npm", cacheKey, upstreamURL, "", h.packumentValidator(), h.packumentAccept())
		return body, err
	})
	inCooldown, err := h.versionInCooldown(packageName, version, metadata)
	if err != nil {
		if h.proxy.serveCachedDespiteCooldownError(w, r, "npm", packageName, version, h.routes.cacheFilename(packageName, filename), err) {
			return
		}
		if writeUpstreamHeld(w, err) {
			return
		}
		h.proxy.Logger.Warn("cooldown: could not read npm publish time",
			"package", packageName, "version", version, "error", err)
		JSONError(w, http.StatusBadGateway, "failed to check cooldown")
		return
	}
	if inCooldown {
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
	// The packument says where the tarball is and what it must hash to.
	// Without it the tarball could not be verified, and an unverified copy
	// would be cached and served to everyone, so the download fails instead.
	body, err := metadata()
	if err != nil {
		return nil, fmt.Errorf("fetching npm metadata to verify tarball: %w", err)
	}
	downloadURL := fmt.Sprintf("%s/%s/-/%s", registryURL, escapeNPMDownloadPackage(packageName), url.PathEscape(filename))
	if tarball := npmVersionTarball(body, version); tarball != "" {
		downloadURL, err = validateTarballURL(tarball, registryURL)
		if err != nil {
			return nil, err
		}
	}
	sri := npmVersionIntegrity(body, version)
	return h.proxy.GetOrFetchArtifactFromURLWithIntegrity(r.Context(), "npm", packageName, version, cacheFilename, downloadURL, sri)
}

// npmVersionIntegrity returns the integrity a packument declares for a
// version's tarball: dist.integrity when it is a Subresource Integrity string
// the proxy can verify, or else the SHA-1 in dist.shasum, which is all
// versions published before integrity existed carry, as "sha1-<hex>". It
// returns "" when neither is usable, and the tarball is fetched unverified as
// before.
func npmVersionIntegrity(body []byte, version string) string {
	sri, _, _ := jsonscan.LookupString(body, "versions", version, "dist", "integrity")
	if sri != "" {
		if _, err := integrity.ParseSRI(sri); err == nil {
			return sri
		}
	}
	shasum, _, _ := jsonscan.LookupString(body, "versions", version, "dist", "shasum")
	return sha1Checksum(shasum)
}

// npmVersionTarball returns versions[version].dist.tarball from a packument,
// or "" if it has none. It reads that one value in place rather than decoding
// every version, since it runs on each tarball the proxy has not cached yet.
func npmVersionTarball(body []byte, version string) string {
	tarball, _, _ := jsonscan.LookupString(body, "versions", version, "dist", "tarball")
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
// treats it. A packument that can't be fetched or parsed is an error, so a
// failing upstream can't let a version through.
//
// The versions row is keyed by the plain version PURL, which does not say
// which registry the version came from, so routed packages leave it alone:
// their publish time always comes from the route's own metadata, cached under
// the route. Otherwise a time read from the default registry would be trusted
// for a private version of the same name, a private time would hold back the
// public version, and moving a route to another upstream would keep the old
// one's times.
func (h *NPMHandler) versionInCooldown(packageName, version string, metadata func() ([]byte, error)) (bool, error) {
	if !h.proxy.cooldownApplies("npm", packageName) {
		return false, nil
	}

	_, routed := h.routes.match(packageName)
	versionPURL := canonicalVersionPURL("npm", packageName, version)
	if !routed {
		if ver, err := h.proxy.DB.GetVersionByPURL(versionPURL); err == nil && ver != nil && ver.PublishedAt.Valid {
			return !h.proxy.Cooldown.IsAllowed("npm", canonicalPackagePURL("npm", packageName), ver.PublishedAt.Time), nil
		}
	}

	body, err := metadata()
	if errors.Is(err, ErrUpstreamNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	published, ok, err := jsonscan.LookupString(body, "time", version)
	if err != nil {
		return false, fmt.Errorf("parsing npm metadata: %w", err)
	}
	if !ok {
		return false, nil
	}

	publishedAt, err := time.Parse(time.RFC3339, published)
	if err != nil {
		return false, nil
	}

	if !routed {
		if err := h.proxy.DB.SetVersionPublishedAt(versionPURL, canonicalPackagePURL("npm", packageName), publishedAt); err != nil {
			h.proxy.Logger.Warn("cooldown: could not store npm publish time",
				"package", packageName, "version", version, "error", err)
		}
	}

	return !h.proxy.Cooldown.IsAllowed("npm", canonicalPackagePURL("npm", packageName), publishedAt), nil
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

func (h *NPMHandler) extractVersionFromFilename(packageName, filename string) string {
	return npm.VersionFromFilename(packageName, filename)
}
