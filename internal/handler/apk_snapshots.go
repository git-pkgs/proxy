package handler

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
)

// Dated snapshots of APK indexes.
//
// Alpine mirrors keep only the current build of each package, so once a
// package is upgraded the index no longer lists the old build and
// `apk add name=old-version` fails. With snapshots enabled, every version of an
// index (and of a detached signature) the proxy stores is kept, and
// /apk/{name}@{YYYY-MM-DD}/... serves the version that was current at the end
// of that day (UTC). The kept bytes are the signed upstream bytes, so apk
// verifies them as usual. Packages under a snapshot path are the same
// requests as under the plain path.

// apkSnapshotDateLayout is the date format of a snapshot path segment.
const apkSnapshotDateLayout = "2006-01-02"

// WithSnapshots keeps earlier versions of the indexes this handler serves and
// makes them available under /apk/{name}@{YYYY-MM-DD}/. keep is how long a
// version is kept after it was replaced; 0 disables snapshots.
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

// recordSnapshot keeps the document now cached under cacheKey as a snapshot
// when it differs from the newest one kept. It runs after the document was
// served, so a failure only means this version is recorded on a later
// request instead.
func (h *APKHandler) recordSnapshot(ctx context.Context, cacheKey string) {
	entry, err := h.proxy.DB.GetMetadataCache(apkEcosystem, cacheKey)
	if err != nil || entry == nil || !entry.ContentDigest.Valid {
		return
	}
	digest := entry.ContentDigest.String
	latest, err := h.proxy.DB.LatestMetadataSnapshot(apkEcosystem, cacheKey)
	if err != nil || (latest != nil && latest.ContentDigest == digest) {
		return
	}

	validFrom := time.Now()
	storagePath := apkSnapshotStoragePath(cacheKey, digest, validFrom)
	size, ok := h.copySnapshotBlob(ctx, entry.StoragePath, storagePath, digest)
	if !ok {
		return
	}
	added, err := h.proxy.DB.AddMetadataSnapshot(&database.MetadataSnapshot{
		Ecosystem:       apkEcosystem,
		Name:            cacheKey,
		StoragePath:     storagePath,
		ContentType:     entry.ContentType,
		ContentEncoding: entry.ContentEncoding,
		ContentDigest:   digest,
		Size:            size,
		ValidFromMS:     validFrom.UnixMilli(),
	})
	if err != nil {
		h.proxy.Logger.Warn("failed to record apk snapshot", "key", cacheKey, "error", err)
	}
	if err != nil || !added {
		// Another request recorded this version first.
		h.discardSnapshotBlob(ctx, storagePath)
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

// apkSnapshotStoragePath is where one kept version of the document cached
// under cacheKey is stored. The time it became current keeps a version that
// returns after an upgrade was rolled back apart from its earlier copy.
func apkSnapshotStoragePath(cacheKey, digest string, validFrom time.Time) string {
	return "_snapshots/" + apkEcosystem + "/" + cacheKey + "/" +
		strconv.FormatInt(validFrom.UnixMilli(), 10) + "-" + strings.TrimPrefix(digest, "sha256:")
}
