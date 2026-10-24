package handler

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/git-pkgs/proxy/internal/metrics"
)

// serveCachedMetadata answers a ProxyCached request from a fresh metadata
// cache row by streaming the stored document, rather than reading it into
// memory first, so concurrent hits on a large document don't each hold a copy
// of it. The bytes are checked against the row's digest as they are sent, and
// a mismatch aborts the response the way a corrupt cached artifact does.
//
// It reports false, having written nothing, when the request should take the
// buffered path instead: no fresh row, a row without a digest or size, a
// document over the metadata size limit, or one that can't be opened.
func (p *Proxy) serveCachedMetadata(w http.ResponseWriter, r *http.Request, ecosystem, cacheKey string) bool {
	if p.DB == nil || p.Storage == nil || p.MetadataTTL <= 0 || containsPathTraversal(cacheKey) {
		return false
	}
	entry, err := p.DB.GetMetadataCache(ecosystem, cacheKey)
	if err != nil || entry == nil || !entry.FetchedAt.Valid || time.Since(entry.FetchedAt.Time) >= p.MetadataTTL {
		return false
	}
	limit := p.MetadataMaxSize
	if limit <= 0 {
		limit = defaultMetadataMaxSize
	}
	digest, ok := strings.CutPrefix(entry.ContentDigest.String, "sha256:")
	if !entry.ContentDigest.Valid || !ok || !entry.Size.Valid || entry.Size.Int64 > limit {
		return false
	}
	checks, err := newIntegrityChecks(digest, "")
	if err != nil {
		return false
	}
	stored, err := p.Storage.Open(r.Context(), entry.StoragePath)
	if err != nil {
		return false
	}
	body, err := checks.wrapFailOnMismatch(stored, func(reason string) {
		p.Logger.Error("cached metadata failed integrity check",
			"ecosystem", ecosystem, "key", cacheKey, "path", entry.StoragePath, "reason", reason)
	})
	if err != nil {
		_ = stored.Close()
		return false
	}
	defer func() { _ = body.Close() }()

	p.recordMetadataRead(entry)
	metrics.RecordMetadataCacheHit(ecosystem, int(entry.Size.Int64))

	cm := p.cachedMetaFor(entry)
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
