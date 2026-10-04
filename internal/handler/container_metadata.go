package handler

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
)

func (h *ContainerHandler) storeContainerMetadata(ctx context.Context, ecosystem, cacheKey string, body []byte, etag, link, contentType, contentDigest string, lastModified, fetchedAt time.Time) (int64, error) {
	if h.proxy.DB == nil || h.proxy.Storage == nil {
		return int64(len(body)), nil
	}

	storagePath := metadataStoragePath(ecosystem, cacheKey)
	size, _, err := h.proxy.Storage.Store(ctx, storagePath, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("storing metadata: %w", err)
	}
	err = h.proxy.DB.UpsertMetadataCache(&database.MetadataCacheEntry{
		Ecosystem:     ecosystem,
		Name:          cacheKey,
		StoragePath:   storagePath,
		ETag:          sql.NullString{String: etag, Valid: etag != ""},
		Link:          sql.NullString{String: link, Valid: link != ""},
		ContentType:   sql.NullString{String: contentType, Valid: contentType != ""},
		ContentDigest: sql.NullString{String: contentDigest, Valid: contentDigest != ""},
		Size:          sql.NullInt64{Int64: size, Valid: true},
		LastModified:  sql.NullTime{Time: lastModified, Valid: !lastModified.IsZero()},
		FetchedAt:     sql.NullTime{Time: fetchedAt, Valid: !fetchedAt.IsZero()},
	})
	if err != nil {
		return 0, err
	}
	return size, nil
}

// loadContainerMetadata returns a cached metadata row and its body, or nil
// when nothing usable is cached.
func (h *ContainerHandler) loadContainerMetadata(ctx context.Context, ecosystem, cacheKey string) (*database.MetadataCacheEntry, []byte, error) {
	if h.proxy.DB == nil || h.proxy.Storage == nil {
		return nil, nil, nil
	}
	entry, err := h.proxy.DB.GetMetadataCache(ecosystem, cacheKey)
	if err != nil || entry == nil {
		return nil, nil, err
	}
	reader, err := h.proxy.Storage.Open(ctx, entry.StoragePath)
	if err != nil {
		return nil, nil, nil
	}
	defer func() { _ = reader.Close() }()
	body, err := h.proxy.ReadMetadata(reader)
	if err != nil {
		return nil, nil, err
	}
	return entry, body, nil
}
