// Package handler provides HTTP protocol handlers for package manager proxying.
package handler

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/git-pkgs/proxy/internal/cooldown"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/metrics"
	"github.com/git-pkgs/proxy/internal/storage"
	"github.com/git-pkgs/purl"
	"github.com/git-pkgs/registries/fetch"
)

// containsPathTraversal returns true if the path contains ".." segments
// that could be used to escape the intended directory.
func containsPathTraversal(path string) bool {
	for _, segment := range strings.Split(path, "/") {
		if segment == ".." {
			return true
		}
	}
	return false
}

const defaultHTTPTimeout = 30 * time.Second

const contentTypeJSON = "application/json"

// maxMetadataSize is the maximum size of upstream metadata responses (100 MB).
// Package metadata (e.g. npm with many versions) can be large, but unbounded
// reads risk OOM if an upstream misbehaves.
const maxMetadataSize = 100 << 20

// ErrMetadataTooLarge is returned when upstream metadata exceeds maxMetadataSize.
var ErrMetadataTooLarge = errors.New("metadata response exceeds size limit")

// ReadMetadata reads an upstream response body with a size limit to prevent OOM
// from unexpectedly large responses. Returns ErrMetadataTooLarge if the response
// is truncated by the limit.
func ReadMetadata(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxMetadataSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxMetadataSize {
		return nil, ErrMetadataTooLarge
	}
	return data, nil
}

// Proxy provides shared functionality for protocol handlers.
type Proxy struct {
	DB            *database.DB
	Storage       storage.Storage
	Fetcher       fetch.FetcherInterface
	Resolver      *fetch.Resolver
	Logger        *slog.Logger
	Cooldown      *cooldown.Config
	CacheMetadata bool
	MetadataTTL   time.Duration
	HTTPClient    *http.Client
}

// NewProxy creates a new Proxy with the given dependencies.
func NewProxy(db *database.DB, store storage.Storage, fetcher fetch.FetcherInterface, resolver *fetch.Resolver, logger *slog.Logger) *Proxy {
	if logger == nil {
		logger = slog.Default()
	}
	return &Proxy{
		DB:       db,
		Storage:  store,
		Fetcher:  fetcher,
		Resolver: resolver,
		Logger:   logger,
		HTTPClient: &http.Client{
			Timeout: defaultHTTPTimeout,
		},
	}
}

// CacheResult contains information about a cached or fetched artifact.
type CacheResult struct {
	Reader      io.ReadCloser
	Size        int64
	ContentType string
	Hash        string
	Cached      bool
}

// GetOrFetchArtifact retrieves an artifact from cache or fetches from upstream.
func (p *Proxy) GetOrFetchArtifact(ctx context.Context, ecosystem, name, version, filename string) (*CacheResult, error) {
	pkgPURL := purl.MakePURLString(ecosystem, name, "")
	versionPURL := purl.MakePURLString(ecosystem, name, version)

	if cached, err := p.checkCache(ctx, pkgPURL, versionPURL, filename); err != nil {
		return nil, err
	} else if cached != nil {
		return cached, nil
	}

	return p.fetchAndCache(ctx, ecosystem, name, version, filename, pkgPURL, versionPURL)
}

// checkCache looks up an artifact in the cache. Returns nil if not cached.
func (p *Proxy) checkCache(ctx context.Context, pkgPURL, versionPURL, filename string) (*CacheResult, error) {
	pkg, err := p.DB.GetPackageByPURL(pkgPURL)
	if err != nil {
		return nil, fmt.Errorf("checking package cache: %w", err)
	}
	if pkg == nil {
		return nil, nil
	}

	ver, err := p.DB.GetVersionByPURL(versionPURL)
	if err != nil {
		return nil, fmt.Errorf("checking version cache: %w", err)
	}
	if ver == nil {
		return nil, nil
	}

	artifact, err := p.DB.GetArtifact(versionPURL, filename)
	if err != nil {
		return nil, fmt.Errorf("checking artifact cache: %w", err)
	}
	if artifact == nil || !artifact.IsCached() {
		return nil, nil
	}

	start := time.Now()
	reader, err := p.Storage.Open(ctx, artifact.StoragePath.String)
	metrics.RecordStorageOperation("read", time.Since(start))
	if err != nil {
		metrics.RecordStorageError("read")
		p.Logger.Warn("cached artifact missing from storage, will refetch",
			"path", artifact.StoragePath.String, "error", err)
		return nil, nil
	}

	_ = p.DB.RecordArtifactHit(versionPURL, filename)

	// Extract ecosystem from pkgPURL for metrics
	if p, err := purl.Parse(pkgPURL); err == nil {
		metrics.RecordCacheHit(purl.PURLTypeToEcosystem(p.Type))
	}

	return &CacheResult{
		Reader:      reader,
		Size:        artifact.Size.Int64,
		ContentType: artifact.ContentType.String,
		Hash:        artifact.ContentHash.String,
		Cached:      true,
	}, nil
}

func (p *Proxy) fetchAndCache(ctx context.Context, ecosystem, name, version, filename, pkgPURL, versionPURL string) (*CacheResult, error) {
	// Record cache miss
	metrics.RecordCacheMiss(ecosystem)

	// Resolve download URL
	info, err := p.Resolver.Resolve(ctx, ecosystem, name, version)
	if err != nil {
		return nil, fmt.Errorf("resolving download URL: %w", err)
	}

	// Use resolved filename if provided filename is empty
	if filename == "" {
		filename = info.Filename
	}

	p.Logger.Info("fetching from upstream",
		"ecosystem", ecosystem, "name", name, "version", version, "url", info.URL)

	// Fetch from upstream with timing
	fetchStart := time.Now()
	artifact, err := p.Fetcher.Fetch(ctx, info.URL)
	fetchDuration := time.Since(fetchStart)

	if err != nil {
		metrics.RecordUpstreamFetch(ecosystem, fetchDuration)
		metrics.RecordUpstreamError(ecosystem, "fetch_failed")
		return nil, fmt.Errorf("fetching from upstream: %w", err)
	}
	metrics.RecordUpstreamFetch(ecosystem, fetchDuration)

	// Store in cache
	storagePath := storage.ArtifactPath(ecosystem, "", name, version, filename)
	storeStart := time.Now()
	size, hash, err := p.Storage.Store(ctx, storagePath, artifact.Body)
	_ = artifact.Body.Close()
	metrics.RecordStorageOperation("write", time.Since(storeStart))

	if err != nil {
		metrics.RecordStorageError("write")
		return nil, fmt.Errorf("storing artifact: %w", err)
	}

	// Update database
	if err := p.updateCacheDB(ecosystem, name, filename, pkgPURL, versionPURL, info.URL, storagePath, hash, size, artifact.ContentType); err != nil {
		p.Logger.Warn("failed to update cache database", "error", err)
		// Continue anyway - we have the file
	}

	// Open the stored file to return
	readStart := time.Now()
	reader, err := p.Storage.Open(ctx, storagePath)
	metrics.RecordStorageOperation("read", time.Since(readStart))

	if err != nil {
		metrics.RecordStorageError("read")
		return nil, fmt.Errorf("opening cached artifact: %w", err)
	}

	return &CacheResult{
		Reader:      reader,
		Size:        size,
		ContentType: artifact.ContentType,
		Hash:        hash,
		Cached:      false,
	}, nil
}

func (p *Proxy) updateCacheDB(ecosystem, name, filename, pkgPURL, versionPURL, upstreamURL, storagePath, hash string, size int64, contentType string) error {
	now := time.Now()

	// Upsert package
	pkg := &database.Package{
		PURL:        pkgPURL,
		Ecosystem:   ecosystem,
		Name:        name,
		RegistryURL: sql.NullString{String: upstreamURL, Valid: true},
		EnrichedAt:  sql.NullTime{Time: now, Valid: true},
	}
	if err := p.DB.UpsertPackage(pkg); err != nil {
		return fmt.Errorf("upserting package: %w", err)
	}

	// Upsert version
	ver := &database.Version{
		PURL:        versionPURL,
		PackagePURL: pkgPURL,
		EnrichedAt:  sql.NullTime{Time: now, Valid: true},
	}
	if err := p.DB.UpsertVersion(ver); err != nil {
		return fmt.Errorf("upserting version: %w", err)
	}

	// Upsert artifact
	art := &database.Artifact{
		VersionPURL: versionPURL,
		Filename:    filename,
		UpstreamURL: upstreamURL,
		StoragePath: sql.NullString{String: storagePath, Valid: true},
		ContentHash: sql.NullString{String: hash, Valid: true},
		Size:        sql.NullInt64{Int64: size, Valid: true},
		ContentType: sql.NullString{String: contentType, Valid: true},
		FetchedAt:   sql.NullTime{Time: now, Valid: true},
	}
	if err := p.DB.UpsertArtifact(art); err != nil {
		return fmt.Errorf("upserting artifact: %w", err)
	}

	return nil
}

// ServeArtifact writes a CacheResult to an HTTP response.
func ServeArtifact(w http.ResponseWriter, result *CacheResult) {
	defer func() { _ = result.Reader.Close() }()

	if result.ContentType != "" {
		w.Header().Set("Content-Type", result.ContentType)
	}
	if result.Size > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", result.Size))
	}
	if result.Hash != "" {
		w.Header().Set("ETag", fmt.Sprintf(`"%s"`, result.Hash))
	}

	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, result.Reader)
}

// ProxyUpstream forwards a request to an upstream URL without caching.
// It copies the request, forwards specified headers, and streams the response back.
// If forwardHeaders is nil, all response headers are copied.
func (p *Proxy) ProxyUpstream(w http.ResponseWriter, r *http.Request, upstreamURL string, forwardHeaders []string) {
	p.Logger.Debug("proxying to upstream", "url", upstreamURL)

	req, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, nil)
	if err != nil {
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
	}

	// Copy request headers that affect content negotiation / caching
	for _, header := range forwardHeaders {
		if v := r.Header.Get(header); v != "" {
			req.Header.Set(header, v)
		}
	}

	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		p.Logger.Error("upstream request failed", "error", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}

	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// ProxyFile forwards a file request to upstream, copying all response headers.
func (p *Proxy) ProxyFile(w http.ResponseWriter, r *http.Request, upstreamURL string) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, nil)
	if err != nil {
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
	}

	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		http.Error(w, "failed to fetch from upstream", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	for key, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}

	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// JSONError writes a JSON error response.
func JSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":%q}`, message)
}

// ErrUpstreamNotFound indicates the upstream returned 404.
var ErrUpstreamNotFound = fmt.Errorf("upstream: not found")

// errStale304 is returned when upstream sends 304 but the cached file is missing.
var errStale304 = fmt.Errorf("upstream returned 304 but cached file is missing")

// metadataStoragePath builds a storage path for cached metadata.
func metadataStoragePath(ecosystem, cacheKey string) string {
	return "_metadata/" + ecosystem + "/" + cacheKey + "/metadata"
}

// FetchOrCacheMetadata fetches metadata from upstream with caching.
// On success it returns the raw response bytes and content type.
// If upstream fails and a cached copy exists, the cached version is returned.
// cacheKey is typically the package name but can include subpath components.
// Optional acceptHeaders specify the Accept header(s) to send; defaults to application/json.
func (p *Proxy) FetchOrCacheMetadata(ctx context.Context, ecosystem, cacheKey, upstreamURL string, acceptHeaders ...string) ([]byte, string, error) {
	if containsPathTraversal(cacheKey) {
		return nil, "", fmt.Errorf("invalid cache key: %q", cacheKey)
	}

	storagePath := metadataStoragePath(ecosystem, cacheKey)

	// Check for existing cache entry (for ETag revalidation and TTL)
	var entry *database.MetadataCacheEntry
	if p.CacheMetadata && p.DB != nil {
		entry, _ = p.DB.GetMetadataCache(ecosystem, cacheKey)
	}

	// Serve from cache if within TTL (skip upstream entirely)
	if entry != nil && p.MetadataTTL > 0 && entry.FetchedAt.Valid {
		if time.Since(entry.FetchedAt.Time) < p.MetadataTTL {
			cached, readErr := p.Storage.Open(ctx, entry.StoragePath)
			if readErr == nil {
				defer func() { _ = cached.Close() }()
				data, readErr := ReadMetadata(cached)
				if readErr == nil {
					ct := contentTypeJSON
					if entry.ContentType.Valid {
						ct = entry.ContentType.String
					}
					return data, ct, nil
				}
			}
			// Cache file missing/unreadable, fall through to upstream
		}
	}

	accept := contentTypeJSON
	if len(acceptHeaders) > 0 && acceptHeaders[0] != "" {
		accept = acceptHeaders[0]
	}

	// Try upstream
	body, contentType, etag, lastModified, err := p.fetchUpstreamMetadata(ctx, upstreamURL, entry, accept)
	if errors.Is(err, errStale304) {
		// 304 but cached file is gone; retry without ETag
		body, contentType, etag, lastModified, err = p.fetchUpstreamMetadata(ctx, upstreamURL, nil, accept)
	}
	if err == nil {
		if p.CacheMetadata {
			p.cacheMetadataBlob(ctx, ecosystem, cacheKey, storagePath, body, contentType, etag, lastModified)
		}
		return body, contentType, nil
	}

	// Upstream failed -- fall back to cache if available
	if !p.CacheMetadata || entry == nil {
		return nil, "", fmt.Errorf("upstream failed and no cached metadata: %w", err)
	}

	p.Logger.Warn("upstream metadata fetch failed, checking cache",
		"ecosystem", ecosystem, "key", cacheKey, "error", err)

	cached, readErr := p.Storage.Open(ctx, entry.StoragePath)
	if readErr != nil {
		return nil, "", fmt.Errorf("upstream failed and cached file missing: %w", err)
	}
	defer func() { _ = cached.Close() }()

	data, readErr := ReadMetadata(cached)
	if readErr != nil {
		return nil, "", fmt.Errorf("upstream failed and cached read error: %w", err)
	}

	ct := contentTypeJSON
	if entry.ContentType.Valid {
		ct = entry.ContentType.String
	}
	p.Logger.Info("serving metadata from cache",
		"ecosystem", ecosystem, "key", cacheKey)
	return data, ct, nil
}

// fetchUpstreamMetadata fetches metadata from upstream, using ETag for conditional revalidation.
// Returns the body, content type, ETag, upstream Last-Modified time, and any error.
func (p *Proxy) fetchUpstreamMetadata(ctx context.Context, upstreamURL string, entry *database.MetadataCacheEntry, accept string) ([]byte, string, string, time.Time, error) {
	var zeroTime time.Time

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamURL, nil)
	if err != nil {
		return nil, "", "", zeroTime, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Accept", accept)

	if entry != nil && entry.ETag.Valid {
		req.Header.Set("If-None-Match", entry.ETag.String)
	}

	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return nil, "", "", zeroTime, fmt.Errorf("fetching metadata: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 304 Not Modified -- our cached copy is still good
	if resp.StatusCode == http.StatusNotModified && entry != nil {
		cached, readErr := p.Storage.Open(ctx, entry.StoragePath)
		if readErr != nil {
			return nil, "", "", zeroTime, errStale304
		}
		defer func() { _ = cached.Close() }()
		data, readErr := ReadMetadata(cached)
		if readErr != nil {
			return nil, "", "", zeroTime, errStale304
		}
		ct := contentTypeJSON
		if entry.ContentType.Valid {
			ct = entry.ContentType.String
		}
		lm := zeroTime
		if entry.LastModified.Valid {
			lm = entry.LastModified.Time
		}
		return data, ct, entry.ETag.String, lm, nil
	}

	if resp.StatusCode == http.StatusNotFound {
		return nil, "", "", zeroTime, ErrUpstreamNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", "", zeroTime, fmt.Errorf("upstream returned %d", resp.StatusCode)
	}

	body, err := ReadMetadata(resp.Body)
	if err != nil {
		return nil, "", "", zeroTime, fmt.Errorf("reading response: %w", err)
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = contentTypeJSON
	}

	etag := resp.Header.Get("ETag")

	var lastModified time.Time
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		lastModified, _ = http.ParseTime(lm)
	}

	return body, contentType, etag, lastModified, nil
}

// cacheMetadataBlob stores metadata bytes in storage and updates the database.
func (p *Proxy) cacheMetadataBlob(ctx context.Context, ecosystem, cacheKey, storagePath string, data []byte, contentType, etag string, lastModified time.Time) {
	if p.DB == nil || p.Storage == nil {
		return
	}

	size, _, err := p.Storage.Store(ctx, storagePath, bytes.NewReader(data))
	if err != nil {
		p.Logger.Warn("failed to cache metadata", "ecosystem", ecosystem, "key", cacheKey, "error", err)
		return
	}

	_ = p.DB.UpsertMetadataCache(&database.MetadataCacheEntry{
		Ecosystem:    ecosystem,
		Name:         cacheKey,
		StoragePath:  storagePath,
		ETag:         sql.NullString{String: etag, Valid: etag != ""},
		ContentType:  sql.NullString{String: contentType, Valid: contentType != ""},
		Size:         sql.NullInt64{Int64: size, Valid: true},
		LastModified: sql.NullTime{Time: lastModified, Valid: !lastModified.IsZero()},
		FetchedAt:    sql.NullTime{Time: time.Now(), Valid: true},
	})
}

// cachedMeta holds cache validators and freshness state from a metadata cache entry.
type cachedMeta struct {
	etag         string
	lastModified time.Time
	stale        bool
}

// lookupCachedMeta retrieves cache validators for a metadata entry.
func (p *Proxy) lookupCachedMeta(ecosystem, cacheKey string) cachedMeta {
	if p.DB == nil {
		return cachedMeta{}
	}
	entry, err := p.DB.GetMetadataCache(ecosystem, cacheKey)
	if err != nil || entry == nil {
		return cachedMeta{}
	}
	var cm cachedMeta
	if entry.ETag.Valid {
		cm.etag = entry.ETag.String
	}
	if entry.LastModified.Valid {
		cm.lastModified = entry.LastModified.Time
	}
	// If FetchedAt is older than TTL, upstream must have failed and
	// we served from stale cache (successful fetches update FetchedAt).
	if p.MetadataTTL > 0 && entry.FetchedAt.Valid && time.Since(entry.FetchedAt.Time) > p.MetadataTTL {
		cm.stale = true
	}
	return cm
}

// ProxyCached fetches metadata from upstream (with optional caching for offline fallback)
// and writes it to the response. Optional acceptHeaders specify the Accept header to send.
// When metadata caching is disabled, the response is streamed directly to avoid buffering
// large metadata responses (e.g. npm packages with many versions) in memory.
func (p *Proxy) ProxyCached(w http.ResponseWriter, r *http.Request, upstreamURL, ecosystem, cacheKey string, acceptHeaders ...string) {
	if !p.CacheMetadata {
		// Stream directly without buffering when caching is off.
		p.proxyMetadataStream(w, r, upstreamURL, acceptHeaders...)
		return
	}

	body, contentType, err := p.FetchOrCacheMetadata(r.Context(), ecosystem, cacheKey, upstreamURL, acceptHeaders...)
	if err != nil {
		if errors.Is(err, ErrUpstreamNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		p.Logger.Error("metadata fetch failed", "error", err)
		http.Error(w, "failed to fetch from upstream", http.StatusBadGateway)
		return
	}

	cm := p.lookupCachedMeta(ecosystem, cacheKey)

	// Honor client conditional request headers
	if cm.etag != "" {
		if match := r.Header.Get("If-None-Match"); match != "" && match == cm.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	if !cm.lastModified.IsZero() {
		if ims := r.Header.Get("If-Modified-Since"); ims != "" {
			if t, err := http.ParseTime(ims); err == nil && !cm.lastModified.After(t) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if cm.etag != "" {
		w.Header().Set("ETag", cm.etag)
	}
	if !cm.lastModified.IsZero() {
		w.Header().Set("Last-Modified", cm.lastModified.UTC().Format(http.TimeFormat))
	}
	if cm.stale {
		w.Header().Set("Warning", `110 - "Response is Stale"`)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// proxyMetadataStream forwards an upstream metadata response by streaming it to the client
// without buffering the full body in memory.
func (p *Proxy) proxyMetadataStream(w http.ResponseWriter, r *http.Request, upstreamURL string, acceptHeaders ...string) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
	}

	accept := contentTypeJSON
	if len(acceptHeaders) > 0 && acceptHeaders[0] != "" {
		accept = acceptHeaders[0]
	}
	req.Header.Set("Accept", accept)

	for _, header := range []string{"Accept-Encoding", "If-Modified-Since", "If-None-Match"} {
		if v := r.Header.Get(header); v != "" {
			req.Header.Set(header, v)
		}
	}

	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		http.Error(w, "failed to fetch from upstream", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	for _, header := range []string{"Content-Type", "Content-Length", "Last-Modified", "ETag"} {
		if v := resp.Header.Get(header); v != "" {
			w.Header().Set(header, v)
		}
	}

	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// GetOrFetchArtifactFromURL retrieves an artifact from cache or fetches from a specific URL.
// This is useful for registries where download URLs are determined from metadata.
func (p *Proxy) GetOrFetchArtifactFromURL(ctx context.Context, ecosystem, name, version, filename, downloadURL string) (*CacheResult, error) {
	return p.GetOrFetchArtifactFromURLWithHeaders(ctx, ecosystem, name, version, filename, downloadURL, nil)
}

// GetOrFetchArtifactFromURLWithHeaders retrieves an artifact from cache or fetches from a URL
// with additional HTTP headers. This is needed for registries that require authentication
// (e.g. Docker Hub requires a Bearer token even for public images).
func (p *Proxy) GetOrFetchArtifactFromURLWithHeaders(ctx context.Context, ecosystem, name, version, filename, downloadURL string, headers http.Header) (*CacheResult, error) {
	pkgPURL := purl.MakePURLString(ecosystem, name, "")
	versionPURL := purl.MakePURLString(ecosystem, name, version)

	if cached, err := p.checkCache(ctx, pkgPURL, versionPURL, filename); err != nil {
		return nil, err
	} else if cached != nil {
		return cached, nil
	}

	return p.fetchAndCacheFromURL(ctx, ecosystem, name, version, filename, pkgPURL, versionPURL, downloadURL, headers)
}

func (p *Proxy) fetchAndCacheFromURL(ctx context.Context, ecosystem, name, version, filename, pkgPURL, versionPURL, downloadURL string, headers http.Header) (*CacheResult, error) {
	p.Logger.Info("fetching from upstream",
		"ecosystem", ecosystem, "name", name, "version", version, "url", downloadURL)

	artifact, err := p.Fetcher.FetchWithHeaders(ctx, downloadURL, headers)
	if err != nil {
		return nil, fmt.Errorf("fetching from upstream: %w", err)
	}

	storagePath := storage.ArtifactPath(ecosystem, "", name, version, filename)
	size, hash, err := p.Storage.Store(ctx, storagePath, artifact.Body)
	_ = artifact.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("storing artifact: %w", err)
	}

	if err := p.updateCacheDB(ecosystem, name, filename, pkgPURL, versionPURL, downloadURL, storagePath, hash, size, artifact.ContentType); err != nil {
		p.Logger.Warn("failed to update cache database", "error", err)
	}

	reader, err := p.Storage.Open(ctx, storagePath)
	if err != nil {
		return nil, fmt.Errorf("opening cached artifact: %w", err)
	}

	return &CacheResult{
		Reader:      reader,
		Size:        size,
		ContentType: artifact.ContentType,
		Hash:        hash,
		Cached:      false,
	}, nil
}
