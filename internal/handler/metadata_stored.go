package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/metrics"
)

// A metadata document can be rewritten from storage instead of from memory:
// storedMetadataDocument streams it from upstream into storage, and the
// rewrite reads it back as often as it needs. Nothing holds the whole
// document, so its size is not limited by metadata_max_size or by memory.

// errMetadataNotStored is returned when metadata is not cached in storage, so
// a document has to be read into memory.
var errMetadataNotStored = errors.New("metadata is not cached in storage")

// errStoredMetadataChanged is returned when a stored document no longer has
// the digest its cache row records: a fetch replaced it while it was read.
var errStoredMetadataChanged = errors.New("stored metadata changed while it was read")

// storedMetadataDocument returns the cache row of a metadata document stored
// uncompressed with its digest: the stored copy while it is within its TTL,
// or else the document fetched from upstream and stored, or the stored copy
// when upstream fails. The document is streamed from upstream into storage
// without being held in memory, so a fetch takes no metadata_max_size limit.
// Concurrent calls for one document share a fetch, and instances sharing the
// database take turns as for other metadata.
//
// It returns errMetadataNotStored when the proxy doesn't cache metadata.
func (p *Proxy) storedMetadataDocument(ctx context.Context, ecosystem, cacheKey, upstreamURL, accept string) (*database.MetadataCacheEntry, error) {
	if !p.CacheMetadata || p.DB == nil || p.Storage == nil || p.MetadataTTL <= 0 {
		return nil, errMetadataNotStored
	}
	if containsPathTraversal(cacheKey) {
		return nil, fmt.Errorf("invalid cache key: %q", cacheKey)
	}
	if entry, fresh := p.storedMetadataEntryState(ctx, ecosystem, cacheKey); fresh {
		metrics.RecordMetadataCacheHit(ecosystem, int(entry.Size.Int64))
		p.recordMetadataRead(entry)
		return entry, nil
	}
	p.recordMetadataCacheMiss(ecosystem)

	key := metadataCoalesceKey(ecosystem, cacheKey, upstreamURL, accept, "", false) + "\x00stored"
	res, _ := p.coalesceMetadata(ctx, key, func(fetchCtx context.Context) metadataResult {
		entry, fresh := p.storedMetadataEntryState(fetchCtx, ecosystem, cacheKey)
		if fresh {
			return metadataResult{entry: entry}
		}
		release, outcome, err := p.awaitFetchLease(fetchCtx, metrics.CacheKindMetadata, key, func() bool {
			entry, fresh = p.storedMetadataEntryState(fetchCtx, ecosystem, cacheKey)
			return fresh
		})
		if err != nil {
			return metadataResult{err: err}
		}
		switch outcome {
		case leaseStored:
			return metadataResult{entry: entry}
		case leaseFailed:
			// Serve the stored copy, as the holder did, rather than trying
			// the failing upstream again.
			p.reloadUpstreamHolds()
			if usableStoredMetadata(entry) {
				return p.staleStoredMetadata(ecosystem, cacheKey, entry, errFetchFailedElsewhere)
			}
		case leaseFetch:
		}
		res := metadataResult{err: errSharedFetchAbandoned}
		defer func() { release(res.err != nil || res.stale) }()
		res = p.fetchStoredMetadata(fetchCtx, ecosystem, cacheKey, upstreamURL, accept, entry)
		return res
	})
	return res.entry, res.err
}

// storedMetadataEntryState reads the cache row of a document and reports
// whether it is within its TTL and usable by storedMetadataDocument.
func (p *Proxy) storedMetadataEntryState(ctx context.Context, ecosystem, cacheKey string) (*database.MetadataCacheEntry, bool) {
	entry, _ := p.DB.GetMetadataCache(ecosystem, cacheKey)
	entry = p.migrateLegacyMetadataEntry(ctx, ecosystem, cacheKey, entry)
	fresh := usableStoredMetadata(entry) && entry.FetchedAt.Valid && time.Since(entry.FetchedAt.Time) < p.MetadataTTL
	return entry, fresh
}

// usableStoredMetadata reports whether entry describes a document stored
// uncompressed with its digest, which a rewrite can read back and check.
func usableStoredMetadata(entry *database.MetadataCacheEntry) bool {
	return entry != nil && entry.ContentDigest.Valid && entry.ContentEncoding.String == ""
}

// fetchStoredMetadata fetches a document from upstream into storage,
// revalidating entry when there is one, and falls back to entry when
// upstream fails.
func (p *Proxy) fetchStoredMetadata(ctx context.Context, ecosystem, cacheKey, upstreamURL, accept string, entry *database.MetadataCacheEntry) metadataResult {
	revalidate := entry
	if !usableStoredMetadata(entry) {
		revalidate = nil
	}
	stored, err := p.storeUpstreamMetadata(ctx, ecosystem, cacheKey, upstreamURL, accept, revalidate)
	if errors.Is(err, errStale304) {
		stored, err = p.storeUpstreamMetadata(ctx, ecosystem, cacheKey, upstreamURL, accept, nil)
	}
	if err == nil {
		return metadataResult{entry: stored}
	}
	if !usableStoredMetadata(entry) || errors.Is(err, ErrUpstreamNotFound) {
		return metadataResult{err: err}
	}
	return p.staleStoredMetadata(ecosystem, cacheKey, entry, err)
}

// staleStoredMetadata falls back to the stored document entry describes after
// the upstream fetch failed with err.
func (p *Proxy) staleStoredMetadata(ecosystem, cacheKey string, entry *database.MetadataCacheEntry, err error) metadataResult {
	p.Logger.Warn("upstream metadata fetch failed, serving cached copy",
		"ecosystem", ecosystem, "key", cacheKey, "error", err)
	entry = p.currentMetadataEntry(ecosystem, cacheKey, entry)
	if !usableStoredMetadata(entry) {
		return metadataResult{err: fmt.Errorf("upstream failed and no usable cached metadata: %w", err)}
	}
	return metadataResult{entry: entry, stale: true}
}

// storeUpstreamMetadata fetches a document and streams it into storage, and
// returns its new cache row. On a 304 for entry it only moves the row's
// fetched_at on; errStale304 means the row was replaced meanwhile and the
// caller should fetch without validators.
func (p *Proxy) storeUpstreamMetadata(ctx context.Context, ecosystem, cacheKey, upstreamURL, accept string, entry *database.MetadataCacheEntry) (*database.MetadataCacheEntry, error) {
	resp, err := p.requestUpstreamMetadata(ctx, upstreamURL, entry, accept, "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotModified && entry != nil {
		refreshed, err := p.DB.RefreshMetadataCache(ecosystem, cacheKey, entry.ContentDigest.String, time.Now())
		if err != nil || !refreshed {
			return nil, errStale304
		}
		return p.currentMetadataEntry(ecosystem, cacheKey, entry), nil
	}
	if err := upstreamMetadataStatus(resp); err != nil {
		return nil, err
	}

	meta := &upstreamMetadata{
		contentType: resp.Header.Get(headerContentType),
		etag:        resp.Header.Get(headerETag),
	}
	if meta.contentType == "" {
		meta.contentType = contentTypeJSON
	}
	if lm := resp.Header.Get(headerLastModified); lm != "" {
		meta.lastModified, _ = http.ParseTime(lm)
	}
	// Go decompresses the body, since no Accept-Encoding was set, so it is
	// stored uncompressed.
	if _, err := p.storeMetadataBlob(ctx, ecosystem, cacheKey, metadataStoragePath(ecosystem, cacheKey), resp.Body, meta); err != nil {
		return nil, err
	}
	stored, err := p.DB.GetMetadataCache(ecosystem, cacheKey)
	if err != nil || !usableStoredMetadata(stored) {
		return nil, fmt.Errorf("reading the stored metadata row: %w", errors.Join(err, errMetadataNotStored))
	}
	return stored, nil
}

// openStoredMetadata opens the document entry describes. Reading it to the
// end fails with errStoredMetadataChanged when its bytes don't have the
// digest entry records.
func (p *Proxy) openStoredMetadata(ctx context.Context, entry *database.MetadataCacheEntry) (io.ReadCloser, error) {
	r, err := p.Storage.Open(ctx, entry.StoragePath)
	if err != nil {
		return nil, err
	}
	return &digestReader{r: r, h: sha256.New(), want: entry.ContentDigest.String}, nil
}

// digestReader checks at the end of its reader that what it read has the
// digest want, in the form the metadata cache records it.
type digestReader struct {
	r    io.ReadCloser
	h    hash.Hash
	want string
}

func (d *digestReader) Read(b []byte) (int, error) {
	n, err := d.r.Read(b)
	d.h.Write(b[:n])
	if errors.Is(err, io.EOF) && "sha256:"+hex.EncodeToString(d.h.Sum(nil)) != d.want {
		return n, errStoredMetadataChanged
	}
	return n, err
}

func (d *digestReader) Close() error { return d.r.Close() }
