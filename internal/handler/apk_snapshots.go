package handler

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
)

// apkSnapshotDateLayout is the date format of a snapshot path segment.
const apkSnapshotDateLayout = "2006-01-02"

// WithSnapshots keeps earlier versions of the indexes this handler serves and
// makes them available under /apk/{name}@{YYYY-MM-DD}/, as signed upstream
// bytes that apk verifies as usual. keep is how long a version is kept after
// it was replaced; 0 disables snapshots.
func (h *APKHandler) WithSnapshots(keep time.Duration) *APKHandler {
	h.snapshots = keep
	return h
}

// snapshotsEnabled reports whether snapshots are recorded and served. They
// need the metadata cache, which stores the documents they are copied from.
func (h *APKHandler) snapshotsEnabled() bool {
	return h.snapshots > 0 && h.proxy.CacheMetadata && h.proxy.DB != nil && h.proxy.Storage != nil
}

// resolveRepository maps the first path segment to a configured repository.
// A segment of the form {name}@{YYYY-MM-DD} names a snapshot of repository
// name for that day, when snapshots are enabled and no repository is
// configured under the full segment. ok is false for anything else.
func (h *APKHandler) resolveRepository(segment string) (repository, upstreamURL string, day time.Time, snapshot, ok bool) {
	if upstreamURL, found := h.repositories[segment]; found {
		return segment, upstreamURL, time.Time{}, false, true
	}
	if !h.snapshotsEnabled() {
		return "", "", time.Time{}, false, false
	}
	at := strings.LastIndexByte(segment, '@')
	if at < 0 {
		return "", "", time.Time{}, false, false
	}
	upstreamURL, found := h.repositories[segment[:at]]
	if !found {
		return "", "", time.Time{}, false, false
	}
	day, err := time.Parse(apkSnapshotDateLayout, segment[at+1:])
	if err != nil {
		return "", "", time.Time{}, false, false
	}
	return segment[:at], upstreamURL, day, true, true
}

// serveSnapshot answers a request for an index or signature under a snapshot
// path with the version that was current at the end of day. A day for which
// no version was stored, or one that has not begun yet, is 404.
func (h *APKHandler) serveSnapshot(w http.ResponseWriter, r *http.Request, repository, upstreamURL, path string, day time.Time) {
	end := day.AddDate(0, 0, 1)
	if day.After(time.Now()) {
		http.NotFound(w, r)
		return
	}
	snap, err := h.proxy.DB.MetadataSnapshotAt(apkEcosystem, h.metadataCacheKey(repository, upstreamURL, path), end)
	if err != nil {
		h.proxy.Logger.Error("apk snapshot lookup failed", "repository", repository, "path", path, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if snap == nil {
		http.NotFound(w, r)
		return
	}

	if writeMetadataValidators(w, r, cachedMeta{etag: `"` + snap.ContentDigest + `"`, lastModified: snap.ValidFrom()}) {
		return
	}
	body, err := h.proxy.Storage.Open(r.Context(), snap.StoragePath)
	if err != nil {
		h.proxy.Logger.Error("apk snapshot unreadable", "repository", repository, "path", path,
			"storage_path", snap.StoragePath, "error", err)
		http.Error(w, "snapshot unavailable", http.StatusInternalServerError)
		return
	}
	defer func() { _ = body.Close() }()

	if snap.ContentType.String != "" {
		w.Header().Set(headerContentType, snap.ContentType.String)
	}
	w.Header().Set(headerContentLength, strconv.FormatInt(snap.Size, 10))
	if snap.ContentEncoding.String != "" {
		w.Header().Set(headerContentEncoding, snap.ContentEncoding.String)
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, body)
	}
}

// serveMetadataWithSnapshot is ProxyCached for an index or signature that
// keeps each version it serves as a snapshot. A version is recorded from the
// document the request serves, not by reading the cache again afterwards: a
// concurrent refresh can replace the cached copy at any time, and the version
// it replaced would then never be recorded.
func (h *APKHandler) serveMetadataWithSnapshot(w http.ResponseWriter, r *http.Request, upstream, cacheKey string) {
	p := h.proxy
	ctx := context.WithoutCancel(r.Context())

	// A cached copy within its TTL was recorded by the request that fetched
	// it; this only catches one stored before snapshots were enabled, or
	// whose recording failed.
	if entry := p.freshMetadataEntry(apkEcosystem, cacheKey); entry != nil {
		seen := time.Now()
		if p.serveCachedMetadata(w, r, apkEcosystem, cacheKey) {
			h.recordStoredSnapshot(ctx, cacheKey, entry, seen)
			return
		}
	}

	body, contentType, contentEncoding, err := p.fetchOrCacheMetadata(r.Context(), apkEcosystem, cacheKey, upstream, "identity", nil, "*/*")
	if err != nil {
		p.writeMetadataFetchError(w, err)
		return
	}
	// Record before responding, so the version is kept by the time a
	// client has it.
	h.recordServedSnapshot(ctx, cacheKey, body, contentType, contentEncoding, time.Now())
	p.writeMetadataCachedResponseWithEncoding(w, r, apkEcosystem, cacheKey, body, contentType, contentEncoding)
}

// recordServedSnapshot keeps body, the document a request is serving, as the
// version current from observed, unless the version current then already has
// its bytes.
func (h *APKHandler) recordServedSnapshot(ctx context.Context, cacheKey string, body []byte, contentType, contentEncoding string, observed time.Time) {
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if !h.snapshotNeeded(cacheKey, digest, observed) {
		return
	}
	storagePath := apkSnapshotStoragePath(cacheKey, digest, observed)
	if _, _, err := h.proxy.Storage.Store(ctx, storagePath, bytes.NewReader(body)); err != nil {
		h.proxy.Logger.Warn("failed to store apk snapshot", "path", storagePath, "error", err)
		h.discardSnapshotBlob(ctx, storagePath)
		return
	}
	h.addSnapshot(ctx, &database.MetadataSnapshot{
		Ecosystem:       apkEcosystem,
		Name:            cacheKey,
		StoragePath:     storagePath,
		ContentType:     sql.NullString{String: contentType, Valid: contentType != ""},
		ContentEncoding: sql.NullString{String: contentEncoding, Valid: contentEncoding != ""},
		ContentDigest:   digest,
		Size:            int64(len(body)),
		ValidFromMS:     observed.UnixMilli(),
	})
}

// recordStoredSnapshot keeps the cached document entry describes as the
// version current from observed, unless the version current then already
// has its digest.
func (h *APKHandler) recordStoredSnapshot(ctx context.Context, cacheKey string, entry *database.MetadataCacheEntry, observed time.Time) {
	if !entry.ContentDigest.Valid || !h.snapshotNeeded(cacheKey, entry.ContentDigest.String, observed) {
		return
	}
	h.storeSnapshot(ctx, cacheKey, entry, observed)
}

// snapshotNeeded reports whether a version with digest seen at observed
// differs from the version current then. It is only a shortcut that saves
// copying the document: AddMetadataSnapshot makes the same check.
func (h *APKHandler) snapshotNeeded(cacheKey, digest string, observed time.Time) bool {
	current, err := h.proxy.DB.MetadataSnapshotAt(apkEcosystem, cacheKey, observed.Add(time.Millisecond))
	return err == nil && (current == nil || current.ContentDigest != digest)
}

// storeSnapshot records the cached document entry as the version current
// from validFrom. Concurrent requests can both get here for the same
// version; each copies to a path of its own, so the one whose row is not
// added only deletes its own copy.
func (h *APKHandler) storeSnapshot(ctx context.Context, cacheKey string, entry *database.MetadataCacheEntry, validFrom time.Time) {
	digest := entry.ContentDigest.String
	storagePath := apkSnapshotStoragePath(cacheKey, digest, validFrom)
	size, ok := h.copySnapshotBlob(ctx, entry.StoragePath, storagePath, digest)
	if !ok {
		return
	}
	h.addSnapshot(ctx, &database.MetadataSnapshot{
		Ecosystem:       apkEcosystem,
		Name:            cacheKey,
		StoragePath:     storagePath,
		ContentType:     entry.ContentType,
		ContentEncoding: entry.ContentEncoding,
		ContentDigest:   digest,
		Size:            size,
		ValidFromMS:     validFrom.UnixMilli(),
	})
}

// addSnapshot adds the row for a snapshot whose blob is stored, and deletes
// the blob when the row isn't added, as when another request recorded the
// same version first.
func (h *APKHandler) addSnapshot(ctx context.Context, snap *database.MetadataSnapshot) {
	added, err := h.proxy.DB.AddMetadataSnapshot(snap)
	if err != nil {
		h.proxy.Logger.Warn("failed to record apk snapshot", "key", snap.Name, "error", err)
	}
	if err != nil || !added {
		h.discardSnapshotBlob(ctx, snap.StoragePath)
	}
}

// copySnapshotBlob copies the cached document at from to to and reports its
// size, provided the copied bytes still have digest. A concurrent fetch can
// replace the cached document after its row was read; that version is
// recorded by the request that stored it.
func (h *APKHandler) copySnapshotBlob(ctx context.Context, from, to, digest string) (int64, bool) {
	src, err := h.proxy.Storage.Open(ctx, from)
	if err != nil {
		return 0, false
	}
	defer func() { _ = src.Close() }()
	size, hash, err := h.proxy.Storage.Store(ctx, to, src)
	if err != nil {
		h.proxy.Logger.Warn("failed to store apk snapshot", "path", to, "error", err)
		return 0, false
	}
	if "sha256:"+hash != digest {
		h.discardSnapshotBlob(ctx, to)
		return 0, false
	}
	return size, true
}

func (h *APKHandler) discardSnapshotBlob(ctx context.Context, path string) {
	if err := h.proxy.Storage.Delete(ctx, path); err != nil {
		h.proxy.Logger.Warn("failed to discard apk snapshot blob", "path", path, "error", err)
	}
}

// apkSnapshotStoragePath returns a new path to store one kept version of the
// document cached under cacheKey at. The time it became current keeps a
// version that returns after an upgrade was rolled back apart from its
// earlier copy, and a random suffix keeps two requests recording the same
// version in the same millisecond apart.
func apkSnapshotStoragePath(cacheKey, digest string, validFrom time.Time) string {
	return "_snapshots/" + apkEcosystem + "/" + cacheKey + "/" +
		strconv.FormatInt(validFrom.UnixMilli(), 10) + "-" + strings.TrimPrefix(digest, "sha256:") +
		"-" + rand.Text()
}
