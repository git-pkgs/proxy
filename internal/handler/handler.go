// Package handler provides HTTP protocol handlers for package manager proxying.
package handler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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
	DB         *database.DB
	Storage    storage.Storage
	Fetcher    fetch.FetcherInterface
	Resolver   *fetch.Resolver
	Logger     *slog.Logger
	Cooldown   *cooldown.Config
	HTTPClient *http.Client
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
		PURL:       pkgPURL,
		Ecosystem:  ecosystem,
		Name:       name,
		RegistryURL: sql.NullString{String: upstreamURL, Valid: true},
		EnrichedAt: sql.NullTime{Time: now, Valid: true},
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

// ProxyMetadata forwards a metadata request to upstream, copying only specific response headers.
func (p *Proxy) ProxyMetadata(w http.ResponseWriter, r *http.Request, upstreamURL string, logLabel string) {
	p.Logger.Debug(logLabel+" metadata request", "url", upstreamURL)

	req, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, nil)
	if err != nil {
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
	}

	for _, header := range []string{"Accept", "Accept-Encoding", "If-Modified-Since", "If-None-Match"} {
		if v := r.Header.Get(header); v != "" {
			req.Header.Set(header, v)
		}
	}

	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		p.Logger.Error("failed to fetch upstream metadata", "error", err)
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
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":%q}`, message)
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

