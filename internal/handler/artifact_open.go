package handler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/git-pkgs/artifacts"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/metrics"
	"github.com/git-pkgs/proxy/internal/storage"
	"github.com/git-pkgs/purl"
)

// ApprovedArtifact is an artifact whose bytes the cache holds and may serve:
// its record and where its object is stored, without an open reader. Open
// delivers it. Finding or fetching an artifact and delivering it are kept
// apart so a caller can deliver it another way, such as from the object
// store directly (#501).
type ApprovedArtifact struct {
	Artifact    artifacts.Artifact
	StoragePath string
	// Cached is set for an artifact found in the cache. One just fetched
	// for the request is delivered without the cache's checks; see Open.
	Cached bool

	ecosystem string
	checks    integrityChecks
}

// findStored returns the artifact cached under pkgPURL, versionPURL and
// filename, or nil when none is. A record whose integrity metadata can't
// be used is discarded and reported as a miss, so the caller fetches the
// artifact again. It doesn't look at the stored object.
func (p *Proxy) findStored(pkgPURL, versionPURL, filename string) (*ApprovedArtifact, error) {
	record, err := p.DB.GetCachedArtifact(pkgPURL, versionPURL, filename)
	if err != nil {
		return nil, fmt.Errorf("checking artifact cache: %w", err)
	}
	if record == nil {
		return nil, nil
	}
	checks, err := newIntegrityChecks(record.Artifact.Digest.Encoded(), record.Integrity.String)
	if err != nil {
		p.rejectUnusableCacheRecord(record, versionPURL, filename, err)
		return nil, nil
	}
	return &ApprovedArtifact{
		Artifact:    record.Artifact,
		StoragePath: record.StoragePath,
		Cached:      true,
		ecosystem:   record.Ecosystem,
		checks:      checks,
	}, nil
}

// Open delivers an approved artifact.
//
// A cached one is delivered without opening its object for a HEAD request
// (see ServingHead), as a signed URL with DirectServe when the store can
// sign one, and otherwise as a reader that checks the bytes against the
// recorded integrity as they are read, discarding the record when they
// don't match. When its object can't be opened, or the record's integrity
// can't be checked, Open returns nil, so the caller fetches it again.
//
// One just fetched for the request is opened as stored, and failing to open
// it is an error: its bytes were verified as they were stored, moments ago.
func (p *Proxy) Open(ctx context.Context, a *ApprovedArtifact) (*CacheResult, error) {
	if !a.Cached {
		return p.openFetched(ctx, a)
	}
	result := &CacheResult{Artifact: a.Artifact, Cached: true, storagePath: a.StoragePath}

	if servingHead(ctx) {
		result.rangeable = readsSeekable(p.Storage)
		return result, nil
	}

	if p.DirectServe {
		signed, err := p.Storage.SignedURL(ctx, a.StoragePath, p.DirectServeTTL)
		if err == nil {
			result.RedirectURL = signed
			return result, nil
		}
		if !errors.Is(err, storage.ErrSignedURLUnsupported) {
			p.Logger.Warn("failed to sign storage URL, falling back to streaming",
				"path", a.StoragePath, "error", err)
		}
	}

	versionPURL, filename := a.Artifact.PURL, a.Artifact.Filename
	start := time.Now()
	reader, err := p.Storage.Open(ctx, a.StoragePath)
	metrics.RecordStorageOperation("read", time.Since(start))
	if err != nil {
		metrics.RecordStorageError("read", metrics.FailureDetail{
			PURL: versionPURL, Filename: filename, Path: a.StoragePath, Err: err})
		p.Logger.Warn("cached artifact missing from storage, will refetch",
			"path", a.StoragePath, "error", err)
		return nil, nil
	}

	result.Reader, err = a.checks.forStoredObject().wrapFailOnMismatch(reader,
		func(reason string) {
			p.Logger.Error("cached artifact failed integrity check",
				"purl", versionPURL, "filename", filename,
				"path", a.StoragePath, "reason", reason)
			metrics.RecordIntegrityFailure(purl.NormalizeEcosystem(a.ecosystem), metrics.FailureDetail{
				PURL: versionPURL, Filename: filename, Path: a.StoragePath, Err: errors.New(reason)})
			if err := p.DB.DiscardArtifact(versionPURL, filename, a.StoragePath); err != nil {
				p.Logger.Warn("failed to clear corrupt artifact from cache", "error", err)
			}
		})
	if err != nil {
		_ = reader.Close()
		p.rejectUnusableCacheRecord(&database.CachedArtifact{Ecosystem: a.ecosystem, StoragePath: a.StoragePath}, versionPURL, filename, err)
		return nil, nil
	}
	return result, nil
}

// openFetched gives one caller its own reader over just-committed bytes.
// Callers sharing a fetch cannot share a handle: it has one read position,
// so they would consume each other's bytes and the first Close would break
// the rest.
func (p *Proxy) openFetched(ctx context.Context, a *ApprovedArtifact) (*CacheResult, error) {
	readStart := time.Now()
	reader, err := p.Storage.Open(ctx, a.StoragePath)
	metrics.RecordStorageOperation("read", time.Since(readStart))
	if err != nil {
		metrics.RecordStorageError("read", metrics.FailureDetail{
			PURL: a.Artifact.PURL, Filename: a.Artifact.Filename, Path: a.StoragePath, Err: err})
		return nil, fmt.Errorf("opening cached artifact: %w", err)
	}
	return &CacheResult{Reader: reader, Artifact: a.Artifact}, nil
}
