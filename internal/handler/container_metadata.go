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
