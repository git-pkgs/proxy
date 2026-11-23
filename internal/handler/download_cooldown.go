package handler

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// publishTimeLookup reads a version's publish time from upstream metadata. ok
// is false when the metadata has no usable time for the version. withholdUnknown
// marks a listed version missing from stale publish-time metadata.
type publishTimeLookup func() (published time.Time, ok, withholdUnknown bool, err error)

// downloadVersion names the version an artifact download is for, so its
// cooldown can be checked before the artifact is served.
type downloadVersion struct {
	ecosystem string
	// policyName is the package name the cooldown policy is keyed by,
	// canonicalPackagePURL(ecosystem, policyName).
	policyName string
	// storedName and version name the versions row the publish time is kept
	// in, the same names the artifact is cached under. An empty storedName
	// keeps no time, and every check asks lookup.
	storedName string
	version    string
	cacheName  string
	filename   string
	lookup     publishTimeLookup
}

// withholdInCooldown reports whether a download is inside the cooldown
// window, or a lookup failure prevents an uncached download.
func (p *Proxy) withholdInCooldown(w http.ResponseWriter, r *http.Request, d downloadVersion) bool {
	withheld, err := p.versionInCooldown(d)
	if err != nil {
		if !p.serveCachedDespiteCooldownError(w, r, d.ecosystem, d.cacheName, d.version, d.filename, err) {
			p.writeCooldownLookupError(w, d.ecosystem, d.policyName, err)
		}
		return true
	}
	if !withheld {
		return false
	}
	p.Logger.Info("cooldown: withholding "+d.ecosystem+" download",
		"name", d.policyName, "version", d.version)
	http.Error(w, ErrVersionNotFound.Error(), http.StatusNotFound)
	return true
}

func (p *Proxy) versionInCooldown(d downloadVersion) (bool, error) {
	policyKey := canonicalPackagePURL(d.ecosystem, d.policyName)
	if !p.cooldownApplies(d.ecosystem, d.policyName) {
		return false, nil
	}

	var versionPURL string
	if d.storedName != "" {
		versionPURL = canonicalVersionPURL(d.ecosystem, d.storedName, d.version)
		if ver, err := p.DB.GetVersionByPURL(versionPURL); err == nil && ver != nil && ver.PublishedAt.Valid {
			return !p.cooldown().IsAllowed(d.ecosystem, policyKey, ver.PublishedAt.Time), nil
		}
	}

	published, ok, withholdUnknown, err := d.lookup()
	if err != nil {
		return false, err
	}
	if !ok {
		return withholdUnknown, nil
	}

	if versionPURL != "" {
		if err := p.DB.SetVersionPublishedAt(versionPURL, canonicalPackagePURL(d.ecosystem, d.storedName), published); err != nil {
			p.Logger.Warn("cooldown: could not store publish time",
				"ecosystem", d.ecosystem, "name", d.policyName, "version", d.version, "error", err)
		}
	}

	return !p.cooldown().IsAllowed(d.ecosystem, policyKey, published), nil
}

// maxPublishIndexes bounds how many documents publishIndexCache keeps an
// index for.
const maxPublishIndexes = 64

// publishIndexCache keeps, for each large metadata document, the publish
// times a download check reads from it, so a document is decompressed and
// parsed once per version from upstream rather than on every download.
// Concurrent checks for a document not indexed yet share one parse.
type publishIndexCache struct {
	mu      sync.Mutex
	entries map[string]publishIndexEntry
	group   singleflight.Group
}

type publishIndexEntry struct {
	digest  string
	index   any
	expires time.Time
	// refetched is set for an index built by a refetch for a version the
	// document didn't list.
	refetched bool
}

// publishIndex reuses parsed documents by digest, or for a minute without one.
func publishIndex[V any](p *Proxy, key string, load func() metadataResult, build func(metadataResult) (V, error)) (V, error) { //nolint:ireturn // Supports timestamp maps and Helm lookups.
	c := &p.sharedProxy().publishIndexes
	fresh := func() (V, bool) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if e, ok := c.entries[key]; ok && !e.expires.IsZero() && p.clock().Before(e.expires) {
			return e.index.(V), true
		}
		var zero V
		return zero, false
	}
	if index, ok := fresh(); ok {
		return index, nil
	}
	v, err, _ := c.group.Do(key, func() (any, error) {
		if index, ok := fresh(); ok {
			return index, nil
		}
		res := load()
		if index, ok := keptPublishIndex(p, key, res); ok {
			return index, nil
		}
		index, err := build(res)
		if err != nil {
			return nil, err
		}
		storePublishIndex(p, key, res, index, false)
		return index, nil
	})
	if err != nil {
		var zero V
		return zero, err
	}
	return v.(V), nil
}

// listedTime is a version's publish time as a metadata document gives it.
// ok is false when the document lists the version without a usable time.
type listedTime struct {
	published time.Time
	ok        bool
}

// keptPublishIndex returns the index kept for key when it was built from the
// bytes res read.
func keptPublishIndex(p *Proxy, key string, res metadataResult) (any, bool) {
	if res.err != nil || res.digest == "" {
		return nil, false
	}
	c := &p.sharedProxy().publishIndexes
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || entry.digest != res.digest {
		return nil, false
	}
	return entry.index, true
}

// recentRefetch returns the index kept for key when it was built by a
// refetch and is within its keep, so a version it doesn't list was missing
// upstream less than a minute ago.
func recentRefetch(p *Proxy, key string) (any, bool) {
	c := &p.sharedProxy().publishIndexes
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || !entry.refetched || !p.clock().Before(entry.expires) {
		return nil, false
	}
	return entry.index, true
}

// storePublishIndex keeps index, built from res, as publishIndex's entry for
// key, replacing one built from older bytes. refetched marks an index built
// by a refetch.
func storePublishIndex(p *Proxy, key string, res metadataResult, index any, refetched bool) {
	c := &p.sharedProxy().publishIndexes
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]publishIndexEntry)
	}
	if _, ok := c.entries[key]; !ok && len(c.entries) >= maxPublishIndexes {
		for evict := range c.entries {
			delete(c.entries, evict)
			break
		}
	}
	entry := publishIndexEntry{digest: res.digest, index: index, refetched: refetched}
	if res.digest == "" || !p.CacheMetadata {
		entry.expires = p.clock().Add(time.Minute)
	}
	c.entries[key] = entry
}

// listingPublishIndex is publishIndex for a download that lists checks.
// An index built from a copy cached within metadata_ttl, or served stale,
// can predate the download; when lists reports it missing, the document is
// fetched again (refetch) and the index built from what upstream lists now.
// Downloads that find it missing from the same index share one refetch, and
// for a minute after it later ones use the refetched index.
func listingPublishIndex[V any](p *Proxy, key string, load, refetch func() metadataResult, build func(metadataResult) (V, error), lists func(V) bool) (V, error) { //nolint:ireturn // V is what build returns.
	// loaded is what the index was built from, unless an index built
	// earlier was reused.
	var loaded *metadataResult
	index, err := publishIndex(p, key, func() metadataResult {
		res := load()
		loaded = &res
		return res
	}, build)
	if err != nil || lists(index) || loaded != nil && !loaded.fromCache && !loaded.stale {
		return index, err
	}
	// A concurrent refetch may have replaced the index read above.
	if recent, ok := recentRefetch(p, key); ok {
		return recent.(V), nil
	}
	refetched, err, _ := p.sharedProxy().publishIndexes.group.Do(key+"\x00refetch", func() (any, error) {
		res := refetch()
		if index, ok := keptPublishIndex(p, key, res); ok {
			return index, nil
		}
		index, err := build(res)
		if err != nil {
			return nil, err
		}
		storePublishIndex(p, key, res, index, true)
		return index, nil
	})
	if err != nil {
		var zero V
		return zero, err
	}
	return refetched.(V), nil
}

// readListed reads a version's entry from a metadata document fetched
// through the metadata cache. read reports whether the document lists the
// version at all. A copy cached within metadata_ttl, or served stale, can
// predate the version, and a download of it would find no publish time and
// be let through cooldown; such a copy is fetched again and the version read
// from what upstream lists now. If it can't be fetched, the lookup fails, as
// when a publish time can't be read.
func readListed[V any](ctx context.Context, p *Proxy, ecosystem, cacheKey, upstreamURL, accept, acceptEncoding string, read func(metadataResult) (V, bool, error)) (V, error) { //nolint:ireturn // V is what read returns.
	res := p.lookupMetadata(ctx, ecosystem, cacheKey, upstreamURL, accept, acceptEncoding, nil)
	if res.err != nil {
		var zero V
		return zero, res.err
	}
	value, listed, err := read(res)
	if err != nil || listed || !res.fromCache && !res.stale {
		return value, err
	}
	res = p.refetchLacking(ctx, ecosystem, cacheKey, upstreamURL, accept, acceptEncoding, nil)
	if res.err != nil {
		var zero V
		return zero, res.err
	}
	value, _, err = read(res)
	return value, err
}

// refetchLacking fetches a cached metadata document again from upstream,
// for a download of a version the cached copy doesn't list. A stale copy is
// not upstream's answer, so it is an error.
func (p *Proxy) refetchLacking(ctx context.Context, ecosystem, cacheKey, upstreamURL, accept, acceptEncoding string, validate func([]byte) error) metadataResult {
	p.Logger.Info("cooldown: cached metadata lacks the version, fetching it again",
		"ecosystem", ecosystem, "url", upstreamURL)
	res := p.refreshMetadata(ctx, ecosystem, cacheKey, upstreamURL, accept, acceptEncoding, validate)
	if res.err == nil && res.stale {
		res.err = fmt.Errorf("%s metadata could not be fetched again: %w", ecosystem, res.upstreamErr)
	}
	return res
}
