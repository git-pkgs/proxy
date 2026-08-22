package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
)

const containerTagsCacheEcosystem = "oci-tags"

type cachedContainerTags struct {
	body        []byte
	contentType string
	etag        string
	size        int64
	fetchedAt   time.Time
}

func (h *ContainerHandler) serveTagsList(w http.ResponseWriter, r *http.Request, registryURL, name string) {
	cacheKey := h.containerTagsCacheKey(registryURL, name, r.URL.Query())
	cached, err := h.loadContainerTags(r.Context(), cacheKey)
	if err != nil {
		h.proxy.Logger.Warn("failed to read cached container tag list", "error", err)
		cached = nil
	}
	if cached != nil && h.containerTagsFresh(cached) {
		writeContainerTags(w, cached, false)
		return
	}

	upstreamURL := fmt.Sprintf("%s/v2/%s/tags/list", registryURL, name)
	if query := r.URL.Query().Encode(); query != "" {
		upstreamURL += "?" + query
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
		h.serveStaleTagsOrError(w, cached, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotModified && cached != nil {
		cached.fetchedAt = time.Now()
		if err := h.storeContainerTags(r.Context(), cacheKey, cached); err != nil {
			h.proxy.Logger.Warn("failed to refresh cached container tag list", "error", err)
		}
		writeContainerTags(w, cached, false)
		return
	}
	if resp.StatusCode != http.StatusOK {
		if cached != nil && shouldServeStaleManifest(resp.StatusCode) {
			writeContainerTags(w, cached, true)
			return
		}
		copyContainerTagsHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	body, err := h.proxy.ReadMetadata(resp.Body)
	if err != nil {
		h.serveStaleTagsOrError(w, cached, fmt.Errorf("reading tag list: %w", err))
		return
	}
	tags := &cachedContainerTags{
		body:        body,
		contentType: resp.Header.Get("Content-Type"),
		etag:        resp.Header.Get("ETag"),
		size:        int64(len(body)),
		fetchedAt:   time.Now(),
	}
	if tags.contentType == "" {
		tags.contentType = contentTypeJSON
	}
	if err := h.storeContainerTags(r.Context(), cacheKey, tags); err != nil {
		h.proxy.Logger.Warn("failed to cache container tag list", "error", err)
	}
	writeContainerTags(w, tags, false)
}

func (h *ContainerHandler) serveStaleTagsOrError(w http.ResponseWriter, cached *cachedContainerTags, err error) {
	if cached != nil {
		h.proxy.Logger.Warn("upstream tag list fetch failed, serving stale cache", "error", err)
		writeContainerTags(w, cached, true)
		return
	}
	h.proxy.Logger.Error("failed to fetch container tag list", "error", err)
	h.containerError(w, http.StatusBadGateway, "INTERNAL_ERROR", "failed to fetch from upstream")
}

func (h *ContainerHandler) containerTagsCacheKey(registryURL, name string, query url.Values) string {
	identity := registryURL + "\x00" + name + "\x00" + query.Encode()
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

	tags := &cachedContainerTags{body: body, contentType: contentTypeJSON, size: int64(len(body))}
	if entry.ContentType.Valid {
		tags.contentType = entry.ContentType.String
	}
	if entry.ETag.Valid {
		tags.etag = entry.ETag.String
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
	if h.proxy.DB == nil || h.proxy.Storage == nil {
		return nil
	}
	storagePath := metadataStoragePath(containerTagsCacheEcosystem, cacheKey)
	size, _, err := h.proxy.Storage.Store(ctx, storagePath, bytes.NewReader(tags.body))
	if err != nil {
		return fmt.Errorf("storing tag list: %w", err)
	}
	tags.size = size
	return h.proxy.DB.UpsertMetadataCache(&database.MetadataCacheEntry{
		Ecosystem:   containerTagsCacheEcosystem,
		Name:        cacheKey,
		StoragePath: storagePath,
		ETag:        sql.NullString{String: tags.etag, Valid: tags.etag != ""},
		ContentType: sql.NullString{String: tags.contentType, Valid: tags.contentType != ""},
		Size:        sql.NullInt64{Int64: size, Valid: true},
		FetchedAt:   sql.NullTime{Time: tags.fetchedAt, Valid: !tags.fetchedAt.IsZero()},
	})
}

func writeContainerTags(w http.ResponseWriter, tags *cachedContainerTags, stale bool) {
	w.Header().Set("Content-Type", tags.contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(tags.size, 10))
	if tags.etag != "" {
		w.Header().Set("ETag", tags.etag)
	}
	if stale {
		w.Header().Set("Warning", containerStaleWarning)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(tags.body)
}

func copyContainerTagsHeaders(destination, source http.Header) {
	for _, header := range []string{"Content-Type", "Content-Length", "ETag", "WWW-Authenticate"} {
		if value := source.Get(header); value != "" {
			destination.Set(header, value)
		}
	}
}
