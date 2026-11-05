package handler

import (
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
}

// publishIndex reuses parsed documents by digest, or for a minute without one.
func publishIndex[V any](p *Proxy, key string, load func() metadataResult, build func(metadataResult) (V, error)) (V, error) { //nolint:ireturn // Supports timestamp maps and Helm lookups.
	c := &p.publishIndexes
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
		if res.err == nil && res.digest != "" {
			c.mu.Lock()
			entry, ok := c.entries[key]
			c.mu.Unlock()
			if ok && entry.digest == res.digest {
				return entry.index, nil
			}
		}
		index, err := build(res)
		if err != nil {
			return nil, err
		}
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
		entry := publishIndexEntry{digest: res.digest, index: index}
		if res.digest == "" || !p.CacheMetadata {
			entry.expires = p.clock().Add(time.Minute)
		}
		c.entries[key] = entry
		return index, nil
	})
	if err != nil {
		var zero V
		return zero, err
	}
	return v.(V), nil
}
