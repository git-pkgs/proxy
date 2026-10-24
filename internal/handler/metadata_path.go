package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/git-pkgs/proxy/internal/database"
)

// metadataStoragePath builds the storage path for cached metadata. Cache
// keys are case-sensitive, but on a case-insensitive filesystem (APFS, NTFS)
// keys that differ only in case, such as Go's github.com/Sirupsen/logrus and
// github.com/sirupsen/logrus, would share one file. Keys may also hold names
// Windows reserves or end in a dot. So the path carries a hash of the exact
// key. Keys that already are lowercase SHA-256 hex digests are used as they
// are, so their existing entries stay valid.
func metadataStoragePath(ecosystem, cacheKey string) string {
	if !isSHA256Hex(cacheKey) {
		digest := sha256.Sum256([]byte(cacheKey))
		cacheKey = hex.EncodeToString(digest[:])
	}
	return legacyMetadataStoragePath(ecosystem, cacheKey)
}

// legacyMetadataStoragePath is where metadata was stored before paths were
// hashed: under the cache key itself.
func legacyMetadataStoragePath(ecosystem, cacheKey string) string {
	return "_metadata/" + ecosystem + "/" + cacheKey + "/metadata"
}

func isSHA256Hex(s string) bool {
	if len(s) != 2*sha256.Size {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// migrateLegacyMetadataEntry handles a cache row whose blob is still at its
// legacy, unhashed path, and returns the row to use, or nil when there is
// none. On a case-insensitive filesystem keys differing only in case shared
// that blob, so it may hold another key's document. The blob is hashed and
// compared with the row's digest, once per row:
//
//   - When it matches, the blob is this key's document. It is copied to the
//     hashed path and the row moved there, so it keeps being served, as a
//     stale fallback too. If the copy fails the row is served from where it
//     is and checked again next time.
//   - When it does not, or the row has no digest to check against, the row
//     is pointed at the hashed path with its digest and validators cleared,
//     so the document is fetched again and the blob never served.
//   - When the blob cannot be read, nothing changes and nil is returned.
//
// Once no row's current path is a legacy blob of this key or a case variant
// of it, those blobs are queued for deletion.
func (p *Proxy) migrateLegacyMetadataEntry(ctx context.Context, ecosystem, cacheKey string, entry *database.MetadataCacheEntry) *database.MetadataCacheEntry {
	if entry == nil || p.Storage == nil {
		return entry
	}
	legacy := legacyMetadataStoragePath(ecosystem, cacheKey)
	hashed := metadataStoragePath(ecosystem, cacheKey)
	if entry.StoragePath != legacy || legacy == hashed {
		return entry
	}
	var matches bool
	if entry.ContentDigest.Valid {
		digest, err := p.storedDigest(ctx, legacy)
		if err != nil {
			p.Logger.Warn("failed to read legacy metadata blob", "ecosystem", ecosystem, "key", cacheKey, "error", err)
			return nil
		}
		matches = digest == entry.ContentDigest.String
	}
	if !matches {
		if err := p.DB.ForgetMetadataDocument(ecosystem, cacheKey, legacy, hashed); err != nil {
			p.Logger.Warn("failed to drop legacy metadata row", "ecosystem", ecosystem, "key", cacheKey, "error", err)
			return nil
		}
		p.queueLegacyMetadataBlobs(ecosystem, cacheKey)
		return nil
	}
	if !p.moveLegacyMetadataBlob(ctx, entry, legacy, hashed) {
		return entry
	}
	p.queueLegacyMetadataBlobs(ecosystem, cacheKey)
	moved := *entry
	moved.StoragePath = hashed
	return &moved
}

// storedDigest hashes the blob at path as cacheMetadataBlob records it.
func (p *Proxy) storedDigest(ctx context.Context, path string) (string, error) {
	r, err := p.Storage.Open(ctx, path)
	if err != nil {
		return "", err
	}
	defer func() { _ = r.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// moveLegacyMetadataBlob copies entry's verified blob from legacy to hashed
// and points the row there, reporting whether it did.
func (p *Proxy) moveLegacyMetadataBlob(ctx context.Context, entry *database.MetadataCacheEntry, legacy, hashed string) bool {
	logFailure := func(err error) {
		p.Logger.Warn("failed to move legacy metadata blob", "ecosystem", entry.Ecosystem, "key", entry.Name, "error", err)
	}
	r, err := p.Storage.Open(ctx, legacy)
	if err != nil {
		logFailure(err)
		return false
	}
	defer func() { _ = r.Close() }()
	_, hash, err := p.Storage.Store(ctx, hashed, r)
	if err != nil {
		logFailure(err)
		return false
	}
	if "sha256:"+hash != entry.ContentDigest.String {
		logFailure(fmt.Errorf("blob changed while being copied"))
		return false
	}
	moved, err := p.DB.MoveMetadataCache(entry.Ecosystem, entry.Name, legacy, hashed, entry.ContentDigest.String)
	if err != nil {
		logFailure(err)
		return false
	}
	return moved
}

// queueLegacyMetadataBlobs queues the legacy blobs of cacheKey and its case
// variants for deletion, unless a row still reads one of them: on a
// case-insensitive filesystem a variant still at its legacy path is reading
// the same file. The last variant to move off its legacy path queues them
// all, so on a case-sensitive filesystem none is left behind. Reclaim also
// skips a path a row points at again.
func (p *Proxy) queueLegacyMetadataBlobs(ecosystem, cacheKey string) {
	rows, err := p.DB.MetadataCaseVariants(ecosystem, cacheKey)
	if err != nil {
		p.Logger.Warn("failed to look up metadata case variants", "ecosystem", ecosystem, "key", cacheKey, "error", err)
		return
	}
	for _, row := range rows {
		legacy := legacyMetadataStoragePath(ecosystem, row.Name)
		if legacy == metadataStoragePath(ecosystem, row.Name) {
			continue
		}
		inUse := slices.ContainsFunc(rows, func(r database.MetadataPath) bool {
			return strings.EqualFold(r.StoragePath, legacy)
		})
		if inUse {
			return
		}
	}
	for _, row := range rows {
		legacy := legacyMetadataStoragePath(ecosystem, row.Name)
		if legacy == metadataStoragePath(ecosystem, row.Name) {
			continue
		}
		if err := p.DB.QueuePendingDelete(legacy); err != nil {
			p.Logger.Warn("failed to queue legacy metadata blob for deletion", "ecosystem", ecosystem, "key", row.Name, "error", err)
		}
	}
}
