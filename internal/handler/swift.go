package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/metadata/swift"
	"github.com/git-pkgs/proxy/internal/packageurl"
	"github.com/git-pkgs/proxy/internal/retention"
	"github.com/git-pkgs/purl"
)

const (
	swiftAcceptJSON     = "application/vnd.swift.registry.v1+json"
	swiftAcceptManifest = "application/vnd.swift.registry.v1+swift"
	swiftAcceptArchive  = "application/vnd.swift.registry.v1+zip"
	swiftContentVersion = "1"
)

// Source archives are stored under the registry identity,
// pkg:generic/swift-registry/{scope}.{name}, lowercased. Package rules write
// the scope as the namespace, pkg:swift/apple/swift-nio, or use the stored
// form. A source repository such as pkg:swift/github.com/apple/swift-nio
// names no registry identity; it is resolved through the upstream's
// /identifiers before it applies.
func init() {
	retention.Default.Register(retention.Spec{
		Key:              "swift",
		CanonicalPackage: swiftRetentionKey,
		SourcePackage:    func(p *purl.PURL) bool { return SwiftSourcePURL(p) != "" },
	})
}

// SwiftSourceURL returns the repository URL of a pkg:swift name whose
// namespace starts with a host, as in github.com/apple/swift-nio, or "" for
// a name whose namespace is a registry scope.
func SwiftSourceURL(name string) string {
	if host, _, ok := strings.Cut(name, "/"); ok && strings.Contains(host, ".") {
		return "https://" + name
	}
	return ""
}

// SwiftSourcePURL returns the repository URL of a pkg:swift PURL that names
// a package by its source repository, as SBOM tools and GHSA do, or "".
func SwiftSourcePURL(p *purl.PURL) string {
	if p == nil || p.Type != "swift" || p.Namespace == "" {
		return ""
	}
	return SwiftSourceURL(p.Namespace + "/" + p.Name)
}

// ResolveSourcePURL returns p with a source repository namespace replaced by
// each registry scope and name the upstream lists for it, and p itself when
// it already names a registry identity. A policy for the repository applies
// to all of them, since a client may use any.
func (h *SwiftHandler) ResolveSourcePURL(ctx context.Context, p *purl.PURL) ([]*purl.PURL, error) {
	source := SwiftSourcePURL(p)
	if source == "" {
		return []*purl.PURL{p}, nil
	}
	ids, err := h.ResolveIdentities(ctx, source)
	if err != nil {
		return nil, err
	}
	resolved := make([]*purl.PURL, 0, len(ids))
	for _, id := range ids {
		resolved = append(resolved, purl.New("swift", id.Scope, id.Name, p.Version, nil))
	}
	return resolved, nil
}

// swiftRetentionKey maps a package rule to the stored package PURL, refusing
// keys that name no valid registry scope and package. MakeCacheStrings
// lowercases the identity the way the handler stores it.
func swiftRetentionKey(p *purl.PURL) (string, bool) {
	if p == nil {
		return "", false
	}
	var scope, name string
	switch {
	case p.Type == "swift":
		scope, name = p.Namespace, p.Name
	case p.Type == "generic" && strings.EqualFold(p.Namespace, "swift-registry"):
		var found bool
		if scope, name, found = strings.Cut(p.Name, "."); !found {
			return "", false
		}
	default:
		return "", false
	}
	if !validSwiftScope(scope) || !validSwiftPackageName(name) {
		return "", false
	}
	packagePURL, _ := packageurl.MakeCacheStrings("swift", scope+"/"+name, "")
	return packagePURL, packagePURL != ""
}

// SwiftHandler handles the read-only Swift Package Registry v1 protocol.
type SwiftHandler struct {
	proxy       *Proxy
	upstreamURL string
	proxyURL    string
}

// NewSwiftHandler creates a Swift Package Registry protocol handler.
func NewSwiftHandler(proxy *Proxy, proxyURL, upstreamURL string) *SwiftHandler {
	if strings.TrimSpace(upstreamURL) == "" {
		upstreamURL = config.DefaultSwiftUpstream
	}

	return &SwiftHandler{
		proxy:       proxy,
		upstreamURL: strings.TrimSuffix(upstreamURL, "/"),
		proxyURL:    strings.TrimSuffix(proxyURL, "/"),
	}
}

// Routes returns the HTTP handler for Swift registry requests.
func (h *SwiftHandler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /identifiers", h.handleIdentifiers)
	mux.HandleFunc("GET /{scope}/{name}/{version}/Package.swift", h.handleManifest)
	mux.HandleFunc("GET /{scope}/{name}/{version}", h.handleRelease)
	mux.HandleFunc("PUT /{scope}/{name}/{version}", h.handlePublishingUnsupported)
	mux.HandleFunc("GET /{scope}/{name}", h.handlePackageReleases)
	return mux
}

func (h *SwiftHandler) handlePackageReleases(w http.ResponseWriter, r *http.Request) {
	scope := r.PathValue("scope")
	name := strings.TrimSuffix(r.PathValue("name"), ".json")
	if !validSwiftScope(scope) || !validSwiftPackageName(name) {
		writeSwiftProblem(w, http.StatusBadRequest, "invalid package identifier")
		return
	}
	scope, name = canonicalSwiftPackage(scope, name)

	upstreamURL := h.buildUpstreamURL(scope, name, "", "", r.URL.RawQuery)
	accept := requestAccept(r, swiftAcceptJSON)
	doc, err := h.fetchDocument(
		r.Context(), swiftReleasesCacheKey(scope, name, r.URL.RawQuery, accept), upstreamURL, accept,
	)
	if err != nil {
		h.writeMetadataError(w, err)
		return
	}

	rewritten, err := h.rewriteReleaseURLs(scope, name, doc.body)
	if err != nil {
		h.proxy.Logger.Warn("failed to rewrite Swift release URLs", "error", err)
		rewritten = doc.body
	}
	if doc.link != "" {
		w.Header().Set("Link", h.rewriteLinkHeader(doc.link, upstreamURL))
	}
	writeSwiftMetadata(w, r, rewritten, doc.contentType)
}

func (h *SwiftHandler) handleRelease(w http.ResponseWriter, r *http.Request) {
	scope := r.PathValue("scope")
	name := r.PathValue("name")
	version := r.PathValue("version")
	if strings.HasSuffix(version, ".zip") {
		h.handleSourceArchive(w, r, scope, name, strings.TrimSuffix(version, ".zip"))
		return
	}

	version = strings.TrimSuffix(version, ".json")
	if !validSwiftPackageReference(scope, name, version) {
		writeSwiftProblem(w, http.StatusBadRequest, "invalid package release")
		return
	}
	scope, name = canonicalSwiftPackage(scope, name)

	upstreamURL := h.buildUpstreamURL(scope, name, version, "", r.URL.RawQuery)
	accept := requestAccept(r, swiftAcceptJSON)
	body, contentType, err := h.proxy.FetchOrCacheMetadata(
		r.Context(), "swift", swiftReleaseVariantCacheKey(scope, name, version, r.URL.RawQuery, accept), upstreamURL, accept,
	)
	if err != nil {
		h.writeMetadataError(w, err)
		return
	}
	writeSwiftMetadata(w, r, body, contentType)
}

func (h *SwiftHandler) handleManifest(w http.ResponseWriter, r *http.Request) {
	scope := r.PathValue("scope")
	name := r.PathValue("name")
	version := r.PathValue("version")
	if !validSwiftPackageReference(scope, name, version) {
		writeSwiftProblem(w, http.StatusBadRequest, "invalid package release")
		return
	}
	scope, name = canonicalSwiftPackage(scope, name)

	upstreamURL := h.buildUpstreamURL(scope, name, version, "Package.swift", r.URL.RawQuery)
	accept := requestAccept(r, swiftAcceptManifest)
	doc, err := h.fetchDocument(
		r.Context(), swiftManifestCacheKey(scope, name, version, r.URL.RawQuery, accept), upstreamURL, accept,
	)
	if err != nil {
		h.writeMetadataError(w, err)
		return
	}
	if doc.link != "" {
		w.Header().Set("Link", h.rewriteLinkHeader(doc.link, upstreamURL))
	}
	writeSwiftMetadata(w, r, doc.body, doc.contentType)
}

func (h *SwiftHandler) handleIdentifiers(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("url") == "" {
		writeSwiftProblem(w, http.StatusBadRequest, "url query parameter is required")
		return
	}

	upstreamURL := h.upstreamURL + "/identifiers?" + r.URL.RawQuery
	accept := requestAccept(r, swiftAcceptJSON)
	cacheKey := swiftVariantCacheKey(accept, swiftAcceptJSON, "identifiers", r.URL.RawQuery)
	body, contentType, err := h.proxy.FetchOrCacheMetadata(
		r.Context(), "swift", cacheKey, upstreamURL, accept,
	)
	if err != nil {
		h.writeMetadataError(w, err)
		return
	}
	writeSwiftMetadata(w, r, body, contentType)
}

func (h *SwiftHandler) handlePublishingUnsupported(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Allow", "GET, HEAD")
	writeSwiftProblem(w, http.StatusMethodNotAllowed, "publishing isn't supported")
}

func (h *SwiftHandler) handleSourceArchive(w http.ResponseWriter, r *http.Request, scope, name, version string) {
	if !validSwiftPackageReference(scope, name, version) {
		writeSwiftProblem(w, http.StatusBadRequest, "invalid package release")
		return
	}
	scope, name = canonicalSwiftPackage(scope, name)

	key, err := swiftSourceArchiveKey(scope, name, version)
	if err != nil {
		h.writeArtifactError(w, err)
		return
	}
	filename, packagePURL, versionPURL := key.filename, key.packagePURL, key.versionPURL
	upstreamURL := h.buildUpstreamURL(scope, name, version+".zip", "", r.URL.RawQuery)
	archiveInfo, infoErr := h.fetchArchiveInfo(r.Context(), scope, name, version)
	if infoErr != nil {
		h.writeArtifactError(w, fmt.Errorf("fetching release metadata: %w", infoErr))
		return
	}

	if r.Method == http.MethodHead {
		h.handleSourceArchiveHead(w, r, name, version, filename, packagePURL, versionPURL, upstreamURL, archiveInfo)
		return
	}

	headers := make(http.Header)
	headers.Set("Accept", requestAccept(r, swiftAcceptArchive))
	result, err := h.fetchSourceArchive(r.Context(), key, version, upstreamURL, headers, archiveInfo)
	if err != nil {
		h.writeArtifactError(w, err)
		return
	}

	result.Artifact.MediaType = "application/zip"
	setSwiftArchiveHeaders(w.Header(), name, version, result.Artifact.Digest.Encoded(), archiveInfo)
	ServeArtifactRequest(w, r, result)
}

// swiftArchiveKey is the cache key a release's source archive is stored
// under.
type swiftArchiveKey struct {
	packageName, filename, packagePURL, versionPURL string
}

// swiftSourceArchiveKey returns the cache key of the source archive of the
// canonical (lowercased) scope and name at version.
func swiftSourceArchiveKey(scope, name, version string) (swiftArchiveKey, error) {
	key := swiftArchiveKey{packageName: scope + "/" + name, filename: fmt.Sprintf("%s-%s.zip", name, version)}
	key.packagePURL, key.versionPURL = packageurl.MakeCacheStrings("swift", key.packageName, version)
	if key.packagePURL == "" || key.versionPURL == "" {
		return swiftArchiveKey{}, fmt.Errorf("%w: swift %q", errUnsupportedPackageIdentity, key.packageName)
	}
	return key, nil
}

// fetchSourceArchive returns the source archive under key from the cache, or
// fetches it from upstreamURL, verified against the checksum in info.
func (h *SwiftHandler) fetchSourceArchive(ctx context.Context, key swiftArchiveKey, version, upstreamURL string, headers http.Header, info swiftArchiveInfo) (*CacheResult, error) {
	return h.proxy.getOrFetchArtifactFromURLWithCachePURLs(
		ctx, "swift", key.packageName, version, key.filename, key.packagePURL, key.versionPURL,
		upstreamURL, headers, info.checksum, "",
	)
}

func (h *SwiftHandler) handleSourceArchiveHead(
	w http.ResponseWriter,
	r *http.Request,
	name, version, filename, packagePURL, versionPURL, upstreamURL string,
	archiveInfo swiftArchiveInfo,
) {
	result, err := h.proxy.getCachedArtifactWithUpstreamHash(
		r.Context(), packagePURL, versionPURL, filename, archiveInfo.checksum,
	)
	if err != nil {
		h.writeArtifactError(w, err)
		return
	}
	if result != nil {
		result.Artifact.MediaType = "application/zip"
		setSwiftArchiveHeaders(w.Header(), name, version, result.Artifact.Digest.Encoded(), archiveInfo)
		ServeArtifactRequest(w, r, result)
		return
	}

	size, err := h.probeSourceArchive(r.Context(), upstreamURL, requestAccept(r, swiftAcceptArchive))
	if err != nil {
		h.writeArtifactError(w, err)
		return
	}
	setSwiftArchiveHeaders(w.Header(), name, version, "", archiveInfo)
	w.Header().Set(headerContentType, "application/zip")
	if size >= 0 {
		w.Header().Set(headerContentLength, strconv.FormatInt(size, 10))
	}
	w.WriteHeader(http.StatusOK)
}

func (h *SwiftHandler) probeSourceArchive(ctx context.Context, upstreamURL, accept string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamURL, nil)
	if err != nil {
		return 0, fmt.Errorf("creating upstream archive request: %w", err)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("Range", "bytes=0-0")

	resp, err := h.proxy.HTTPClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("requesting upstream archive: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return 0, ErrUpstreamNotFound
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("upstream archive returned %d", resp.StatusCode)
	}

	if resp.StatusCode == http.StatusPartialContent {
		_, total, found := strings.Cut(resp.Header.Get("Content-Range"), "/")
		if !found || total == "*" {
			return -1, nil
		}
		if parsed, parseErr := strconv.ParseInt(total, 10, 64); parseErr == nil {
			return parsed, nil
		}
		return -1, nil
	}

	size := int64(-1)
	if contentLength := resp.Header.Get(headerContentLength); contentLength != "" {
		if parsed, parseErr := strconv.ParseInt(contentLength, 10, 64); parseErr == nil {
			size = parsed
		}
	}
	return size, nil
}

func (h *SwiftHandler) fetchMetadataWithHeaders(
	ctx context.Context,
	upstreamURL, accept string,
) ([]byte, string, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamURL, nil)
	if err != nil {
		return nil, "", nil, fmt.Errorf("creating upstream metadata request: %w", err)
	}
	req.Header.Set("Accept", accept)

	resp, err := h.proxy.HTTPClient.Do(req)
	if err != nil {
		return nil, "", nil, fmt.Errorf("requesting upstream metadata: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, "", nil, ErrUpstreamNotFound
	}
	if err := upstreamMetadataStatus(resp); err != nil {
		return nil, "", nil, err
	}

	body, err := h.proxy.ReadMetadata(resp.Body)
	if err != nil {
		return nil, "", nil, fmt.Errorf("reading upstream metadata: %w", err)
	}
	contentType := resp.Header.Get(headerContentType)
	if contentType == "" {
		contentType = contentTypeJSON
	}
	return body, contentType, resp.Header.Clone(), nil
}

type swiftArchiveInfo struct {
	checksum        string
	signature       string
	signatureFormat string
}

func (h *SwiftHandler) fetchArchiveInfo(ctx context.Context, scope, name, version string) (swiftArchiveInfo, error) {
	upstreamURL := h.buildUpstreamURL(scope, name, version, "", "")
	body, _, err := h.proxy.FetchOrCacheMetadata(
		ctx, "swift", swiftReleaseCacheKey(scope, name, version), upstreamURL, swiftAcceptJSON,
	)
	if err != nil {
		return swiftArchiveInfo{}, err
	}

	archive, err := swift.SourceArchive(body)
	if err != nil {
		return swiftArchiveInfo{}, err
	}
	return swiftArchiveInfo{
		checksum:        archive.Checksum,
		signature:       archive.Signature,
		signatureFormat: archive.SignatureFormat,
	}, nil
}

func setSwiftArchiveHeaders(header http.Header, name, version, contentHash string, info swiftArchiveInfo) {
	header.Set("Cache-Control", "public, immutable")
	header.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.zip"`, name, version))
	header.Set("Content-Version", swiftContentVersion)

	checksum := info.checksum
	if checksum == "" {
		checksum = contentHash
	}
	if digest := swiftDigestHeader(checksum); digest != "" {
		header.Set("Digest", digest)
	}
	if info.signature != "" && info.signatureFormat != "" {
		header.Set("X-Swift-Package-Signature", info.signature)
		header.Set("X-Swift-Package-Signature-Format", info.signatureFormat)
	}
}

func swiftDigestHeader(checksum string) string {
	return swift.DigestHeader(checksum)
}

// rewriteLinkHeader points the upstream URLs in a Link header at the proxy
// (see swift.RewriteLinkHeader).
func (h *SwiftHandler) rewriteLinkHeader(value, upstreamRequestURL string) string {
	return swift.RewriteLinkHeader(value, upstreamRequestURL, h.rewriteOptions())
}

// rewriteReleaseURLs points a release list's release URLs at the proxy (see
// swift.RewriteReleaseURLs).
func (h *SwiftHandler) rewriteReleaseURLs(scope, name string, body []byte) ([]byte, error) {
	return swift.RewriteReleaseURLs(scope, name, body, h.rewriteOptions())
}

func (h *SwiftHandler) rewriteOptions() swift.Options {
	return swift.Options{ProxyURL: h.proxyURL, UpstreamURL: h.upstreamURL}
}

func (h *SwiftHandler) buildUpstreamURL(scope, name, version, resource, rawQuery string) string {
	parts := []string{h.upstreamURL, url.PathEscape(scope), url.PathEscape(name)}
	if version != "" {
		parts = append(parts, url.PathEscape(version))
	}
	if resource != "" {
		parts = append(parts, resource)
	}
	result := strings.Join(parts, "/")
	if rawQuery != "" {
		result += "?" + rawQuery
	}
	return result
}

func swiftMetadataCacheKey(parts ...string) string {
	joined := strings.Join(parts, "\x00")
	digest := sha256.Sum256([]byte(joined))
	return hex.EncodeToString(digest[:])
}

// swiftVariantCacheKey is swiftMetadataCacheKey for a document fetched with
// the Accept header its client sent: an upstream may answer another media
// type with another document. The default one adds nothing to the key.
func swiftVariantCacheKey(accept, fallback string, parts ...string) string {
	if accept != fallback {
		parts = append(parts, "accept", accept)
	}
	return swiftMetadataCacheKey(parts...)
}

// swiftReleaseCacheKey keys the release metadata the proxy itself reads, for
// a source archive's checksum and signature.
func swiftReleaseCacheKey(scope, name, version string) string {
	return swiftMetadataCacheKey("release", scope, name, version)
}

// swiftReleaseVariantCacheKey keys release metadata as a client asked for
// it, which is swiftReleaseCacheKey for a request with no query and the
// default Accept header.
func swiftReleaseVariantCacheKey(scope, name, version, rawQuery, accept string) string {
	parts := []string{"release", scope, name, version}
	if rawQuery != "" {
		parts = append(parts, "query", rawQuery)
	}
	return swiftVariantCacheKey(accept, swiftAcceptJSON, parts...)
}

func requestAccept(r *http.Request, fallback string) string {
	if accept := r.Header.Get("Accept"); accept != "" {
		return accept
	}
	return fallback
}

func writeSwiftMetadata(w http.ResponseWriter, r *http.Request, body []byte, contentType string) {
	if contentType == "" {
		contentType = "application/json"
	}
	digest := sha256.Sum256(body)
	etag := fmt.Sprintf(`"%x"`, digest)
	w.Header().Set(headerContentType, contentType)
	w.Header().Set("Content-Version", swiftContentVersion)
	w.Header().Set(headerETag, etag)
	if ifNoneMatchHits(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set(headerContentLength, strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

func (h *SwiftHandler) writeMetadataError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrUpstreamNotFound) {
		writeSwiftProblem(w, http.StatusNotFound, "not found")
		return
	}
	if h.proxy.writeUpstreamRefusal(w, err) {
		return
	}
	h.proxy.Logger.Error("Swift metadata request failed", "error", err)
	writeSwiftProblem(w, http.StatusBadGateway, "upstream request failed")
}

func (h *SwiftHandler) writeArtifactError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrUpstreamNotFound) {
		writeSwiftProblem(w, http.StatusNotFound, "release not found")
		return
	}
	if errors.Is(err, ErrArtifactBlocked) || errors.Is(err, ErrVersionDenied) {
		writeSwiftProblem(w, http.StatusForbidden, err.Error())
		return
	}
	// The release metadata an uncached archive is looked up in can be
	// refused too.
	if h.proxy.writeUpstreamRefusal(w, err) {
		return
	}
	h.proxy.Logger.Error("Swift archive request failed", "error", err)
	writeSwiftProblem(w, http.StatusBadGateway, "failed to fetch package")
}

func writeSwiftProblem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set(headerContentType, "application/problem+json")
	w.Header().Set("Content-Version", swiftContentVersion)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"detail": detail})
}

func validSwiftPackageReference(scope, name, version string) bool {
	return swift.ValidPackageReference(scope, name, version)
}

func canonicalSwiftPackage(scope, name string) (string, string) {
	return swift.CanonicalPackage(scope, name)
}

func validSwiftScope(scope string) bool {
	return swift.ValidScope(scope)
}

func validSwiftPackageName(name string) bool {
	return swift.ValidPackageName(name)
}
