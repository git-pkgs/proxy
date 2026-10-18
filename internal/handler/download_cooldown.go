package handler

import (
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// publishTimeLookup reads a version's publish time from upstream metadata. ok
// is false when the metadata has no usable time for the version.
type publishTimeLookup func() (published time.Time, ok bool, err error)

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
	lookup     publishTimeLookup
}

// withholdInCooldown reports whether a download is inside the cooldown
// window, and if so answers it with a 404. Filtering metadata is not enough on
// its own: lockfiles record artifact URLs, so clients reach the download path
// without reading the metadata first, and a cached artifact would be served
// even after the policy was raised.
//
// A publish time never changes, so the stored versions row is read first and
// lookup is only asked for a version the proxy has no time for yet; the time
// it finds is stored. A version with no usable publish time is allowed
// through, matching how the metadata filters treat it.
func (p *Proxy) withholdInCooldown(w http.ResponseWriter, d downloadVersion) bool {
	if !p.versionInCooldown(d) {
		return false
	}
	p.Logger.Info("cooldown: withholding "+d.ecosystem+" download",
		"name", d.policyName, "version", d.version)
	http.Error(w, ErrVersionNotFound.Error(), http.StatusNotFound)
	return true
}

func (p *Proxy) versionInCooldown(d downloadVersion) bool {
	if p.Cooldown == nil || !p.Cooldown.Enabled() {
		return false
	}
	policyKey := canonicalPackagePURL(d.ecosystem, d.policyName)
	if p.Cooldown.For(d.ecosystem, policyKey) <= 0 {
		return false
	}

	var versionPURL string
	if d.storedName != "" {
		versionPURL = canonicalVersionPURL(d.ecosystem, d.storedName, d.version)
		if ver, err := p.DB.GetVersionByPURL(versionPURL); err == nil && ver != nil && ver.PublishedAt.Valid {
			return !p.Cooldown.IsAllowed(d.ecosystem, policyKey, ver.PublishedAt.Time)
		}
	}

	published, ok, err := d.lookup()
	if err != nil {
		p.Logger.Warn("cooldown: could not read publish time for download check",
			"ecosystem", d.ecosystem, "name", d.policyName, "version", d.version, "error", err)
		return false
	}
	if !ok {
		return false
	}

	if versionPURL != "" {
		if err := p.DB.SetVersionPublishedAt(versionPURL, canonicalPackagePURL(d.ecosystem, d.storedName), published); err != nil {
			p.Logger.Warn("cooldown: could not store publish time",
				"ecosystem", d.ecosystem, "name", d.policyName, "version", d.version, "error", err)
		}
	}

	return !p.Cooldown.IsAllowed(d.ecosystem, policyKey, published)
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
	digest string
	index  any
}

// publishIndex returns build's index of res, reusing the one built for the
// same document under key. res.digest identifies the document; without one,
// as when the metadata cache is off, the index is built for this call alone.
func publishIndex[V any](p *Proxy, key string, res metadataResult, build func([]byte, string) (map[string]V, error)) (map[string]V, error) {
	c := &p.publishIndexes
	if res.digest == "" {
		return build(res.body, res.contentEncoding)
	}
	c.mu.Lock()
	if e, ok := c.entries[key]; ok && e.digest == res.digest {
		c.mu.Unlock()
		return e.index.(map[string]V), nil
	}
	c.mu.Unlock()

	v, err, _ := c.group.Do(key+"\x00"+res.digest, func() (any, error) {
		index, err := build(res.body, res.contentEncoding)
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
		c.entries[key] = publishIndexEntry{digest: res.digest, index: index}
		return index, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(map[string]V), nil
}
