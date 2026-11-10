package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"

	"github.com/git-pkgs/proxy/internal/metrics"
)

// Some metadata documents never change once published: a Go module's .mod
// and .info for a canonical version, or an RPM repodata file named after its
// own checksum. Revalidating them every metadata TTL, or fetching them again
// on every request without cache_metadata, only costs upstream requests. They
// are kept in the metadata cache like other metadata, but served whatever
// their age and stored whether or not cache_metadata is set, the way
// artifacts are.

// errImmutableDigest is returned when a stored immutable document doesn't
// match the digest recorded for it.
var errImmutableDigest = errors.New("stored metadata does not match its digest")

// proxyImmutable answers a request for an immutable metadata document: from
// the metadata cache when it is stored, otherwise fetched once and stored. A
// document over the metadata size limit is streamed from upstream instead,
// uncached.
func (p *Proxy) proxyImmutable(w http.ResponseWriter, r *http.Request, upstreamURL, ecosystem, cacheKey, accept string) {
	if p.serveStoredMetadata(w, r, ecosystem, cacheKey, true) {
		return
	}
	res := p.fetchImmutableMetadata(r.Context(), ecosystem, cacheKey, upstreamURL, accept)
	if errors.Is(res.err, ErrMetadataTooLarge) {
		p.proxyMetadataStream(w, r, upstreamURL, "identity", accept)
		return
	}
	if res.err != nil {
		p.writeMetadataFetchError(w, res.err)
		return
	}
	res.stale = false
	writeMetadataResult(w, r, res)
}

// fetchImmutableMetadata returns an immutable metadata document from the
// metadata cache whatever its age, or fetches and stores it. Upstream's bytes
// are kept as sent (Accept-Encoding: identity). Without a database or storage
// it falls back to the TTL lookup.
func (p *Proxy) fetchImmutableMetadata(ctx context.Context, ecosystem, cacheKey, upstreamURL, accept string) metadataResult {
	if p.DB == nil || p.Storage == nil {
		return p.lookupMetadata(ctx, ecosystem, cacheKey, upstreamURL, accept, "identity", nil)
	}
	if containsPathTraversal(cacheKey) {
		return metadataResult{err: fmt.Errorf("invalid cache key: %q", cacheKey)}
	}
	if hit := p.storedImmutable(ctx, ecosystem, cacheKey); hit != nil {
		metrics.RecordMetadataCacheHit(ecosystem, len(hit.body))
		return *hit
	}
	metrics.RecordMetadataCacheMiss(ecosystem)

	// Kept apart from TTL lookups of the same key, which don't store the
	// document without cache_metadata.
	key := metadataCoalesceKey(ecosystem, cacheKey, upstreamURL, accept, "identity", false) + "\x00immutable"
	res, _ := p.coalesceMetadata(ctx, key, func(fetchCtx context.Context) metadataResult {
		if hit := p.storedImmutable(fetchCtx, ecosystem, cacheKey); hit != nil {
			return *hit
		}
		meta, err := p.fetchUpstreamMetadata(fetchCtx, upstreamURL, nil, accept, "identity")
		if err != nil {
			return metadataResult{err: err}
		}
		digest := p.cacheMetadataBlob(fetchCtx, ecosystem, cacheKey, metadataStoragePath(ecosystem, cacheKey), meta)
		return fetchedResult(meta, digest)
	})
	return res
}

// storedImmutable reads an immutable document from the metadata cache, or
// returns nil. Since the copy is never revalidated, it is checked against the
// digest recorded when it was stored; a row without one, or whose bytes don't
// match, is a miss, and the fetch that follows replaces it.
func (p *Proxy) storedImmutable(ctx context.Context, ecosystem, cacheKey string) *metadataResult {
	entry, err := p.DB.GetMetadataCache(ecosystem, cacheKey)
	if err != nil || entry == nil {
		return nil
	}
	entry = p.migrateLegacyMetadataEntry(ctx, ecosystem, cacheKey, entry)
	if entry == nil || !entry.ContentDigest.Valid {
		return nil
	}
	want := entry.ContentDigest.String
	body, contentType, err := p.readCachedMetadata(ctx, entry, func(b []byte) error {
		sum := sha256.Sum256(b)
		if "sha256:"+hex.EncodeToString(sum[:]) != want {
			return errImmutableDigest
		}
		return nil
	})
	if errors.Is(err, errImmutableDigest) {
		p.Logger.Error("cached metadata failed integrity check",
			"ecosystem", ecosystem, "key", cacheKey, "path", entry.StoragePath)
		metrics.RecordIntegrityFailure(ecosystem, metrics.FailureDetail{Path: entry.StoragePath, Err: err})
		if err := p.DB.DiscardMetadataCache(ecosystem, cacheKey, entry.StoragePath, want); err != nil {
			p.Logger.Warn("failed to clear corrupt metadata from cache", "error", err)
		}
	}
	if err != nil {
		return nil
	}
	res := metadataResult{body: body, contentType: contentType, contentEncoding: entry.ContentEncoding.String}.describedBy(entry)
	return &res
}
