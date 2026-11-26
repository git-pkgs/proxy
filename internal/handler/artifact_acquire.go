package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/git-pkgs/artifacts"
	"github.com/git-pkgs/proxy/internal/metrics"
	"github.com/git-pkgs/registries/fetch"
)

// ErrArtifactsStreamed is returned by Acquire with StreamArtifacts set: an
// artifact is then streamed from upstream and never stored, so there is no
// stored object to approve. StreamAcquire serves it.
var ErrArtifactsStreamed = errors.New("artifacts are streamed, not stored")

// ArtifactRequest names an artifact and how a cache miss fetches it.
type ArtifactRequest struct {
	Ecosystem, Name, Version string
	// Filename is the artifact's name in the cache. Without a DownloadURL it
	// may be empty, and is then resolved with the download URL.
	Filename string
	// DownloadURL is where a cache miss fetches the artifact. Without it,
	// the ecosystem's resolver finds the URL (and the routed upstream's, see
	// Proxy.RouteFor).
	DownloadURL string
	// Headers are sent with the fetch from DownloadURL.
	Headers http.Header
	// Digest, an OCI-style digest such as "sha256:<hex>", is checked against
	// a newly fetched response before it is served or cached; see
	// GetOrFetchArtifactFromURLWithDigest. A cached artifact whose sha256
	// differs is fetched again.
	Digest string
	// Integrity is the integrity the registry's metadata declares, checked
	// against a newly fetched response and recorded for cache hits; see
	// GetOrFetchArtifactFromURLWithIntegrity. Digest, when set, takes its
	// place.
	Integrity string

	// upstreamHash is the hex sha256 the upstream declares, which a cached
	// artifact has to match. Digest sets it.
	upstreamHash string
	// packagePURL and versionPURL key the cache instead of the PURLs made
	// from Ecosystem, Name and Version, for protocols such as Swift.
	packagePURL, versionPURL string
}

// preparedRequest is an ArtifactRequest with its cache keys and checks
// worked out, and its filename resolved.
type preparedRequest struct {
	ArtifactRequest
	pkgPURL, versionPURL string
	declared             string
	// info is the resolved download, for a request without a DownloadURL.
	info *fetch.ArtifactInfo
}

// prepare refuses a denied version and works out the request's cache keys,
// checks and filename.
func (p *Proxy) prepare(ctx context.Context, req ArtifactRequest) (*preparedRequest, error) {
	r := &preparedRequest{ArtifactRequest: req, declared: req.Integrity}
	if req.Digest != "" {
		upstreamHash, declared, err := digestChecks(req.Digest)
		if err != nil {
			return nil, err
		}
		r.upstreamHash, r.declared = upstreamHash, declared
	}
	if req.DownloadURL == "" {
		return r, p.prepareResolved(ctx, r)
	}

	r.pkgPURL, r.versionPURL = req.packagePURL, req.versionPURL
	if r.versionPURL == "" {
		var err error
		if r.pkgPURL, r.versionPURL, err = packagePURLStrings(req.Ecosystem, req.Name, req.Version); err != nil {
			return nil, err
		}
	}
	// Some protocols use qualified cache keys. Check their package identity
	// too, and the cache key itself for those, such as Swift, whose name has
	// no PURL of its own.
	if p.versionDenied(req.Ecosystem, req.Name, req.Version) {
		return nil, fmt.Errorf("%w: %s", ErrVersionDenied, canonicalVersionPURL(req.Ecosystem, req.Name, req.Version))
	}
	if p.Denylist.Denied(r.versionPURL) {
		return nil, fmt.Errorf("%w: %s", ErrVersionDenied, r.versionPURL)
	}
	return r, nil
}

// prepareResolved prepares a request whose download the ecosystem's
// resolver finds.
func (p *Proxy) prepareResolved(ctx context.Context, r *preparedRequest) error {
	// NuGet ids are case-insensitive and the NuGet handler stores them
	// lowercased, so a mirrored Newtonsoft.Json must land on the same record
	// clients are served.
	if r.Ecosystem == "nuget" {
		r.Name = strings.ToLower(r.Name)
	}
	var err error
	if r.pkgPURL, r.versionPURL, err = packagePURLStrings(r.Ecosystem, r.Name, r.Version); err != nil {
		return err
	}
	// Deny before resolution as well as before reading the cache. Mirror callers
	// do not supply a filename, and resolving it can contact an upstream registry.
	if p.Denylist.Denied(r.versionPURL) {
		return fmt.Errorf("%w: %s", ErrVersionDenied, r.versionPURL)
	}
	if r.Filename != "" {
		return nil
	}
	if r.info, err = p.resolveArtifact(ctx, r.Ecosystem, r.Name, r.Version); err != nil {
		return err
	}
	if r.Filename = r.info.Filename; r.Filename == "" {
		return errors.New("resolved artifact has no filename")
	}
	if route := p.routeFor(r.Ecosystem, r.Name); route != nil {
		r.Filename = route.FilenamePrefix + r.Filename
	}
	return nil
}

// stream streams the artifact from upstream without storing it, for
// StreamArtifacts.
func (p *Proxy) stream(ctx context.Context, r *preparedRequest) (*CacheResult, error) {
	if r.DownloadURL != "" {
		return p.streamFromUpstream(ctx, r.Ecosystem, r.Name, r.Version, r.Filename, r.versionPURL, r.DownloadURL, r.upstreamHash, r.declared,
			func(fetchCtx context.Context) (*fetch.Artifact, error) {
				return p.fetchUpstream(fetchCtx, r.DownloadURL, r.Headers)
			})
	}
	if r.info == nil {
		var err error
		if r.info, err = p.resolveArtifact(ctx, r.Ecosystem, r.Name, r.Version); err != nil {
			return nil, err
		}
	}
	return p.streamFromUpstream(ctx, r.Ecosystem, r.Name, r.Version, r.Filename, r.versionPURL, r.info.URL, "", "",
		func(fetchCtx context.Context) (*fetch.Artifact, error) {
			return p.Fetcher.Fetch(fetchCtx, r.info.URL)
		})
}

// fetchKey returns the coalescing key of the request's fetch, the recheck
// run under it, and the fetch.
func (p *Proxy) fetchKey(r *preparedRequest) (string, func() (artifacts.Artifact, string, bool), func(context.Context) (artifacts.Artifact, string, error)) {
	if r.DownloadURL == "" {
		return artifactCoalesceKey(r.versionPURL, r.Filename, "", "", ""),
			func() (artifacts.Artifact, string, bool) {
				return p.cachedArtifactRecord(r.pkgPURL, r.versionPURL, r.Filename, "")
			},
			func(fetchCtx context.Context) (artifacts.Artifact, string, error) {
				return p.fetchAndCache(fetchCtx, r.Ecosystem, r.Name, r.Version, r.Filename, r.pkgPURL, r.versionPURL, r.info)
			}
	}
	// The caller running the fetch discards a stale entry under the key,
	// where it cannot delete a fetch that just replaced it.
	return artifactCoalesceKey(r.versionPURL, r.Filename, r.DownloadURL, r.upstreamHash, r.declared),
		func() (artifacts.Artifact, string, bool) {
			return p.cachedArtifactRecord(r.pkgPURL, r.versionPURL, r.Filename, r.upstreamHash)
		},
		func(fetchCtx context.Context) (artifacts.Artifact, string, error) {
			p.discardStaleArtifact(r.pkgPURL, r.versionPURL, r.Filename, r.upstreamHash)
			return p.fetchAndCacheFromURL(fetchCtx, r.Ecosystem, r.Name, r.Version, r.Filename, r.pkgPURL, r.versionPURL, r.DownloadURL, r.Headers, r.upstreamHash, r.declared)
		}
}

// StreamAcquire serves an artifact from the cache, or fetches it, stores it
// and serves it, sharing the fetch with concurrent callers. Unlike Acquire it
// may deliver the bytes as they arrive: with StreamArtifacts set they are
// streamed from upstream and never stored, and a context marked with
// StreamCacheMiss is sent a cache miss while it is stored. A cache hit is
// delivered as Open delivers it.
func (p *Proxy) StreamAcquire(ctx context.Context, req ArtifactRequest) (*CacheResult, error) {
	r, err := p.prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	if p.StreamArtifacts {
		return p.stream(ctx, r)
	}
	if cached, err := p.getCachedArtifactWithUpstreamHash(ctx, r.pkgPURL, r.versionPURL, r.Filename, r.upstreamHash); err != nil || cached != nil {
		return cached, err
	}
	metrics.RecordArtifactCacheMiss(r.Ecosystem)
	key, recheck, commit := p.fetchKey(r)
	return p.coalesceFetch(ctx, key, recheck, commit)
}

// Acquire returns an artifact the cache holds, fetching and storing it on a
// cache miss, without delivering it: nothing is opened, and Open delivers
// it. The fetch is shared with concurrent callers and is verified and, with
// scanning on, scanned before it is stored, so what Acquire returns has
// been approved. Acquire never streams; with StreamArtifacts set it returns
// ErrArtifactsStreamed.
//
// A cached artifact is checked to still have its object, which costs a
// HEAD on S3 or GCS rather than the GET opening it would; one whose object
// is gone is fetched again. An artifact fetched now but not recorded must
// be opened or released; see ApprovedArtifact.Release.
func (p *Proxy) Acquire(ctx context.Context, req ArtifactRequest) (*ApprovedArtifact, error) {
	if p.StreamArtifacts {
		return nil, ErrArtifactsStreamed
	}
	r, err := p.prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	if a, err := p.findCached(ctx, r.pkgPURL, r.versionPURL, r.Filename, r.upstreamHash); err != nil || a != nil {
		if a != nil {
			p.recordCacheHit(ctx, a.ecosystem, r.versionPURL, r.Filename)
		}
		return a, err
	}
	metrics.RecordArtifactCacheMiss(r.Ecosystem)
	key, recheck, commit := p.fetchKey(r)
	got, err := p.coalesceAcquire(ctx, key, recheck, commit)
	if err != nil {
		return nil, err
	}
	if got.opened != nil && got.opened.Reader != nil {
		_ = got.opened.Reader.Close()
	}
	a := &ApprovedArtifact{Artifact: got.fetched.artifact, StoragePath: got.fetched.storagePath}
	if got.unrecorded != nil {
		a.release = sync.OnceFunc(got.unrecorded.release)
	}
	return a, nil
}

// findCached is findStored for Acquire: it moves a legacy Go record to its
// current key as a cache lookup does, reports a pushed artifact whose object
// is gone as an error rather than a miss, and treats a record whose digest
// disagrees with upstreamHash, or whose object is gone, as a miss.
func (p *Proxy) findCached(ctx context.Context, pkgPURL, versionPURL, filename, upstreamHash string) (*ApprovedArtifact, error) {
	a, err := p.findStored(pkgPURL, versionPURL, filename)
	if err == nil && a == nil {
		var moved bool
		if moved, err = p.rekeyLegacyGoArtifact(pkgPURL, versionPURL, filename); err == nil && moved {
			a, err = p.findStored(pkgPURL, versionPURL, filename)
		}
	}
	if err != nil {
		return nil, err
	}
	if a != nil && artifactHashMatches(a.Artifact.Digest.Encoded(), upstreamHash) {
		exists, err := p.Storage.Exists(ctx, a.StoragePath)
		if err != nil {
			return nil, fmt.Errorf("checking stored artifact: %w", err)
		}
		if exists {
			return a, nil
		}
		p.Logger.Warn("cached artifact missing from storage, will refetch", "path", a.StoragePath)
	} else if a != nil {
		return nil, nil
	}
	return nil, p.localArtifactMiss(versionPURL, filename)
}
