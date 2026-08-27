package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
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

var manifestDigestReferencePattern = regexp.MustCompile(`^[a-z0-9]+:[a-f0-9]+$`)

type cachedContainerManifest struct {
	body          []byte
	contentType   string
	contentDigest string
	etag          string
	size          int64
	fetchedAt     time.Time
}

func (h *ContainerHandler) serveManifest(w http.ResponseWriter, r *http.Request, registryURL, name, reference string) {
	accept := containerManifestAccept(r)
	cacheAccept := normalizeContainerManifestAccept(accept)
	cacheKey := h.containerManifestCacheKey(registryURL, name, reference, cacheAccept)
	cached := h.loadContainerManifestForAccept(r.Context(), registryURL, name, reference, accept, cacheKey)

	immutable := manifestDigestReferencePattern.MatchString(reference)
	if cached != nil && (immutable || h.containerManifestFresh(cached)) {
		writeContainerManifest(w, r.Method, cached, false)
		return
	}

	upstreamURL := fmt.Sprintf("%s/v2/%s/manifests/%s", registryURL, name, reference)
	req, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, nil)
	if err != nil {
		h.containerError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to create request")
		return
	}
	req.Header.Set("Accept", accept)
	if cached != nil && cached.etag != "" {
		req.Header.Set("If-None-Match", cached.etag)
	}

	resp, err := h.proxy.HTTPClient.Do(req)
	if err != nil {
		h.serveStaleManifestOrError(w, r, cached, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotModified && cached != nil {
		cached.fetchedAt = time.Now()
		h.storeContainerManifestForAccept(r.Context(), registryURL, name, reference, accept, cacheAccept, cached)
		writeContainerManifest(w, r.Method, cached, false)
		return
	}
	if resp.StatusCode != http.StatusOK {
		if cached != nil && shouldServeStaleManifest(resp.StatusCode) {
			writeContainerManifest(w, r.Method, cached, true)
			return
		}
		copyContainerManifestHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	if r.Method == http.MethodHead {
		copyContainerManifestHeaders(w.Header(), resp.Header)
		w.WriteHeader(http.StatusOK)
		return
	}

	body, err := h.proxy.ReadMetadata(resp.Body)
	if err != nil {
		h.serveStaleManifestOrError(w, r, cached, fmt.Errorf("reading manifest: %w", err))
		return
	}
	manifest := &cachedContainerManifest{
		body:          body,
		contentType:   resp.Header.Get("Content-Type"),
		contentDigest: resp.Header.Get("Docker-Content-Digest"),
		etag:          resp.Header.Get("ETag"),
		size:          int64(len(body)),
		fetchedAt:     time.Now(),
	}
	if manifest.contentDigest == "" {
		manifest.contentDigest = sha256Digest(body)
	}
	h.storeContainerManifestForAccept(r.Context(), registryURL, name, reference, accept, cacheAccept, manifest)
	if manifest.contentDigest != reference && manifestDigestReferencePattern.MatchString(manifest.contentDigest) {
		h.storeContainerManifestForAccept(r.Context(), registryURL, name, manifest.contentDigest, accept, cacheAccept, manifest)
	}
	writeContainerManifest(w, r.Method, manifest, false)
}

func (h *ContainerHandler) serveStaleManifestOrError(w http.ResponseWriter, r *http.Request, cached *cachedContainerManifest, err error) {
	if cached != nil {
		h.proxy.Logger.Warn("upstream manifest fetch failed, serving stale cache", "error", err)
		writeContainerManifest(w, r.Method, cached, true)
		return
	}
	h.proxy.Logger.Error("failed to fetch manifest", "error", err)
	h.containerError(w, http.StatusBadGateway, "INTERNAL_ERROR", "failed to fetch from upstream")
}

func (h *ContainerHandler) containerManifestFresh(manifest *cachedContainerManifest) bool {
	return h.proxy.MetadataTTL > 0 && !manifest.fetchedAt.IsZero() && time.Since(manifest.fetchedAt) < h.proxy.MetadataTTL
}

func (h *ContainerHandler) containerManifestCacheKey(registryURL, name, reference, accept string) string {
	identity := strings.Join([]string{registryURL, name, reference, accept}, "\x00")
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])
}

func (h *ContainerHandler) loadContainerManifestForAccept(ctx context.Context, registryURL, name, reference, accept, cacheKey string) *cachedContainerManifest {
	cached, err := h.loadContainerManifest(ctx, cacheKey)
	if err != nil {
		h.proxy.Logger.Warn("failed to read cached container manifest", "error", err)
		return nil
	}
	if cached != nil {
		if containerManifestCacheCompatible(accept, cached) {
			return cached
		}
		return nil
	}

	legacyCacheKey := h.containerManifestCacheKey(registryURL, name, reference, accept)
	if legacyCacheKey == cacheKey {
		return nil
	}
	cached, err = h.loadContainerManifest(ctx, legacyCacheKey)
	if err != nil {
		h.proxy.Logger.Warn("failed to read legacy cached container manifest", "error", err)
		return nil
	}
	if cached == nil || !containerManifestCacheCompatible(accept, cached) {
		return nil
	}
	if err := h.storeContainerManifest(ctx, cacheKey, cached); err != nil {
		h.proxy.Logger.Warn("failed to migrate cached container manifest", "error", err)
	}
	return cached
}

func (h *ContainerHandler) storeContainerManifestForAccept(ctx context.Context, registryURL, name, reference, accept, cacheAccept string, manifest *cachedContainerManifest) {
	cacheKey := h.containerManifestCacheKey(registryURL, name, reference, cacheAccept)
	if err := h.storeContainerManifest(ctx, cacheKey, manifest); err != nil {
		h.proxy.Logger.Warn("failed to cache container manifest", "error", err)
	}

	legacyCacheKey := h.containerManifestCacheKey(registryURL, name, reference, accept)
	if legacyCacheKey == cacheKey {
		return
	}
	if err := h.storeContainerManifest(ctx, legacyCacheKey, manifest); err != nil {
		h.proxy.Logger.Warn("failed to cache legacy container manifest", "error", err)
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
	if entry.FetchedAt.Valid {
		manifest.fetchedAt = entry.FetchedAt.Time
	}
	return manifest, nil
}

func (h *ContainerHandler) storeContainerManifest(ctx context.Context, cacheKey string, manifest *cachedContainerManifest) error {
	size, err := h.storeContainerMetadata(ctx, containerManifestCacheEcosystem, cacheKey, manifest.body,
		manifest.etag, "", manifest.contentType, manifest.contentDigest, manifest.fetchedAt)
	if err != nil {
		return fmt.Errorf("storing manifest: %w", err)
	}
	manifest.size = size
	return nil
}

func writeContainerManifest(w http.ResponseWriter, method string, manifest *cachedContainerManifest, stale bool) {
	if manifest.contentType != "" {
		w.Header().Set("Content-Type", manifest.contentType)
	}
	w.Header().Set("Content-Length", strconv.FormatInt(manifest.size, 10))
	if manifest.contentDigest != "" {
		w.Header().Set("Docker-Content-Digest", manifest.contentDigest)
	}
	if manifest.etag != "" {
		w.Header().Set("ETag", manifest.etag)
	}
	if stale {
		w.Header().Set("Warning", containerStaleWarning)
	}
	w.WriteHeader(http.StatusOK)
	if method != http.MethodHead {
		_, _ = w.Write(manifest.body)
	}
}

func containerManifestAccept(r *http.Request) string {
	if accept := r.Header.Get("Accept"); accept != "" {
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
	for _, header := range []string{"Content-Type", "Content-Length", "Docker-Content-Digest", "ETag", "WWW-Authenticate"} {
		if value := source.Get(header); value != "" {
			destination.Set(header, value)
		}
	}
}

func shouldServeStaleManifest(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

func sha256Digest(body []byte) string {
	digest := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(digest[:])
}
