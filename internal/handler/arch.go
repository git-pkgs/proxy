package handler

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/metrics"
)

const (
	archEcosystem             = "arch"
	defaultArchRepositoryName = "archlinux"
	// defaultArchUpstream is the official Arch Linux GeoIP mirror.
	defaultArchUpstream  = "https://geo.mirror.pkgbuild.com"
	archMatchCount       = 4 // full match + name + version + arch
	archSignatureSuffix  = ".sig"
	archAccept           = "*/*"
	archPackageMediaType = "application/octet-stream"
)

// ArchHandler serves pacman repositories at /arch/{repository}/. The rest of
// the path goes to the mirror unchanged, so the Arch Linux ARM layout works
// too. Databases are served byte for byte through the metadata cache, packages
// and their signatures through the artifact cache.
type ArchHandler struct {
	proxy        *Proxy
	proxyURL     string
	repositories map[string]string
	locks        sync.Map // database cache key -> *sync.Mutex
}

// NewArchHandler creates an Arch Linux handler. Without repositories it serves
// the official GeoIP mirror as "archlinux".
func NewArchHandler(proxy *Proxy, proxyURL string, repositories map[string]string) *ArchHandler {
	h := &ArchHandler{
		proxy:        proxy,
		proxyURL:     strings.TrimSuffix(proxyURL, "/"),
		repositories: make(map[string]string, len(repositories)),
	}
	for name, repositoryURL := range repositories {
		h.repositories[name] = strings.TrimSuffix(repositoryURL, "/")
	}
	if len(h.repositories) == 0 {
		h.repositories[defaultArchRepositoryName] = defaultArchUpstream
	}
	return h
}

// Routes returns the HTTP handler for pacman requests.
// Mount this at /arch on your router.
func (h *ArchHandler) Routes() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		path := strings.TrimPrefix(r.URL.Path, "/")

		// A decoded '?' or '#' would cut the upstream path short and cache one
		// file under another file's name.
		if containsPathTraversal(path) || strings.ContainsAny(path, "?#") {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}

		repository, rest, ok := strings.Cut(path, "/")
		upstreamURL, found := h.repositories[repository]
		if !ok || rest == "" || !found {
			http.NotFound(w, r)
			return
		}

		switch {
		case isArchDatabase(rest):
			h.handleDatabase(w, r, repository, upstreamURL, rest)
		case isArchDatabaseSignature(rest):
			h.handleDatabaseSignature(w, r, repository, upstreamURL, rest)
		case isArchPackage(rest) || isArchPackageSignature(rest):
			h.handlePackageDownload(w, r, repository, upstreamURL, rest)
		default:
			h.proxyFile(w, r, upstreamURL, rest)
		}
	})
}

// archDatabasePattern matches {repo}.db and {repo}.files, bare or as .tar.*.
var archDatabasePattern = regexp.MustCompile(`^[^/]+\.(?:db|files)(?:\.tar(?:\.(?:gz|xz|zst|bz2|lz4|lzo|lrz|lz|Z))?)?$`)

func archBase(path string) string {
	return path[strings.LastIndex(path, "/")+1:]
}

func isArchDatabase(path string) bool {
	return archDatabasePattern.MatchString(archBase(path))
}

func isArchDatabaseSignature(path string) bool {
	base, ok := strings.CutSuffix(archBase(path), archSignatureSuffix)
	return ok && archDatabasePattern.MatchString(base)
}

// archPackagePattern matches {pkgname}-{pkgver}-{pkgrel}-{arch}.pkg.tar[.ext],
// e.g. lz4-1:1.10.0-2-x86_64.pkg.tar.zst. pkgname may contain '-', and pkgver
// may carry an epoch.
var archPackagePattern = regexp.MustCompile(`^(.+)-((?:\d+:)?[^-/:]+-[^-/:]+)-([^-/:]+)\.pkg\.tar(?:\.(?:zst|xz|gz|bz2|lz4|lzo|lrz|lz|Z))?$`)

func isArchPackage(path string) bool {
	return archPackagePattern.MatchString(archBase(path))
}

func isArchPackageSignature(path string) bool {
	base, ok := strings.CutSuffix(archBase(path), archSignatureSuffix)
	return ok && archPackagePattern.MatchString(base)
}

// parseArchPackage returns name, pkgver-pkgrel and arch of a package or .sig.
func parseArchPackage(filename string) (name, version, arch string) {
	matches := archPackagePattern.FindStringSubmatch(strings.TrimSuffix(filename, archSignatureSuffix))
	if len(matches) != archMatchCount {
		return "", "", ""
	}
	return matches[1], matches[2], matches[3]
}

// handlePackageDownload caches packages and their signatures. The cache key
// holds repository and full path, since two repositories may ship different
// bytes under one filename.
func (h *ArchHandler) handlePackageDownload(w http.ResponseWriter, r *http.Request, repository, upstreamURL, path string) {
	name, version, arch := parseArchPackage(archBase(path))
	if name == "" {
		h.proxyFile(w, r, upstreamURL, path)
		return
	}

	downloadURL := upstreamURL + "/" + path
	cacheFilename := repository + "/" + path

	h.proxy.Logger.Info("arch package download",
		"repository", repository, "name", name, "version", version, "arch", arch,
		"signature", strings.HasSuffix(path, archSignatureSuffix))

	result, err := h.proxy.GetOrFetchArtifactFromURL(
		r.Context(), archEcosystem, name, version, cacheFilename, downloadURL)
	if err != nil {
		h.proxy.serveArtifactError(w, err, "failed to fetch package")
		return
	}

	if result.Artifact.MediaType == "" {
		result.Artifact.MediaType = archPackageMediaType
	}
	ServeArtifactRequest(w, r, result)
}

// handleMetadata serves databases unchanged through the metadata cache.
func (h *ArchHandler) handleMetadata(w http.ResponseWriter, r *http.Request, repository, upstreamURL, path string) {
	h.proxy.ProxyCached(w, r, upstreamURL+"/"+path, archEcosystem,
		h.metadataCacheKey(repository, upstreamURL, path), archAccept)
}

// archDatabase names a database and its signature in the cache.
type archDatabase struct {
	dbKey, dbURL   string
	sigKey, sigURL string
}

func (h *ArchHandler) database(repository, upstreamURL, path string) archDatabase {
	return archDatabase{
		dbKey:  h.metadataCacheKey(repository, upstreamURL, path),
		dbURL:  upstreamURL + "/" + path,
		sigKey: h.metadataCacheKey(repository, upstreamURL, path+archSignatureSuffix),
		sigURL: upstreamURL + "/" + path + archSignatureSuffix,
	}
}

func (d archDatabase) coalesceKey(kind string) string {
	return metadataCoalesceKey(archEcosystem, d.dbKey, d.dbURL, archAccept, "identity", false) + "\x00" + kind
}

// handleDatabase serves a database. Each refresh fetches the signature too, so
// a signature requested later still matches the database the client got.
func (h *ArchHandler) handleDatabase(w http.ResponseWriter, r *http.Request, repository, upstreamURL, path string) {
	p := h.proxy
	if !p.CacheMetadata || p.DB == nil {
		h.handleMetadata(w, r, repository, upstreamURL, path)
		return
	}
	d := h.database(repository, upstreamURL, path)
	if _, hit := p.cachedMetadataState(r.Context(), archEcosystem, d.dbKey, nil); hit != nil {
		metrics.RecordMetadataCacheHit(archEcosystem, len(hit.body))
		h.writeMetadata(w, r, d.dbKey, *hit)
		return
	}
	p.recordMetadataCacheMiss(archEcosystem)
	res := h.sharedRefresh(r.Context(), d, true)
	h.writeMetadata(w, r, d.dbKey, res)
}

// handleDatabaseSignature serves the signature stored for the cached
// database's content, never one fetched for another version of it.
func (h *ArchHandler) handleDatabaseSignature(w http.ResponseWriter, r *http.Request, repository, upstreamURL, path string) {
	p := h.proxy
	if !p.CacheMetadata || p.DB == nil {
		h.handleMetadata(w, r, repository, upstreamURL, path)
		return
	}
	d := h.database(repository, upstreamURL, strings.TrimSuffix(path, archSignatureSuffix))
	if res, ok := h.cachedSignature(r.Context(), d); ok {
		metrics.RecordMetadataCacheHit(archEcosystem, len(res.body))
		h.writeMetadata(w, r, d.sigKey, res)
		return
	}
	p.recordMetadataCacheMiss(archEcosystem)
	// Nothing stored for the cached database, e.g. its signature fetch failed
	// during the last refresh. The client may already hold that database, so
	// only a signature for it will do; if the refresh replaces it, fail.
	served := h.databaseDigest(d)
	res, _ := p.coalesceMetadata(r.Context(), d.coalesceKey("signature\x00"+served), func(ctx context.Context) metadataResult {
		if res, ok := h.storedSignature(ctx, d, served); ok {
			return res
		}
		if res := h.sharedRefresh(ctx, d, false); res.err != nil {
			return res
		}
		if served == "" {
			if res, ok := h.cachedSignature(ctx, d); ok {
				return res
			}
		} else if res, ok := h.storedSignature(ctx, d, served); ok {
			return res
		}
		return metadataResult{err: errArchSignatureUnpaired}
	})
	h.writeMetadata(w, r, d.sigKey, res)
}

var errArchSignatureUnpaired = errors.New("database signature could not be paired with the database")

// sharedRefresh runs one refresh per database at a time, so a slow fetch
// cannot store an older database over one cached after it started. With
// ifStale, a refresh that finds the database fresh again skips the upstream.
func (h *ArchHandler) sharedRefresh(ctx context.Context, d archDatabase, ifStale bool) metadataResult {
	p := h.proxy
	res, _ := p.coalesceMetadata(ctx, d.coalesceKey("database"), func(ctx context.Context) metadataResult {
		entry, hit := p.cachedMetadataState(ctx, archEcosystem, d.dbKey, nil)
		if hit != nil && ifStale {
			return *hit
		}
		res, _ := h.refresh(ctx, d, entry)
		return res
	})
	return res
}

// archPairAttempts bounds refetches while the mirror replaces the database.
const archPairAttempts = 3

// refresh fetches the database, then its signature, and stores the signature
// for the database's content. It returns the database as cached and why no
// signature could be stored, if so.
func (h *ArchHandler) refresh(ctx context.Context, d archDatabase, entry *database.MetadataCacheEntry) (metadataResult, error) {
	p := h.proxy
	res, row := h.fetchDatabase(ctx, d, entry)
	if res.err != nil {
		return res, res.err
	}
	for range archPairAttempts {
		if row == nil || !row.ContentDigest.Valid {
			return res, errArchSignatureUnpaired
		}
		dbDigest := row.ContentDigest.String
		if _, ok := h.storedSignature(ctx, d, dbDigest); ok {
			return res, nil
		}

		sig, err := p.fetchUpstreamMetadata(ctx, d.sigURL, nil, archAccept, "identity")
		if errors.Is(err, ErrUpstreamNotFound) {
			h.storeSignature(ctx, d, dbDigest, nil)
			return res, err
		}
		if err != nil {
			return res, err
		}

		// The mirror may have replaced the database in the meantime.
		again, err := p.fetchUpstreamMetadata(ctx, d.dbURL, row, archAccept, "identity")
		if err != nil {
			return res, err
		}
		if archDigest(again.body) == dbDigest {
			h.storeSignature(ctx, d, dbDigest, sig)
			return res, nil
		}
		if res, row = h.storeDatabase(ctx, d, again); res.err != nil {
			return res, res.err
		}
	}
	return res, errArchSignatureUnpaired
}

// fetchDatabase fetches and caches the database, falling back to the cached
// copy when the upstream fails.
func (h *ArchHandler) fetchDatabase(ctx context.Context, d archDatabase, entry *database.MetadataCacheEntry) (metadataResult, *database.MetadataCacheEntry) {
	p := h.proxy
	meta, err := p.fetchUpstreamMetadata(ctx, d.dbURL, entry, archAccept, "identity")
	if errors.Is(err, errStale304) {
		meta, err = p.fetchUpstreamMetadata(ctx, d.dbURL, nil, archAccept, "identity")
	}
	if err == nil {
		return h.storeDatabase(ctx, d, meta)
	}
	if entry == nil {
		return metadataResult{err: fmt.Errorf("upstream failed and no cached metadata: %w", err)}, nil
	}
	p.Logger.Warn("upstream metadata fetch failed, checking cache", "ecosystem", archEcosystem, "key", d.dbKey, "error", err)
	return h.readDatabase(ctx, p.currentMetadataEntry(archEcosystem, d.dbKey, entry), err)
}

// storeDatabase caches meta under its digest and returns the database the
// cache row now describes. That is meta unless the write failed; bytes the
// row does not describe would be paired with the wrong signature.
func (h *ArchHandler) storeDatabase(ctx context.Context, d archDatabase, meta *upstreamMetadata) (metadataResult, *database.MetadataCacheEntry) {
	p := h.proxy
	path := archBlobPath(d.dbKey, archDigest(meta.body))
	defer h.lock(d.dbKey)()

	previous, _ := p.DB.GetMetadataCache(archEcosystem, d.dbKey)
	if previous != nil && previous.StoragePath == path {
		// Same bytes: a failed rewrite would delete the blob the row uses.
		h.touchDatabase(previous, meta)
	} else {
		p.cacheMetadataBlob(ctx, archEcosystem, d.dbKey, path, meta)
	}
	row, _ := p.DB.GetMetadataCache(archEcosystem, d.dbKey)
	if row == nil {
		// Nothing cached at all: serve it like an uncached proxy would.
		return metadataResult{body: meta.body, contentType: meta.contentType, contentEncoding: meta.contentEncoding}, nil
	}
	if row.StoragePath != path {
		return h.readDatabase(ctx, row, errArchDatabaseNotCached)
	}
	if previous != nil && previous.StoragePath != path {
		_ = p.Storage.Delete(ctx, previous.StoragePath)
	}
	return metadataResult{body: meta.body, contentType: meta.contentType, contentEncoding: meta.contentEncoding}, row
}

// touchDatabase marks an unchanged cached database as fetched now.
func (h *ArchHandler) touchDatabase(row *database.MetadataCacheEntry, meta *upstreamMetadata) {
	updated := *row
	updated.ETag = sql.NullString{String: meta.etag, Valid: meta.etag != ""}
	updated.LastModified = sql.NullTime{Time: meta.lastModified, Valid: !meta.lastModified.IsZero()}
	updated.FetchedAt = sql.NullTime{Time: time.Now(), Valid: true}
	if err := h.proxy.DB.UpsertMetadataCache(&updated); err != nil {
		h.proxy.Logger.Warn("failed to record cached metadata", "ecosystem", archEcosystem, "key", row.Name, "error", err)
	}
}

var errArchDatabaseNotCached = errors.New("database could not be cached")

// readDatabase returns the cached database row's bytes, or cause if they are
// unreadable.
func (h *ArchHandler) readDatabase(ctx context.Context, row *database.MetadataCacheEntry, cause error) (metadataResult, *database.MetadataCacheEntry) {
	body, contentType, err := h.proxy.readCachedMetadata(ctx, row, nil)
	if err != nil {
		return metadataResult{err: fmt.Errorf("cached database unusable (%v): %w", err, cause)}, nil
	}
	return metadataResult{body: body, contentType: contentType, contentEncoding: row.ContentEncoding.String}, row
}

// lock serializes cache writes for one database and its signature, so a row
// never points at a blob another write has just removed.
func (h *ArchHandler) lock(dbKey string) func() {
	m, _ := h.locks.LoadOrStore(dbKey, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// cachedSignature returns the signature stored for the cached database.
func (h *ArchHandler) cachedSignature(ctx context.Context, d archDatabase) (metadataResult, bool) {
	dbDigest := h.databaseDigest(d)
	if dbDigest == "" {
		return metadataResult{}, false
	}
	return h.storedSignature(ctx, d, dbDigest)
}

// databaseDigest returns the cached database's content digest, or "".
func (h *ArchHandler) databaseDigest(d archDatabase) string {
	current, _ := h.proxy.DB.GetMetadataCache(archEcosystem, d.dbKey)
	if current == nil || !current.ContentDigest.Valid {
		return ""
	}
	return current.ContentDigest.String
}

// storedSignature returns the signature stored for dbDigest. The storage
// path names the digest, so a refresh with unchanged bytes keeps it.
func (h *ArchHandler) storedSignature(ctx context.Context, d archDatabase, dbDigest string) (metadataResult, bool) {
	p := h.proxy
	entry, _ := p.DB.GetMetadataCache(archEcosystem, d.sigKey)
	if entry == nil {
		return metadataResult{}, false
	}
	switch entry.StoragePath {
	case archMissingSignaturePath(d, dbDigest):
		// Ask again after the TTL, in case the 404 was transient.
		if p.MetadataTTL <= 0 || !entry.FetchedAt.Valid || time.Since(entry.FetchedAt.Time) >= p.MetadataTTL {
			return metadataResult{}, false
		}
		return metadataResult{err: ErrUpstreamNotFound}, true
	case archSignaturePath(d, dbDigest):
		body, contentType, err := p.readCachedMetadata(ctx, entry, nil)
		if err != nil {
			return metadataResult{}, false
		}
		return metadataResult{body: body, contentType: contentType, contentEncoding: entry.ContentEncoding.String}, true
	}
	return metadataResult{}, false
}

// storeSignature stores sig for dbDigest, or that it is missing when sig is
// nil, and drops the one stored for an earlier database.
func (h *ArchHandler) storeSignature(ctx context.Context, d archDatabase, dbDigest string, sig *upstreamMetadata) {
	p := h.proxy
	path := archSignaturePath(d, dbDigest)
	if sig == nil {
		path = archMissingSignaturePath(d, dbDigest)
		sig = &upstreamMetadata{contentType: archPackageMediaType}
	}
	defer h.lock(d.dbKey)()

	previous, _ := p.DB.GetMetadataCache(archEcosystem, d.sigKey)
	p.cacheMetadataBlob(ctx, archEcosystem, d.sigKey, path, sig)
	if previous == nil || previous.StoragePath == path {
		return
	}
	if current, _ := p.DB.GetMetadataCache(archEcosystem, d.sigKey); current != nil && current.StoragePath == path {
		_ = p.Storage.Delete(ctx, previous.StoragePath)
	}
}

// archBlobPath names a database or signature blob after the database digest
// it belongs to, so a cache row and its bytes always agree.
func archBlobPath(key, dbDigest string) string {
	return "_metadata/" + archEcosystem + "/" + key + "/" + strings.TrimPrefix(dbDigest, "sha256:")
}

func archSignaturePath(d archDatabase, dbDigest string) string {
	return archBlobPath(d.sigKey, dbDigest)
}

func archMissingSignaturePath(d archDatabase, dbDigest string) string {
	return archSignaturePath(d, dbDigest) + "-missing"
}

func archDigest(body []byte) string {
	digest := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func (h *ArchHandler) writeMetadata(w http.ResponseWriter, r *http.Request, cacheKey string, res metadataResult) {
	if res.err != nil {
		if errors.Is(res.err, ErrUpstreamNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		h.proxy.Logger.Error("metadata fetch failed", "error", res.err)
		http.Error(w, "failed to fetch from upstream", http.StatusBadGateway)
		return
	}
	h.proxy.writeMetadataCachedResponseWithEncoding(w, r, archEcosystem, cacheKey, res.body, res.contentType, res.contentEncoding)
}

// metadataCacheKey hashes repository, upstream URL and path, as APKHandler does.
func (h *ArchHandler) metadataCacheKey(repository, upstreamURL, path string) string {
	identity := repository + "\x00" + upstreamURL + "\x00" + path
	digest := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(digest[:])
}

// proxyFile passes other files (lastupdate, ISOs) through uncached.
func (h *ArchHandler) proxyFile(w http.ResponseWriter, r *http.Request, upstreamURL, path string) {
	h.proxy.ProxyFile(w, r, upstreamURL+"/"+path)
}
