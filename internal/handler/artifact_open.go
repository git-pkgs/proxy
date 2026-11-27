package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
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
// store directly.
type ApprovedArtifact struct {
	Artifact    artifacts.Artifact
	StoragePath string

	// cached is set for an artifact found in the cache, by findStored, which
	// also fills checks. One just fetched for the request is delivered
	// without the cache's checks; see Open.
	cached    bool
	ecosystem string
	checks    integrityChecks
	// release gives up the hold on an object fetched without a record,
	// which is deleted once every holder lets go. Open moves it to the
	// reader it returns.
	release func()
}

// Release gives up an artifact Acquire returned that won't be opened. Only
// an artifact fetched but not recorded holds anything: its object is
// deleted once nobody holds it. Releasing one that was opened does nothing,
// as its reader has the hold, and so does releasing one that holds nothing.
func (a *ApprovedArtifact) Release() {
	if a.release != nil {
		a.release()
	}
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
		cached:      true,
		ecosystem:   record.Ecosystem,
		checks:      checks,
	}, nil
}

// Open delivers an approved artifact. It returns nil for a cached one that
// can't be served, so the caller fetches it again. One just fetched for the
// request skips the cache's checks: its bytes were verified as they were
// stored.
func (p *Proxy) Open(ctx context.Context, a *ApprovedArtifact) (*CacheResult, error) {
	if !a.cached {
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
//
// An artifact holding an unrecorded object hands its hold to the reader,
// which releases it on Close, so a Release after Open can't delete the object
// under the reader. The hold is released right away if the object can't be
// opened.
func (p *Proxy) openFetched(ctx context.Context, a *ApprovedArtifact) (*CacheResult, error) {
	readStart := time.Now()
	reader, err := p.Storage.Open(ctx, a.StoragePath)
	metrics.RecordStorageOperation("read", time.Since(readStart))
	if err != nil {
		metrics.RecordStorageError("read", metrics.FailureDetail{
			PURL: a.Artifact.PURL, Filename: a.Artifact.Filename, Path: a.StoragePath, Err: err})
		a.Release()
		return nil, fmt.Errorf("opening cached artifact: %w", err)
	}
	res := &CacheResult{Reader: reader, Artifact: a.Artifact}
	release := a.release
	a.release = nil
	if release == nil {
		return res, nil
	}
	r := releaseOnClose{ReadCloser: reader, release: release}
	if s, ok := reader.(io.Seeker); ok {
		res.Reader = &seekableReleaseOnClose{releaseOnClose: r, Seeker: s}
	} else {
		res.Reader = &r
	}
	return res, nil
}
