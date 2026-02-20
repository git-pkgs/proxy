// Package handler provides HTTP protocol handlers for package manager proxying.
package handler

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/metrics"
	"github.com/git-pkgs/proxy/internal/storage"
	"github.com/git-pkgs/registries/fetch"
)

// Proxy provides shared functionality for protocol handlers.
type Proxy struct {
	DB       *database.DB
	Storage  storage.Storage
	Fetcher  fetch.FetcherInterface
	Resolver *fetch.Resolver
	Logger   *slog.Logger
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
	pkgPURL := fmt.Sprintf("pkg:%s/%s", ecosystem, name)
	versionPURL := fmt.Sprintf("pkg:%s/%s@%s", ecosystem, name, version)

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
	ecosystem := extractEcosystem(pkgPURL)
	metrics.RecordCacheHit(ecosystem)

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
	if err := p.updateCacheDB(ctx, ecosystem, name, version, filename, pkgPURL, versionPURL, info.URL, storagePath, hash, size, artifact.ContentType); err != nil {
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

func (p *Proxy) updateCacheDB(ctx context.Context, ecosystem, name, version, filename, pkgPURL, versionPURL, upstreamURL, storagePath, hash string, size int64, contentType string) error {
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

// JSONError writes a JSON error response.
func JSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":%q}`, message)
}

// GetOrFetchArtifactFromURL retrieves an artifact from cache or fetches from a specific URL.
// This is useful for registries where download URLs are determined from metadata.
func (p *Proxy) GetOrFetchArtifactFromURL(ctx context.Context, ecosystem, name, version, filename, downloadURL string) (*CacheResult, error) {
	pkgPURL := fmt.Sprintf("pkg:%s/%s", ecosystem, name)
	versionPURL := fmt.Sprintf("pkg:%s/%s@%s", ecosystem, name, version)

	if cached, err := p.checkCache(ctx, pkgPURL, versionPURL, filename); err != nil {
		return nil, err
	} else if cached != nil {
		return cached, nil
	}

	return p.fetchAndCacheFromURL(ctx, ecosystem, name, version, filename, pkgPURL, versionPURL, downloadURL)
}

func (p *Proxy) fetchAndCacheFromURL(ctx context.Context, ecosystem, name, version, filename, pkgPURL, versionPURL, downloadURL string) (*CacheResult, error) {
	p.Logger.Info("fetching from upstream",
		"ecosystem", ecosystem, "name", name, "version", version, "url", downloadURL)

	artifact, err := p.Fetcher.Fetch(ctx, downloadURL)
	if err != nil {
		return nil, fmt.Errorf("fetching from upstream: %w", err)
	}

	storagePath := storage.ArtifactPath(ecosystem, "", name, version, filename)
	size, hash, err := p.Storage.Store(ctx, storagePath, artifact.Body)
	_ = artifact.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("storing artifact: %w", err)
	}

	if err := p.updateCacheDB(ctx, ecosystem, name, version, filename, pkgPURL, versionPURL, downloadURL, storagePath, hash, size, artifact.ContentType); err != nil {
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

// extractEcosystem extracts the ecosystem from a package PURL.
// PURL format: pkg:ecosystem/name[@version]
func extractEcosystem(purl string) string {
	if len(purl) < 5 || !startsWith(purl, "pkg:") {
		return "unknown"
	}
	rest := purl[4:] // Skip "pkg:"
	if idx := indexOf(rest, "/"); idx != -1 {
		return rest[:idx]
	}
	return "unknown"
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
