package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/git-pkgs/proxy/internal/metadata/oci"
)

const (
	containerTagsCacheEcosystem = "oci-tags"
	// containerTagsCacheFormat versions the tag-list cache identity. Rows
	// written before it hold a Link already rewritten for the route that
	// filled them, while this format stores the upstream Link verbatim. The
	// two must not share rows: an older binary would serve a raw Link as is.
	containerTagsCacheFormat = "raw-link"
)

type cachedContainerTags struct {
	body        []byte
	contentType string
	etag        string
	link        string
	size        int64
	fetchedAt   time.Time
}

func (h *ContainerHandler) serveTagsList(w http.ResponseWriter, r *http.Request, registryURL, name string) {
	// ns only selects the registry; it is neither forwarded upstream nor part
	// of the cache identity, so all routes to one registry share tag lists.
	query := r.URL.Query()
	query.Del(namespaceQueryParam)
	cacheKey := h.containerTagsCacheKey(registryURL, name, query)
	cached, err := h.loadContainerTags(r.Context(), cacheKey)
	if err != nil {
		h.proxy.Logger.Warn("failed to read cached container tag list", "error", err)
		cached = nil
	}
	if cached != nil && h.containerTagsFresh(cached) {
		h.writeContainerTags(w, r, registryURL, cached, false)
		return
	}

	upstreamURL := fmt.Sprintf("%s/v2/%s/tags/list", registryURL, name)
	if encoded := query.Encode(); encoded != "" {
		upstreamURL += "?" + encoded
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		h.containerError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to create request")
		return
	}
	req.Header.Set("Accept", "application/json")
	if cached != nil && cached.etag != "" {
		req.Header.Set("If-None-Match", cached.etag)
	}

	resp, err := h.proxy.HTTPClient.Do(req)
	if err != nil {
		h.serveStaleTagsOrError(w, r, registryURL, cached, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotModified && cached != nil {
		cached.fetchedAt = time.Now()
		if err := h.storeContainerTags(r.Context(), cacheKey, cached); err != nil {
			h.proxy.Logger.Warn("failed to refresh cached container tag list", "error", err)
		}
		h.writeContainerTags(w, r, registryURL, cached, false)
		return
	}
	if resp.StatusCode != http.StatusOK {
		if cached != nil && shouldServeStaleManifest(resp.StatusCode) {
			h.writeContainerTags(w, r, registryURL, cached, true)
			return
		}
		h.proxy.relayResponse(w, r, resp, copyContainerTagsHeaders)
		return
	}

	body, err := h.proxy.ReadMetadata(resp.Body)
	if err != nil {
		h.serveStaleTagsOrError(w, r, registryURL, cached, fmt.Errorf("reading tag list: %w", err))
		return
	}
	// The upstream Link is stored verbatim and rewritten per request because
	// clients on different routes share this cache entry.
	tags := &cachedContainerTags{
		body:        body,
		contentType: resp.Header.Get(headerContentType),
		etag:        resp.Header.Get(headerETag),
		link:        strings.Join(resp.Header.Values("Link"), ", "),
		size:        int64(len(body)),
		fetchedAt:   time.Now(),
	}
	if tags.contentType == "" {
		tags.contentType = contentTypeJSON
	}
	if err := h.storeContainerTags(r.Context(), cacheKey, tags); err != nil {
		h.proxy.Logger.Warn("failed to cache container tag list", "error", err)
	}
	h.writeContainerTags(w, r, registryURL, tags, false)
}

func (h *ContainerHandler) serveStaleTagsOrError(w http.ResponseWriter, r *http.Request, registryURL string, cached *cachedContainerTags, err error) {
	if cached != nil {
		h.proxy.Logger.Warn("upstream tag list fetch failed, serving stale cache", "error", err)
		h.writeContainerTags(w, r, registryURL, cached, true)
		return
	}
	if writeUpstreamHeld(w, err) {
		return
	}
	h.proxy.Logger.Error("failed to fetch container tag list", "error", err)
	h.containerError(w, http.StatusBadGateway, "INTERNAL_ERROR", "failed to fetch from upstream")
}

func (h *ContainerHandler) containerTagsCacheKey(registryURL, name string, query url.Values) string {
	identity := containerTagsCacheFormat + "\x00" + registryURL + "\x00" + name + "\x00" + query.Encode()
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])
}

func (h *ContainerHandler) containerTagsFresh(tags *cachedContainerTags) bool {
	return h.proxy.MetadataTTL > 0 && !tags.fetchedAt.IsZero() && time.Since(tags.fetchedAt) < h.proxy.MetadataTTL
}

func (h *ContainerHandler) loadContainerTags(ctx context.Context, cacheKey string) (*cachedContainerTags, error) {
	if h.proxy.DB == nil || h.proxy.Storage == nil {
		return nil, nil
	}
	entry, err := h.proxy.DB.GetMetadataCache(containerTagsCacheEcosystem, cacheKey)
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

	tags := &cachedContainerTags{body: body, contentType: contentTypeJSON, size: int64(len(body))}
	if entry.ContentType.Valid {
		tags.contentType = entry.ContentType.String
	}
	if entry.ETag.Valid {
		tags.etag = entry.ETag.String
	}
	if entry.Link.Valid {
		tags.link = entry.Link.String
	}
	if entry.Size.Valid {
		tags.size = entry.Size.Int64
	}
	if entry.FetchedAt.Valid {
		tags.fetchedAt = entry.FetchedAt.Time
	}
	return tags, nil
}

func (h *ContainerHandler) storeContainerTags(ctx context.Context, cacheKey string, tags *cachedContainerTags) error {
	size, err := h.storeContainerMetadata(ctx, containerTagsCacheEcosystem, cacheKey, tags.body,
		tags.etag, tags.link, tags.contentType, "", time.Time{}, tags.fetchedAt)
	if err != nil {
		return fmt.Errorf("storing tag list: %w", err)
	}
	tags.size = size
	return nil
}

func (h *ContainerHandler) writeContainerTags(w http.ResponseWriter, r *http.Request, registryURL string, tags *cachedContainerTags, stale bool) {
	w.Header().Set(headerContentType, tags.contentType)
	w.Header().Set(headerContentLength, strconv.FormatInt(tags.size, 10))
	if tags.etag != "" {
		w.Header().Set(headerETag, tags.etag)
	}
	link := h.rewriteContainerTagsLink(tags.link, registryURL, r.URL.Path, r.URL.Query().Get(namespaceQueryParam))
	if link != "" {
		w.Header().Set("Link", link)
	}
	if stale {
		w.Header().Set("Warning", containerStaleWarning)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(tags.body)
}

func copyContainerTagsHeaders(destination, source http.Header) {
	for _, header := range []string{headerContentType, headerContentLength, headerETag, "Link", "WWW-Authenticate"} {
		if value := source.Get(header); value != "" {
			destination.Set(header, value)
		}
	}
}

// rewriteContainerTagsLink points a tag list or referrers Link header back
// at the proxy; see oci.RewriteLink.
func (h *ContainerHandler) rewriteContainerTagsLink(link, registryURL, requestPath, namespace string) string {
	return oci.RewriteLink(link, oci.LinkOptions{
		ProxyURL:    h.proxyURL,
		RegistryURL: registryURL,
		RequestPath: requestPath,
		Namespace:   namespace,
	})
}
