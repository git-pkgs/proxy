package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/git-pkgs/proxy/internal/metadata/oci"
)

const (
	containerManifestCacheEcosystem = "oci-manifest"
	containerManifestTagEcosystem   = "oci-manifest-tag"
	containerStaleWarning           = `110 - "Response is Stale"`
)

const (
	// containerManifestIndexCacheClass replaces the Accept header in tag cache
	// keys for clients that accept an index type.
	containerManifestIndexCacheClass = "index"
	ociImageIndexMediaType           = oci.ImageIndexMediaType
	dockerManifestListMediaType      = oci.DockerManifestListMediaType
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

// writeManifest writes the manifest that reference resolved to, unless the
// denylist names its digest.
func (h *ContainerHandler) writeManifest(w http.ResponseWriter, r *http.Request, cacheName, reference string, manifest *cachedContainerManifest, stale bool) {
	if h.manifestDenied(w, cacheName, manifest.contentDigest) {
		return
	}
	h.recordImageManifest(cacheName, reference, manifest, stale)
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
// all clients that accept an index type, so Docker, containerd and Podman read
// the same entry; containerManifestSharable says what may go there. Clients
// without index support keep a key per normalized Accept header, because
// registries may convert an index into a single platform manifest for them.
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
	return oci.AcceptsIndex(accept)
}

// containerManifestNames reports whether accept lists mediaType itself and
// doesn't refuse it with q=0; see oci.Names.
func containerManifestNames(accept, mediaType string) bool {
	return oci.Names(accept, mediaType)
}

// containerManifestSharable reports whether manifest may go under the shared
// key of reference. Every client that accepts an index type reads a tag's
// shared entry, so it takes an index, or what a client naming both index
// types got: registries convert an index into the default platform's manifest
// for a client that lacks the index's type, and that manifest must not stand in
// for the index for the others.
func containerManifestSharable(reference, cacheAccept string, manifest *cachedContainerManifest) bool {
	if manifestDigestReferencePattern.MatchString(reference) || !containerManifestAcceptsIndex(cacheAccept) {
		return true
	}
	if oci.IsIndexMediaType(manifest.contentType) {
		return true
	}
	return containerManifestNames(cacheAccept, ociImageIndexMediaType) && containerManifestNames(cacheAccept, dockerManifestListMediaType)
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
	cached, fromFallback := h.loadContainerManifestCandidate(ctx, registryURL, name, reference, accept, cacheAccept)
	if manifestDigestReferencePattern.MatchString(reference) || h.proxy.DB == nil ||
		(cached != nil && h.containerManifestFresh(cached)) {
		return cached, fromFallback
	}
	// Only a missing or stale compatible body can benefit from a saved HEAD
	// resolution. An incompatible shared representation must not suppress it.
	cacheKey := h.containerManifestSharedCacheKey(registryURL, name, reference, cacheAccept)
	keys := append([]string{cacheKey}, h.containerManifestLegacyCacheKeys(registryURL, name, reference, accept, cacheAccept, cacheKey)...)
	for _, key := range keys {
		cached = h.loadResolvedContainerManifest(ctx, registryURL, name, accept, cacheAccept, key, cached)
	}
	return cached, fromFallback
}

// Resolutions obey the same sharing boundary as manifest bodies: a converted
// single-platform response is visible only to the Accept variant that got it.
func (h *ContainerHandler) loadResolvedContainerManifest(ctx context.Context, registryURL, name, accept, cacheAccept, cacheKey string, cached *cachedContainerManifest) *cachedContainerManifest {
	entry, err := h.proxy.DB.GetMetadataCache(containerManifestTagEcosystem, cacheKey)
	if err != nil || entry == nil || !entry.ContentDigest.Valid ||
		!manifestDigestReferencePattern.MatchString(entry.ContentDigest.String) ||
		(cached != nil && !entry.FetchedAt.Time.After(cached.fetchedAt)) {
		return cached
	}
	digest := entry.ContentDigest.String
	stored, err := h.loadContainerManifest(ctx, h.containerManifestSharedCacheKey(registryURL, name, digest, cacheAccept))
	if err == nil && stored != nil && stored.contentDigest == digest && containerManifestCacheCompatible(accept, stored) {
		stored.fetchedAt = entry.FetchedAt.Time
		h.proxy.recordMetadataRead(entry)
		return stored
	}
	return cached
}

func (h *ContainerHandler) loadContainerManifestCandidate(ctx context.Context, registryURL, name, reference, accept, cacheAccept string) (*cachedContainerManifest, bool) {
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
		if !sharedEntryExists && containerManifestSharable(reference, cacheAccept, cached) {
			if err := h.storeContainerManifest(ctx, cacheKey, cached); err != nil {
				h.proxy.Logger.Warn("failed to migrate cached container manifest", "error", err)
			}
		}
		return cached, sharedEntryExists
	}
	return nil, false
}

func (h *ContainerHandler) storeContainerManifestForAccept(ctx context.Context, registryURL, name, reference, accept, cacheAccept string, manifest *cachedContainerManifest) {
	if containerManifestSharable(reference, cacheAccept, manifest) {
		cacheKey := h.containerManifestSharedCacheKey(registryURL, name, reference, cacheAccept)
		if err := h.storeContainerManifest(ctx, cacheKey, manifest); err != nil {
			h.proxy.Logger.Warn("failed to cache container manifest", "error", err)
		}
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
	return oci.DefaultManifestAccept
}

// normalizeContainerManifestAccept is accept in the form cache keys use;
// see oci.NormalizeAccept.
func normalizeContainerManifestAccept(accept string) string {
	return oci.NormalizeAccept(accept)
}

// containerManifestAccepts reports whether accept allows contentType; see
// oci.Accepts.
func containerManifestAccepts(accept, contentType string) bool {
	return oci.Accepts(accept, contentType)
}

func containerManifestCacheCompatible(accept string, manifest *cachedContainerManifest) bool {
	return manifest.contentType == "" || containerManifestAccepts(accept, manifest.contentType)
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
