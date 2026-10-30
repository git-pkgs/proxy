package handler

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
)

// swiftDocument is a release list or manifest as the upstream sent it.
type swiftDocument struct {
	body        []byte
	contentType string
	// link is the upstream Link header, its values joined, with the
	// upstream's URLs.
	link      string
	fetchedAt time.Time
}

// fetchDocument returns the document at upstreamURL, from the metadata cache
// under cacheKey while it is fresh or when the upstream fails, and otherwise
// from upstream, storing it. The upstream Link header is cached with it,
// since it carries the pagination of release lists and the version-specific
// manifests a client picks from.
func (h *SwiftHandler) fetchDocument(ctx context.Context, cacheKey, upstreamURL, accept string) (*swiftDocument, error) {
	cached := h.loadDocument(ctx, cacheKey)
	if cached != nil && h.proxy.MetadataTTL > 0 && time.Since(cached.fetchedAt) < h.proxy.MetadataTTL {
		return cached, nil
	}
	body, contentType, headers, err := h.fetchMetadataWithHeaders(ctx, upstreamURL, accept)
	if err != nil {
		if cached == nil || errors.Is(err, ErrUpstreamNotFound) {
			return nil, err
		}
		h.proxy.Logger.Warn("upstream Swift metadata fetch failed, serving cached copy",
			"url", upstreamURL, "error", err)
		return cached, nil
	}
	doc := &swiftDocument{
		body:        body,
		contentType: contentType,
		link:        strings.Join(headers.Values("Link"), ", "),
		fetchedAt:   time.Now(),
	}
	h.storeDocument(ctx, cacheKey, doc)
	return doc, nil
}

func (h *SwiftHandler) documentsCached() bool {
	return h.proxy.CacheMetadata && h.proxy.DB != nil && h.proxy.Storage != nil
}

// loadDocument returns the cached document under cacheKey, or nil.
func (h *SwiftHandler) loadDocument(ctx context.Context, cacheKey string) *swiftDocument {
	if !h.documentsCached() {
		return nil
	}
	entry := h.proxy.storedMetadataEntry("swift", cacheKey, true)
	if entry == nil {
		return nil
	}
	reader, err := h.proxy.openMetadataEntry(ctx, "swift", cacheKey, entry)
	if err != nil {
		return nil
	}
	defer func() { _ = reader.Close() }()
	body, err := h.proxy.ReadMetadata(reader)
	if err != nil {
		return nil
	}
	h.proxy.recordMetadataRead(entry)
	return &swiftDocument{
		body:        body,
		contentType: entry.ContentType.String,
		link:        entry.Link.String,
		fetchedAt:   entry.FetchedAt.Time,
	}
}

// storeDocument caches doc under cacheKey. A failure is logged; the
// document is still served.
func (h *SwiftHandler) storeDocument(ctx context.Context, cacheKey string, doc *swiftDocument) {
	if !h.documentsCached() {
		return
	}
	storagePath := metadataStoragePath("swift", cacheKey)
	size, hash, err := h.proxy.Storage.Store(ctx, storagePath, bytes.NewReader(doc.body))
	if err == nil {
		err = h.proxy.DB.UpsertMetadataCache(&database.MetadataCacheEntry{
			Ecosystem:     "swift",
			Name:          cacheKey,
			StoragePath:   storagePath,
			Link:          sql.NullString{String: doc.link, Valid: doc.link != ""},
			ContentType:   sql.NullString{String: doc.contentType, Valid: doc.contentType != ""},
			ContentDigest: sql.NullString{String: "sha256:" + hash, Valid: hash != ""},
			Size:          sql.NullInt64{Int64: size, Valid: true},
			FetchedAt:     sql.NullTime{Time: doc.fetchedAt, Valid: true},
		})
	}
	if err != nil {
		h.proxy.Logger.Warn("failed to cache Swift metadata", "key", cacheKey, "error", err)
	}
}

func swiftReleasesCacheKey(scope, name, rawQuery string) string {
	return swiftMetadataCacheKey("releases", scope, name, rawQuery)
}

// swiftManifestCacheKey keys a manifest by its swift-version as a tools
// version, so the 5.8 of a manifest's alternate link and the 5.8.0 a client
// requests for it share a key.
func swiftManifestCacheKey(scope, name, version, rawQuery string) string {
	if query, err := url.ParseQuery(rawQuery); err == nil {
		if tools := query.Get("swift-version"); tools != "" {
			for strings.Count(tools, ".") > 0 && strings.HasSuffix(tools, ".0") {
				tools = strings.TrimSuffix(tools, ".0")
			}
			query.Set("swift-version", tools)
		}
		rawQuery = query.Encode()
	}
	return swiftMetadataCacheKey("manifest", scope, name, version, rawQuery)
}
