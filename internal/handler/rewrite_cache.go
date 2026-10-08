package handler

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/git-pkgs/proxy/internal/metrics"
)

// rewriteCache keeps metadata documents after a handler has rewritten them.
// Rewriting means decoding the whole document into generic maps, changing its
// download URLs and encoding it again, which for a large npm packument or an
// expanded Composer document costs milliseconds to hundreds of milliseconds
// and megabytes of allocations. It ran on every request, cached metadata
// included. The cache keeps one rewrite per distinct upstream document, and
// concurrent requests for a document that is not cached yet share a single
// rewrite.
//
// Entries are keyed by a hash of the raw document, so new bytes from upstream
// are rewritten again rather than served stale. Everything else a rewrite
// depends on is fixed for the life of the process: the proxy URL is part of
// the key, and the denylist is loaded at startup.
type rewriteCache struct {
	maxBytes int64

	mu       sync.Mutex
	size     int64
	order    *list.List // most recently used at the front
	entries  map[string]*list.Element
	inFlight map[string]*inflightRewrite
}

type rewriteEntry struct {
	key string
	out []byte
}

// inflightRewrite is one rewrite that concurrent callers share. out and err
// are written before done closes and read only after, so the close is the
// handoff.
type inflightRewrite struct {
	done chan struct{}
	out  []byte
	err  error
}

// errSharedRewriteAbandoned is what waiters see if the caller running a
// shared rewrite panicked out of it.
var errSharedRewriteAbandoned = errors.New("shared metadata rewrite did not complete")

// newRewriteCache returns a cache holding up to maxBytes of rewritten output.
// A size of zero or less disables it.
func newRewriteCache(maxBytes int64) *rewriteCache {
	if maxBytes <= 0 {
		return nil
	}
	return &rewriteCache{
		maxBytes: maxBytes,
		order:    list.New(),
		entries:  make(map[string]*list.Element),
		inFlight: make(map[string]*inflightRewrite),
	}
}

// rewriteCacheKey identifies one rewrite: the ecosystem and proxy URL the
// handler rewrites for, the package, and the exact upstream bytes.
func rewriteCacheKey(ecosystem, proxyURL, name string, in []byte) string {
	sum := sha256.Sum256(in)
	return rewriteCacheKeyForDigest(ecosystem, proxyURL, name, "sha256:"+hex.EncodeToString(sum[:]))
}

// rewriteCacheKeyForDigest is rewriteCacheKey for upstream bytes known only by
// their digest, in the form the metadata cache records it.
func rewriteCacheKeyForDigest(ecosystem, proxyURL, name, digest string) string {
	return strings.Join([]string{ecosystem, proxyURL, name, digest}, "\x00")
}

// get returns the cached rewrite for key, if there is one.
func (c *rewriteCache) get(key string) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*rewriteEntry).out, true
}

// rewrite returns rewrite(in), from the cache when it can. Callers must treat
// the returned bytes as read-only: a cached result is shared. A caller waiting
// on another's rewrite leaves when ctx ends; the rewrite itself always runs
// to completion, so the result is cached for the next request.
func (c *rewriteCache) rewrite(ctx context.Context, key string, in []byte, rewrite func([]byte) ([]byte, error)) ([]byte, error) {
	if c == nil {
		return rewrite(in)
	}

	c.mu.Lock()
	if el, ok := c.entries[key]; ok {
		c.order.MoveToFront(el)
		out := el.Value.(*rewriteEntry).out
		c.mu.Unlock()
		return out, nil
	}
	if f, ok := c.inFlight[key]; ok {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-f.done:
			return f.out, f.err
		}
	}
	f := &inflightRewrite{done: make(chan struct{}), err: errSharedRewriteAbandoned}
	c.inFlight[key] = f
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.inFlight, key)
		if f.err == nil {
			c.add(key, f.out)
		}
		c.mu.Unlock()
		close(f.done)
	}()
	f.out, f.err = rewrite(in)
	return f.out, f.err
}

// add stores out under key and evicts least recently used entries until the
// cache is back under its limit. Output larger than the whole cache is not
// stored. Called with mu held.
func (c *rewriteCache) add(key string, out []byte) {
	n := int64(len(out))
	if n > c.maxBytes {
		return
	}
	c.entries[key] = c.order.PushFront(&rewriteEntry{key: key, out: out})
	c.size += n
	for c.size > c.maxBytes {
		oldest := c.order.Back()
		e := oldest.Value.(*rewriteEntry)
		c.order.Remove(oldest)
		delete(c.entries, e.key)
		c.size -= int64(len(e.out))
	}
}

// len reports how many rewrites are cached.
func (c *rewriteCache) len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// cachedRewrite rewrites a metadata document through the proxy's rewrite
// cache. Cooldown filtering depends on the current time, so with cooldown on
// a cached rewrite could keep hiding a version past its cooldown; those
// rewrites always run.
func (p *Proxy) cachedRewrite(ctx context.Context, ecosystem, proxyURL, name string, in []byte, rewrite func([]byte) ([]byte, error)) ([]byte, error) {
	if p.rewrites == nil || (p.Cooldown != nil && p.Cooldown.Enabled()) {
		return rewrite(in)
	}
	return p.rewrites.rewrite(ctx, rewriteCacheKey(ecosystem, proxyURL, name, in), in, rewrite)
}

// storedRewrite returns the cached rewrite of the metadata stored for
// ecosystem and cacheKey, when that metadata is still within its TTL and a
// rewrite of it is cached. It reads only the cache row, never the stored
// document, so a repeated request for a large packument costs a database
// lookup instead of reading and hashing the whole document again. When it
// reports false the caller takes the usual fetch and rewrite path.
func (p *Proxy) storedRewrite(ecosystem, cacheKey, proxyURL, name string) ([]byte, bool) {
	if p.rewrites == nil || (p.Cooldown != nil && p.Cooldown.Enabled()) {
		return nil, false
	}
	if !p.CacheMetadata || p.DB == nil || p.MetadataTTL <= 0 {
		return nil, false
	}
	entry, err := p.DB.GetMetadataCache(ecosystem, cacheKey)
	if err != nil || entry == nil || !entry.ContentDigest.Valid || entry.ContentEncoding.String != "" {
		return nil, false
	}
	if !entry.FetchedAt.Valid || time.Since(entry.FetchedAt.Time) >= p.MetadataTTL {
		return nil, false
	}
	out, ok := p.rewrites.get(rewriteCacheKeyForDigest(ecosystem, proxyURL, name, entry.ContentDigest.String))
	if ok {
		metrics.RecordCacheHit(ecosystem)
	}
	return out, ok
}

// SetMetadataRewriteCacheSize enables the cache of rewritten metadata with
// room for maxBytes of output. Zero or less disables it.
func (p *Proxy) SetMetadataRewriteCacheSize(maxBytes int64) {
	p.rewrites = newRewriteCache(maxBytes)
}
