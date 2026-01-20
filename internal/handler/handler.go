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
	"github.com/git-pkgs/proxy/internal/storage"
	"github.com/git-pkgs/proxy/internal/upstream"
)

// Proxy provides shared functionality for protocol handlers.
type Proxy struct {
	DB       *database.DB
	Storage  storage.Storage
	Fetcher  *upstream.Fetcher
	Resolver *upstream.Resolver
	Logger   *slog.Logger
}

// NewProxy creates a new Proxy with the given dependencies.
func NewProxy(db *database.DB, store storage.Storage, fetcher *upstream.Fetcher, resolver *upstream.Resolver, logger *slog.Logger) *Proxy {
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
	// Build PURL for lookups
	pkgPURL := fmt.Sprintf("pkg:%s/%s", ecosystem, name)
	versionPURL := fmt.Sprintf("pkg:%s/%s@%s", ecosystem, name, version)

	// Check if we have this artifact cached
	pkg, err := p.DB.GetPackageByPURL(pkgPURL)
	if err != nil {
		return nil, fmt.Errorf("checking package cache: %w", err)
	}

	if pkg != nil {
		ver, err := p.DB.GetVersionByPURL(versionPURL)
		if err != nil {
			return nil, fmt.Errorf("checking version cache: %w", err)
		}

		if ver != nil {
			artifact, err := p.DB.GetArtifact(ver.ID, filename)
			if err != nil {
				return nil, fmt.Errorf("checking artifact cache: %w", err)
			}

			if artifact != nil && artifact.IsCached() {
				// Serve from cache
				reader, err := p.Storage.Open(ctx, artifact.StoragePath.String)
				if err != nil {
					p.Logger.Warn("cached artifact missing from storage, will refetch",
						"path", artifact.StoragePath.String, "error", err)
				} else {
					// Record hit
					_ = p.DB.RecordArtifactHit(artifact.ID)

					return &CacheResult{
						Reader:      reader,
						Size:        artifact.Size.Int64,
						ContentType: artifact.ContentType.String,
						Hash:        artifact.ContentHash.String,
						Cached:      true,
					}, nil
				}
			}
		}
	}

	// Not cached, fetch from upstream
	return p.fetchAndCache(ctx, ecosystem, name, version, filename, pkgPURL, versionPURL)
}

func (p *Proxy) fetchAndCache(ctx context.Context, ecosystem, name, version, filename, pkgPURL, versionPURL string) (*CacheResult, error) {
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

	// Fetch from upstream
	artifact, err := p.Fetcher.Fetch(ctx, info.URL)
	if err != nil {
		return nil, fmt.Errorf("fetching from upstream: %w", err)
	}

	// Store in cache
	storagePath := storage.ArtifactPath(ecosystem, "", name, version, filename)
	size, hash, err := p.Storage.Store(ctx, storagePath, artifact.Body)
	_ = artifact.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("storing artifact: %w", err)
	}

	// Update database
	if err := p.updateCacheDB(ctx, ecosystem, name, version, filename, pkgPURL, versionPURL, info.URL, storagePath, hash, size, artifact.ContentType); err != nil {
		p.Logger.Warn("failed to update cache database", "error", err)
		// Continue anyway - we have the file
	}

	// Open the stored file to return
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

func (p *Proxy) updateCacheDB(ctx context.Context, ecosystem, name, version, filename, pkgPURL, versionPURL, upstreamURL, storagePath, hash string, size int64, contentType string) error {
	now := time.Now()

	// Upsert package
	pkg := &database.Package{
		PURL:        pkgPURL,
		Ecosystem:   ecosystem,
		Name:        name,
		UpstreamURL: upstreamURL,
		MetadataFetchedAt: sql.NullTime{Time: now, Valid: true},
	}
	pkgID, err := p.DB.UpsertPackage(pkg)
	if err != nil {
		return fmt.Errorf("upserting package: %w", err)
	}

	// Upsert version
	ver := &database.Version{
		PURL:              versionPURL,
		PackageID:         pkgID,
		Version:           version,
		MetadataFetchedAt: sql.NullTime{Time: now, Valid: true},
	}
	verID, err := p.DB.UpsertVersion(ver)
	if err != nil {
		return fmt.Errorf("upserting version: %w", err)
	}

	// Upsert artifact
	art := &database.Artifact{
		VersionID:   verID,
		Filename:    filename,
		UpstreamURL: upstreamURL,
		StoragePath: sql.NullString{String: storagePath, Valid: true},
		ContentHash: sql.NullString{String: hash, Valid: true},
		Size:        sql.NullInt64{Int64: size, Valid: true},
		ContentType: sql.NullString{String: contentType, Valid: true},
		FetchedAt:   sql.NullTime{Time: now, Valid: true},
	}
	_, err = p.DB.UpsertArtifact(art)
	if err != nil {
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
	// Build PURL for lookups
	pkgPURL := fmt.Sprintf("pkg:%s/%s", ecosystem, name)
	versionPURL := fmt.Sprintf("pkg:%s/%s@%s", ecosystem, name, version)

	// Check if we have this artifact cached
	pkg, err := p.DB.GetPackageByPURL(pkgPURL)
	if err != nil {
		return nil, fmt.Errorf("checking package cache: %w", err)
	}

	if pkg != nil {
		ver, err := p.DB.GetVersionByPURL(versionPURL)
		if err != nil {
			return nil, fmt.Errorf("checking version cache: %w", err)
		}

		if ver != nil {
			artifact, err := p.DB.GetArtifact(ver.ID, filename)
			if err != nil {
				return nil, fmt.Errorf("checking artifact cache: %w", err)
			}

			if artifact != nil && artifact.IsCached() {
				reader, err := p.Storage.Open(ctx, artifact.StoragePath.String)
				if err != nil {
					p.Logger.Warn("cached artifact missing from storage, will refetch",
						"path", artifact.StoragePath.String, "error", err)
				} else {
					_ = p.DB.RecordArtifactHit(artifact.ID)
					return &CacheResult{
						Reader:      reader,
						Size:        artifact.Size.Int64,
						ContentType: artifact.ContentType.String,
						Hash:        artifact.ContentHash.String,
						Cached:      true,
					}, nil
				}
			}
		}
	}

	// Not cached, fetch from provided URL
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
