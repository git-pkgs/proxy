package handler

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	containerReferrersCacheEcosystem = "oci-referrers"
	containerReferrersMediaType      = "application/vnd.oci.image.index.v1+json"
	referrersMatchCount              = 3 // full match + name + digest
)

var (
	// referrersPathPattern matches referrers paths: {name}/referrers/{digest}
	referrersPathPattern   = regexp.MustCompile(`^(.+)/referrers/([^/]+)$`)
	referrersDigestPattern = regexp.MustCompile(`^(sha256:[a-f0-9]{64}|sha512:[a-f0-9]{128})$`)
)

type cachedContainerReferrers struct {
	body        []byte
	contentType string
	etag        string
	link        string
	size        int64
	fetchedAt   time.Time
}

// parseReferrersPath extracts repository name and subject digest from a
// referrers path.
func (h *ContainerHandler) parseReferrersPath(path string) (name, digest string) {
	matches := referrersPathPattern.FindStringSubmatch(path)
	if len(matches) != referrersMatchCount {
		return "", ""
	}
	return matches[1], matches[2]
}

// handleReferrers serves the OCI 1.1 referrers API.
// Path format: {name}/referrers/{digest}
func (h *ContainerHandler) handleReferrers(w http.ResponseWriter, r *http.Request, path string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	name, digest := h.parseReferrersPath(path)
	if name == "" || !referrersDigestPattern.MatchString(digest) {
		h.containerError(w, http.StatusBadRequest, "DIGEST_INVALID", "invalid referrers digest")
		return
	}

	registryURL, upstreamName, _, ok := h.registryForName(name)
	if !ok {
		h.containerError(w, http.StatusNotFound, "NAME_UNKNOWN", "unknown upstream registry")
		return
	}

	h.proxy.Logger.Info("container referrers request", "name", upstreamName, "digest", digest)
	h.serveReferrers(w, r, registryURL, upstreamName, digest)
}

func (h *ContainerHandler) serveReferrers(w http.ResponseWriter, r *http.Request, registryURL, name, digest string) {
	// artifactType is never forwarded, so the upstream returns the full index
	// and one row serves every filter. Without OCI-Filters-Applied in the
	// response, clients filter the index themselves, as the spec requires.
	query := r.URL.Query()
	query.Del("artifactType")
	// Same identity shape as manifests (registry, name, reference, variant);
	// the oci-referrers ecosystem keeps the rows apart.
	cacheKey := h.containerManifestCacheKey(registryURL, name, digest, query.Encode())
	cached, err := h.loadContainerReferrers(r.Context(), cacheKey)
	if err != nil {
		h.proxy.Logger.Warn("failed to read cached container referrers", "error", err)
		cached = nil
	}
	if cached != nil && h.containerReferrersFresh(cached) {
		h.writeContainerReferrers(w, r, registryURL, cached, false)
		return
	}

	upstreamURL := fmt.Sprintf("%s/v2/%s/referrers/%s", registryURL, name, digest)
	if encoded := query.Encode(); encoded != "" {
		upstreamURL += "?" + encoded
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		h.containerError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to create request")
		return
	}
	req.Header.Set("Accept", containerReferrersMediaType)
	if cached != nil && cached.etag != "" {
		req.Header.Set("If-None-Match", cached.etag)
	}

	resp, err := h.proxy.HTTPClient.Do(req)
	if err != nil {
		h.serveStaleReferrersOrFallback(w, r, registryURL, cached, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotModified && cached != nil {
		cached.fetchedAt = time.Now()
		if err := h.storeContainerReferrers(r.Context(), cacheKey, cached); err != nil {
			h.proxy.Logger.Warn("failed to refresh cached container referrers", "error", err)
		}
		h.writeContainerReferrers(w, r, registryURL, cached, false)
		return
	}
	if resp.StatusCode != http.StatusOK {
		if shouldServeStaleManifest(resp.StatusCode) {
			h.serveStaleReferrersOrFallback(w, r, registryURL, cached,
				fmt.Errorf("upstream returned status %d", resp.StatusCode))
			return
		}
		// A 404 tells the client the registry has no referrers API, so it
		// falls back to the sha256-<hex> tag schema served as manifests.
		h.proxy.relayResponse(w, r, resp, copyContainerTagsHeaders)
		return
	}

	body, err := h.proxy.ReadMetadata(resp.Body)
	if err != nil {
		h.serveStaleReferrersOrFallback(w, r, registryURL, cached, fmt.Errorf("reading referrers: %w", err))
		return
	}
	if !json.Valid(body) {
		h.serveStaleReferrersOrFallback(w, r, registryURL, cached, errors.New("upstream referrers response is not JSON"))
		return
	}
	referrers := &cachedContainerReferrers{
		body:        body,
		contentType: resp.Header.Get(headerContentType),
		etag:        resp.Header.Get(headerETag),
		// Stored as sent upstream and rewritten when served, so a row shared
		// by several client paths to the same registry links back correctly.
		link:      strings.Join(resp.Header.Values("Link"), ", "),
		size:      int64(len(body)),
		fetchedAt: time.Now(),
	}
	if referrers.contentType == "" {
		referrers.contentType = containerReferrersMediaType
	}
	if err := h.storeContainerReferrers(r.Context(), cacheKey, referrers); err != nil {
		h.proxy.Logger.Warn("failed to cache container referrers", "error", err)
	}
	h.writeContainerReferrers(w, r, registryURL, referrers, false)
}

// serveStaleReferrersOrFallback serves a cached index when the upstream cannot
// answer. Without one it returns 404, the signal clients already got before
// the proxy served this endpoint: they fall back to the tag schema, whose
// manifests may well be cached.
func (h *ContainerHandler) serveStaleReferrersOrFallback(w http.ResponseWriter, r *http.Request, registryURL string, cached *cachedContainerReferrers, err error) {
	if cached != nil {
		h.proxy.Logger.Warn("upstream referrers fetch failed, serving stale cache", "error", err)
		h.writeContainerReferrers(w, r, registryURL, cached, true)
		return
	}
	h.proxy.Logger.Warn("upstream referrers fetch failed, answering without referrers API", "error", err)
	h.containerError(w, http.StatusNotFound, "UNSUPPORTED", "referrers unavailable from upstream")
}

func (h *ContainerHandler) containerReferrersFresh(referrers *cachedContainerReferrers) bool {
	return h.proxy.MetadataTTL > 0 && !referrers.fetchedAt.IsZero() && time.Since(referrers.fetchedAt) < h.proxy.MetadataTTL
}

func (h *ContainerHandler) loadContainerReferrers(ctx context.Context, cacheKey string) (*cachedContainerReferrers, error) {
	entry, body, err := h.loadContainerMetadata(ctx, containerReferrersCacheEcosystem, cacheKey)
	if err != nil || entry == nil {
		return nil, err
	}
	referrers := &cachedContainerReferrers{
		body:        body,
		contentType: cmp.Or(entry.ContentType.String, containerReferrersMediaType),
		etag:        entry.ETag.String,
		link:        entry.Link.String,
		size:        int64(len(body)),
		fetchedAt:   entry.FetchedAt.Time,
	}
	if entry.Size.Valid {
		referrers.size = entry.Size.Int64
	}
	return referrers, nil
}

func (h *ContainerHandler) storeContainerReferrers(ctx context.Context, cacheKey string, referrers *cachedContainerReferrers) error {
	size, err := h.storeContainerMetadata(ctx, containerReferrersCacheEcosystem, cacheKey, referrers.body,
		referrers.etag, referrers.link, referrers.contentType, "", time.Time{}, referrers.fetchedAt)
	if err != nil {
		return fmt.Errorf("storing referrers: %w", err)
	}
	referrers.size = size
	return nil
}

func (h *ContainerHandler) writeContainerReferrers(w http.ResponseWriter, r *http.Request, registryURL string, referrers *cachedContainerReferrers, stale bool) {
	w.Header().Set(headerContentType, referrers.contentType)
	w.Header().Set(headerContentLength, strconv.FormatInt(referrers.size, 10))
	if referrers.etag != "" {
		w.Header().Set(headerETag, referrers.etag)
	}
	if link := h.rewriteContainerTagsLink(referrers.link, registryURL, r.URL.Path); link != "" {
		w.Header().Set("Link", link)
	}
	if stale {
		w.Header().Set("Warning", containerStaleWarning)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(referrers.body)
}
