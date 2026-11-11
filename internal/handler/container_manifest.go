package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"mime"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	containerManifestCacheEcosystem = "oci-manifest"
	containerStaleWarning           = `110 - "Response is Stale"`

	containerAcceptWildcardSpecificity = iota
	containerAcceptTypeWildcardSpecificity
	containerAcceptExactSpecificity
)

const (
	// containerManifestIndexCacheClass replaces the Accept header in tag cache
	// keys for clients that accept an index type.
	containerManifestIndexCacheClass = "index"
	ociImageIndexMediaType           = "application/vnd.oci.image.index.v1+json"
	dockerManifestListMediaType      = "application/vnd.docker.distribution.manifest.list.v2+json"
)

var manifestDigestReferencePattern = regexp.MustCompile(`^[a-z0-9]+:[a-f0-9]+$`)

// referrersTagPattern matches the sha256-<hex> tags of the OCI referrers tag
// schema. Docker looks one up on every pull and fails the pull on any answer
// other than a manifest or 404.
var referrersTagPattern = regexp.MustCompile(`^sha256-[a-f0-9]{64}$`)

type cachedContainerManifest struct {
	body          []byte
	contentType   string
	contentDigest string
	etag          string
	size          int64
	lastModified  time.Time
	fetchedAt     time.Time
}

// serveManifest serves a manifest unless the denylist names the digest it
// resolves to under cacheName, the name blobs are stored under.
func (h *ContainerHandler) serveManifest(w http.ResponseWriter, r *http.Request, registryURL, name, cacheName, reference string) {
	accept := containerManifestAccept(r)
	req := manifestRequest{
		registryURL: registryURL,
		name:        name,
		reference:   reference,
		accept:      accept,
		cacheAccept: normalizeContainerManifestAccept(accept),
		method:      r.Method,
	}
	cached, _ := h.loadContainerManifestForAccept(r.Context(), registryURL, name, reference, req.accept, req.cacheAccept)
	if cached != nil && (req.immutable() || h.containerManifestFresh(cached)) {
		h.writeManifest(w, r, cacheName, reference, cached, false)
		return
	}

	outcome := h.coalescedManifest(r.Context(), req)
	switch {
	case outcome.manifest != nil:
		h.writeManifest(w, r, cacheName, reference, outcome.manifest, false)
	case outcome.relay != nil:
		outcome.relay.write(h.proxy, w, r)
	case outcome.digestInvalid:
		h.containerError(w, http.StatusBadGateway, "DIGEST_INVALID", "manifest digest verification failed")
	default:
		h.serveStaleManifestOrError(w, r, cacheName, reference, cached, outcome.err)
	}
}

func (h *ContainerHandler) serveStaleManifestOrError(w http.ResponseWriter, r *http.Request, cacheName, reference string, cached *cachedContainerManifest, err error) {
	if cached != nil {
		h.proxy.Logger.Warn("upstream manifest fetch failed, serving stale cache", "error", err)
		h.writeManifest(w, r, cacheName, reference, cached, true)
		return
	}
	if referrersTagPattern.MatchString(reference) {
		h.proxy.Logger.Warn("upstream manifest fetch failed, answering referrers tag as unknown", "reference", reference, "error", err)
		h.containerError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
		return
	}
	if writeUpstreamHeld(w, err) {
		return
	}
	h.proxy.Logger.Error("failed to fetch manifest", "error", err)
	h.containerError(w, http.StatusBadGateway, "INTERNAL_ERROR", "failed to fetch from upstream")
}

// manifestDenied writes a 403 and reports true when the denylist names
// reference, a tag or digest, under the repository's cache name.
func (h *ContainerHandler) manifestDenied(w http.ResponseWriter, cacheName, reference string) bool {
	if reference == "" || !h.proxy.versionDenied("oci", cacheName, reference) {
		return false
	}
	h.containerError(w, http.StatusForbidden, "DENIED",
		ErrVersionDenied.Error()+": "+canonicalVersionPURL("oci", cacheName, reference))
	return true
}

// writeManifest writes manifest unless the denylist names its digest.
func (h *ContainerHandler) writeManifest(w http.ResponseWriter, r *http.Request, cacheName, reference string, manifest *cachedContainerManifest, stale bool) {
	if h.manifestDenied(w, cacheName, manifest.contentDigest) {
		return
	}
	h.recordImageTag(cacheName, reference, manifest, stale)
	writeContainerManifest(w, r, manifest, stale)
}

func (h *ContainerHandler) containerManifestFresh(manifest *cachedContainerManifest) bool {
	return h.proxy.MetadataTTL > 0 && !manifest.fetchedAt.IsZero() && time.Since(manifest.fetchedAt) < h.proxy.MetadataTTL
}

func (h *ContainerHandler) containerManifestCacheKey(registryURL, name, reference, accept string) string {
	identity := strings.Join([]string{registryURL, name, reference, accept}, "\x00")
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])
}

// containerManifestSharedCacheKey is the key clients with different Accept
// headers share. A digest names exactly one document. A tag is stored once for
// all clients that accept an index type, whatever upstream returned for it, so
// Docker, containerd and Podman read the same entry. Clients without index
// support keep a key per normalized Accept header, because registries may
// convert an index into a single platform manifest for them.
func (h *ContainerHandler) containerManifestSharedCacheKey(registryURL, name, reference, cacheAccept string) string {
	if manifestDigestReferencePattern.MatchString(reference) {
		identity := strings.Join([]string{registryURL, name, reference}, "\x00")
		sum := sha256.Sum256([]byte(identity))
		return hex.EncodeToString(sum[:])
	}
	if containerManifestAcceptsIndex(cacheAccept) {
		return h.containerManifestCacheKey(registryURL, name, reference, containerManifestIndexCacheClass)
	}
	return h.containerManifestCacheKey(registryURL, name, reference, cacheAccept)
}

func containerManifestAcceptsIndex(accept string) bool {
	return containerManifestAccepts(accept, ociImageIndexMediaType) || containerManifestAccepts(accept, dockerManifestListMediaType)
}

// containerManifestLegacyCacheKeys are the per-Accept keys older versions
// used. They are still written so a rollback finds the cache, and read when
// the shared key has no entry yet.
func (h *ContainerHandler) containerManifestLegacyCacheKeys(registryURL, name, reference, accept, cacheAccept, sharedKey string) []string {
	var keys []string
	for _, variant := range []string{cacheAccept, accept} {
		key := h.containerManifestCacheKey(registryURL, name, reference, variant)
		if key != sharedKey && !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	return keys
}

// loadContainerManifestForAccept returns the cached manifest for the client and
// whether it came from a per-Accept fallback key while the shared key holds a
// representation the client does not accept.
func (h *ContainerHandler) loadContainerManifestForAccept(ctx context.Context, registryURL, name, reference, accept, cacheAccept string) (*cachedContainerManifest, bool) {
	cacheKey := h.containerManifestSharedCacheKey(registryURL, name, reference, cacheAccept)
	cached, err := h.loadContainerManifest(ctx, cacheKey)
	if err != nil {
		h.proxy.Logger.Warn("failed to read cached container manifest", "error", err)
		return nil, false
	}
	if cached != nil && containerManifestCacheCompatible(accept, cached) {
		return cached, false
	}
	// The shared entry may hold a representation this client does not accept,
	// for example an OCI index for a client that only accepts Docker manifest
	// lists. Its own per-Accept entry is still written, so look there.
	sharedEntryExists := cached != nil

	for _, legacyCacheKey := range h.containerManifestLegacyCacheKeys(registryURL, name, reference, accept, cacheAccept, cacheKey) {
		cached, err = h.loadContainerManifest(ctx, legacyCacheKey)
		if err != nil {
			h.proxy.Logger.Warn("failed to read legacy cached container manifest", "error", err)
			return nil, false
		}
		if cached == nil || !containerManifestCacheCompatible(accept, cached) {
			continue
		}
		if !sharedEntryExists {
			if err := h.storeContainerManifest(ctx, cacheKey, cached); err != nil {
				h.proxy.Logger.Warn("failed to migrate cached container manifest", "error", err)
			}
		}
		return cached, sharedEntryExists
	}
	return nil, false
}

func (h *ContainerHandler) storeContainerManifestForAccept(ctx context.Context, registryURL, name, reference, accept, cacheAccept string, manifest *cachedContainerManifest) {
	cacheKey := h.containerManifestSharedCacheKey(registryURL, name, reference, cacheAccept)
	if err := h.storeContainerManifest(ctx, cacheKey, manifest); err != nil {
		h.proxy.Logger.Warn("failed to cache container manifest", "error", err)
	}

	h.storeContainerManifestLegacyEntries(ctx, registryURL, name, reference, accept, cacheAccept, manifest)
}

// storeContainerManifestLegacyEntries writes only the per-Accept keys.
func (h *ContainerHandler) storeContainerManifestLegacyEntries(ctx context.Context, registryURL, name, reference, accept, cacheAccept string, manifest *cachedContainerManifest) {
	cacheKey := h.containerManifestSharedCacheKey(registryURL, name, reference, cacheAccept)
	for _, legacyCacheKey := range h.containerManifestLegacyCacheKeys(registryURL, name, reference, accept, cacheAccept, cacheKey) {
		if err := h.storeContainerManifest(ctx, legacyCacheKey, manifest); err != nil {
			h.proxy.Logger.Warn("failed to cache legacy container manifest", "error", err)
		}
	}
}

func (h *ContainerHandler) loadContainerManifest(ctx context.Context, cacheKey string) (*cachedContainerManifest, error) {
	if h.proxy.DB == nil || h.proxy.Storage == nil {
		return nil, nil
	}
	entry, err := h.proxy.DB.GetMetadataCache(containerManifestCacheEcosystem, cacheKey)
	if err != nil || entry == nil {
		return nil, err
	}
	reader, err := h.proxy.Storage.Open(ctx, entry.StoragePath)
	if err != nil {
		return nil, nil
	}
	defer func() { _ = reader.Close() }()
	body, err := h.proxy.ReadMetadata(reader)
	if err != nil {
		return nil, err
	}
	h.proxy.recordMetadataRead(entry)

	manifest := &cachedContainerManifest{body: body, size: int64(len(body))}
	if entry.ContentType.Valid {
		manifest.contentType = entry.ContentType.String
	}
	if entry.ContentDigest.Valid {
		manifest.contentDigest = entry.ContentDigest.String
	} else {
		manifest.contentDigest = sha256Digest(body)
	}
	if entry.ETag.Valid {
		manifest.etag = entry.ETag.String
	}
	if entry.Size.Valid {
		manifest.size = entry.Size.Int64
	}
	if entry.LastModified.Valid {
		manifest.lastModified = entry.LastModified.Time
	}
	if entry.FetchedAt.Valid {
		manifest.fetchedAt = entry.FetchedAt.Time
	}
	return manifest, nil
}

func (h *ContainerHandler) storeContainerManifest(ctx context.Context, cacheKey string, manifest *cachedContainerManifest) error {
	size, err := h.storeContainerMetadata(ctx, containerManifestCacheEcosystem, cacheKey, manifest.body,
		manifest.etag, "", manifest.contentType, manifest.contentDigest, manifest.lastModified, manifest.fetchedAt)
	if err != nil {
		return fmt.Errorf("storing manifest: %w", err)
	}
	manifest.size = size
	return nil
}

func writeContainerManifest(w http.ResponseWriter, r *http.Request, manifest *cachedContainerManifest, stale bool) {
	if manifest.contentType != "" {
		w.Header().Set(headerContentType, manifest.contentType)
	}
	// A manifest known only from an upstream HEAD may lack a size.
	if manifest.size >= 0 {
		w.Header().Set(headerContentLength, strconv.FormatInt(manifest.size, 10))
	}
	if manifest.contentDigest != "" {
		w.Header().Set("Docker-Content-Digest", manifest.contentDigest)
	}
	if manifest.etag != "" {
		w.Header().Set(headerETag, manifest.etag)
	}
	if !manifest.lastModified.IsZero() {
		w.Header().Set(headerLastModified, manifest.lastModified.UTC().Format(http.TimeFormat))
	}
	if stale {
		w.Header().Set("Warning", containerStaleWarning)
	}
	if ifNoneMatchHits(r.Header.Get("If-None-Match"), manifest.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if !manifest.lastModified.IsZero() {
		if modifiedSince, err := http.ParseTime(r.Header.Get("If-Modified-Since")); err == nil && !manifest.lastModified.After(modifiedSince) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(manifest.body)
	}
}

// containerManifestAccept joins every Accept header: Docker sends one per
// media type, and the first alone would rule out the others.
func containerManifestAccept(r *http.Request) string {
	if accept := strings.Join(r.Header.Values("Accept"), ", "); accept != "" {
		return accept
	}
	return strings.Join([]string{
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.docker.distribution.manifest.v1+prettyjws",
	}, ", ")
}

func normalizeContainerManifestAccept(accept string) string {
	mediaTypes := make(map[string]struct{})
	for _, value := range strings.Split(accept, ",") {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		mediaType, params, err := mime.ParseMediaType(value)
		if err != nil {
			mediaTypes[strings.ToLower(value)] = struct{}{}
			continue
		}
		paramKeys := make([]string, 0, len(params))
		for key := range params {
			paramKeys = append(paramKeys, key)
		}
		sort.Strings(paramKeys)
		canonical := strings.ToLower(mediaType)
		for _, key := range paramKeys {
			value := params[key]
			if strings.EqualFold(key, "q") {
				if quality, err := strconv.ParseFloat(value, 64); err == nil {
					if quality == 1 {
						continue
					}
					value = strconv.FormatFloat(quality, 'g', -1, 64)
				}
			}
			canonical += ";" + strings.ToLower(key) + "=" + value
		}
		mediaTypes[canonical] = struct{}{}
	}
	canonicalMediaTypes := make([]string, 0, len(mediaTypes))
	for mediaType := range mediaTypes {
		canonicalMediaTypes = append(canonicalMediaTypes, mediaType)
	}
	sort.Strings(canonicalMediaTypes)
	return strings.Join(canonicalMediaTypes, ",")
}

func containerManifestAccepts(accept, contentType string) bool {
	contentType, contentParams, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	contentType = strings.ToLower(contentType)
	contentMajor, contentMinor, found := strings.Cut(contentType, "/")
	if !found {
		return false
	}

	bestMediaTypeSpecificity := -1
	bestParameterSpecificity := 0
	bestQuality := 0.0
	for _, value := range strings.Split(accept, ",") {
		mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(value))
		if err != nil {
			continue
		}
		mediaType = strings.ToLower(mediaType)
		major, minor, found := strings.Cut(mediaType, "/")
		if found && containerAcceptRangeMatches(major, minor, params, contentMajor, contentMinor, contentParams) {
			mediaTypeSpecificity, parameterSpecificity := containerAcceptSpecificity(major, minor, params)
			if mediaTypeSpecificity > bestMediaTypeSpecificity ||
				(mediaTypeSpecificity == bestMediaTypeSpecificity && parameterSpecificity > bestParameterSpecificity) {
				bestMediaTypeSpecificity = mediaTypeSpecificity
				bestParameterSpecificity = parameterSpecificity
				bestQuality = containerAcceptQuality(params)
			}
		}
	}
	return bestQuality > 0
}

func containerManifestCacheCompatible(accept string, manifest *cachedContainerManifest) bool {
	return manifest.contentType == "" || containerManifestAccepts(accept, manifest.contentType)
}

func containerAcceptRangeMatches(major, minor string, params map[string]string, contentMajor, contentMinor string, contentParams map[string]string) bool {
	if (major != "*" && major != contentMajor) || (minor != "*" && minor != contentMinor) {
		return false
	}
	for key, value := range params {
		if strings.EqualFold(key, "q") {
			continue
		}
		if contentParams[key] != value {
			return false
		}
	}
	return true
}

func containerAcceptSpecificity(major, minor string, params map[string]string) (int, int) {
	parameterSpecificity := 0
	for key := range params {
		if !strings.EqualFold(key, "q") {
			parameterSpecificity++
		}
	}

	switch {
	case major == "*" && minor == "*":
		return containerAcceptWildcardSpecificity, parameterSpecificity
	case major == "*" || minor == "*":
		return containerAcceptTypeWildcardSpecificity, parameterSpecificity
	default:
		return containerAcceptExactSpecificity, parameterSpecificity
	}
}

func containerAcceptQuality(params map[string]string) float64 {
	value, ok := params["q"]
	if !ok {
		return 1
	}
	quality, err := strconv.ParseFloat(value, 64)
	if err != nil || quality < 0 || quality > 1 {
		return 0
	}
	return quality
}

func copyContainerManifestHeaders(destination, source http.Header) {
	for _, header := range []string{headerContentType, headerContentLength, "Docker-Content-Digest", headerETag, headerLastModified, "WWW-Authenticate"} {
		if value := source.Get(header); value != "" {
			destination.Set(header, value)
		}
	}
}

func parseHTTPTime(value string) time.Time {
	parsed, _ := http.ParseTime(value)
	return parsed
}

func shouldServeStaleManifest(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

func sha256Digest(body []byte) string {
	digest := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(digest[:])
}
