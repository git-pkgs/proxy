package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/metrics"
)

// serveCachedMetadata answers a ProxyCached request from a fresh metadata
// cache row by streaming the stored document, rather than reading it into
// memory first, so concurrent hits on a large document don't each hold a copy
// of it. The bytes are checked against the row's digest as they are sent, and
// a mismatch aborts the response and drops the row, the way a corrupt cached
// artifact is handled.
//
// It reports false, having written nothing, when the request should take the
// buffered path instead: no fresh row, a row without a digest or size, a
// document over the metadata size limit, or one that can't be opened.
func (p *Proxy) serveCachedMetadata(w http.ResponseWriter, r *http.Request, ecosystem, cacheKey string) bool {
	return p.serveStoredMetadata(w, r, ecosystem, cacheKey, false)
}

// serveStoredMetadata is serveCachedMetadata. With immutable set the row is
// served whatever its age and never marked stale, for documents that can't
// change upstream (see proxyImmutable).
func (p *Proxy) serveStoredMetadata(w http.ResponseWriter, r *http.Request, ecosystem, cacheKey string, immutable bool) bool {
	entry := p.storedMetadataEntry(ecosystem, cacheKey, immutable)
	if entry == nil {
		return false
	}
	return p.serveMetadataEntry(w, r, ecosystem, cacheKey, entry, immutable)
}

// serveMetadataEntry is serveStoredMetadata for entry, a row
// storedMetadataEntry or streamableMetadata accepted.
func (p *Proxy) serveMetadataEntry(w http.ResponseWriter, r *http.Request, ecosystem, cacheKey string, entry *database.MetadataCacheEntry, immutable bool) bool {
	body, err := p.openMetadataEntry(r.Context(), ecosystem, cacheKey, entry)
	if err != nil {
		return false
	}
	defer func() { _ = body.Close() }()

	p.recordStreamedMetadataHit(ecosystem, entry)

	cm := p.cachedMetaFor(entry)
	if immutable {
		cm.stale = false
	}
	if writeMetadataValidators(w, r, cm) {
		return true
	}
	contentType := contentTypeJSON
	if entry.ContentType.Valid {
		contentType = entry.ContentType.String
	}
	w.Header().Set(headerContentType, contentType)
	w.Header().Set(headerContentLength, strconv.FormatInt(entry.Size.Int64, 10))
	if entry.ContentEncoding.String != "" {
		w.Header().Set(headerContentEncoding, entry.ContentEncoding.String)
	}
	markStale(w, cm.stale)
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		copyArtifactBody(w, body, entry.Size.Int64, false)
	}
	return true
}

// freshMetadataEntry returns the metadata cache row for ecosystem and
// cacheKey when its document can be streamed: the row is within its TTL and
// records a sha256 digest and a size within the metadata size limit. It
// returns nil otherwise.
func (p *Proxy) freshMetadataEntry(ecosystem, cacheKey string) *database.MetadataCacheEntry {
	return p.storedMetadataEntry(ecosystem, cacheKey, false)
}

// storedMetadataEntry is freshMetadataEntry, ignoring the row's age when
// immutable is set.
func (p *Proxy) storedMetadataEntry(ecosystem, cacheKey string, immutable bool) *database.MetadataCacheEntry {
	if p.DB == nil || p.Storage == nil || (!immutable && p.MetadataTTL <= 0) || containsPathTraversal(cacheKey) {
		return nil
	}
	entry, err := p.DB.GetMetadataCache(ecosystem, cacheKey)
	if err != nil || entry == nil || !entry.FetchedAt.Valid {
		return nil
	}
	if !immutable && time.Since(entry.FetchedAt.Time) >= p.MetadataTTL {
		return nil
	}
	if !p.streamableMetadata(entry) {
		return nil
	}
	return entry
}

// streamableMetadata reports whether entry records a sha256 digest and a
// size within the metadata size limit, so its document can be streamed.
func (p *Proxy) streamableMetadata(entry *database.MetadataCacheEntry) bool {
	limit := p.MetadataMaxSize
	if limit <= 0 {
		limit = defaultMetadataMaxSize
	}
	return entry.ContentDigest.Valid && strings.HasPrefix(entry.ContentDigest.String, "sha256:") && entry.Size.Valid && entry.Size.Int64 <= limit
}

// openMetadataEntry opens the stored document of a row freshMetadataEntry
// returned. Its bytes are checked against the row's digest as they are read:
// a mismatch fails the read at the end and drops the row, so the next
// request fetches the document again.
func (p *Proxy) openMetadataEntry(ctx context.Context, ecosystem, cacheKey string, entry *database.MetadataCacheEntry) (io.ReadCloser, error) {
	checks, err := newIntegrityChecks(strings.TrimPrefix(entry.ContentDigest.String, "sha256:"), "")
	if err != nil {
		return nil, err
	}
	stored, err := p.Storage.Open(ctx, entry.StoragePath)
	if err != nil {
		return nil, err
	}
	body, err := checks.wrapFailOnMismatch(stored, func(reason string) {
		p.Logger.Error("cached metadata failed integrity check",
			"ecosystem", ecosystem, "key", cacheKey, "path", entry.StoragePath, "reason", reason)
		metrics.RecordIntegrityFailure(ecosystem, metrics.FailureDetail{
			Path: entry.StoragePath, Err: errors.New(reason)})
		if err := p.DB.DiscardMetadataCache(ecosystem, cacheKey, entry.StoragePath, entry.ContentDigest.String); err != nil {
			p.Logger.Warn("failed to clear corrupt metadata from cache", "error", err)
		}
	})
	if err != nil {
		_ = stored.Close()
		return nil, err
	}
	return body, nil
}

// recordStreamedMetadataHit counts a hit on a row served by streaming.
func (p *Proxy) recordStreamedMetadataHit(ecosystem string, entry *database.MetadataCacheEntry) {
	p.recordMetadataRead(entry)
	metrics.RecordMetadataCacheHit(ecosystem, int(entry.Size.Int64))
}
