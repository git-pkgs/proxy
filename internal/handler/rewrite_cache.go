package handler

import (
	"container/list"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"slices"
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
//
// Each entry keeps the ETag of its output, so clients can revalidate their
// copy without the output being hashed again on every request.
type rewriteCache struct {
	maxBytes int64

	mu       sync.Mutex
	size     int64
	order    *list.List // most recently used at the front
	entries  map[string]*list.Element
	inFlight map[string]*inflightRewrite
}

type rewriteEntry struct {
	key  string
	out  []byte
	etag string
}

// inflightRewrite is one rewrite that concurrent callers share. out, etag and
// err are written before done closes and read only after, so the close is the
// handoff.
type inflightRewrite struct {
	done chan struct{}
	out  []byte
	etag string
	err  error
}

// processStartedAt bounds the Last-Modified of rewritten metadata from below.
// A rewrite depends on configuration read at startup as well as on the
// upstream document, so a restart can change it while upstream stays the same.
var processStartedAt = time.Now()

// rewrittenMetadata is a rewritten metadata document with the validators a
// client revalidates its copy with. lastModified is zero when unknown.
type rewrittenMetadata struct {
	body         []byte
	etag         string
	lastModified time.Time
}

// metadataETag returns the strong ETag of a rewritten document.
func metadataETag(out []byte) string {
	sum := sha256.Sum256(out)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// rewriteConfigNonce stands for the configuration a rewrite reads at startup,
// such as the denylist and cooldown rules, in ETags derived from a rewrite's
// inputs. It changes on every start, so no ETag outlives the configuration it
// was derived under.
var rewriteConfigNonce = rand.Text()

// keptVersionsETag returns the strong ETag of a rewrite from what fixes its
// output: the upstream document's digest, the proxy URL and package name its
// download URLs carry, the versions cooldown and the denylist kept, and the
// startup configuration. Hashing these costs far less than hashing the
// rewritten document. Each field is length-prefixed, so different inputs never
// hash the same bytes.
func keptVersionsETag(digest, proxyURL, name string, kept []string) string {
	h := sha256.New()
	var length []byte
	for _, field := range append([]string{rewriteConfigNonce, digest, proxyURL, name}, slices.Sorted(slices.Values(kept))...) {
		length = binary.AppendUvarint(length[:0], uint64(len(field)))
		_, _ = h.Write(length)
		_, _ = io.WriteString(h, field)
	}
	return `"` + hex.EncodeToString(h.Sum(nil)) + `"`
}

// keepingRewrite rewrites a metadata document and reports the versions it
// kept. Together with the document they fix the rewrite's output.
type keepingRewrite func([]byte) ([]byte, []string, error)

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

// get returns the cached rewrite for key and its ETag, if there is one.
func (c *rewriteCache) get(key string) ([]byte, string, bool) {
	if c == nil {
		return nil, "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return nil, "", false
	}
	c.order.MoveToFront(el)
	e := el.Value.(*rewriteEntry)
	return e.out, e.etag, true
}

// rewrite returns rewrite(in) and its ETag, from the cache when it can.
// Callers must treat the returned bytes as read-only: a cached result is
// shared. A caller waiting on another's rewrite leaves when ctx ends; the
// rewrite itself always runs to completion, so the result is cached for the
// next request.
func (c *rewriteCache) rewrite(ctx context.Context, key string, in []byte, rewrite func([]byte) ([]byte, error)) ([]byte, string, error) {
	if c == nil {
		return rewriteWithETag(in, rewrite)
	}

	c.mu.Lock()
	if el, ok := c.entries[key]; ok {
		c.order.MoveToFront(el)
		e := el.Value.(*rewriteEntry)
		c.mu.Unlock()
		return e.out, e.etag, nil
	}
	if f, ok := c.inFlight[key]; ok {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-f.done:
			return f.out, f.etag, f.err
		}
	}
	f := &inflightRewrite{done: make(chan struct{}), err: errSharedRewriteAbandoned}
	c.inFlight[key] = f
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.inFlight, key)
		if f.err == nil {
			c.add(&rewriteEntry{key: key, out: f.out, etag: f.etag})
		}
		c.mu.Unlock()
		close(f.done)
	}()
	f.out, f.etag, f.err = rewriteWithETag(in, rewrite)
	return f.out, f.etag, f.err
}

// rewriteWithETag runs rewrite and computes the ETag of what it returns.
func rewriteWithETag(in []byte, rewrite func([]byte) ([]byte, error)) ([]byte, string, error) {
	out, err := rewrite(in)
	if err != nil {
		return nil, "", err
	}
	return out, metadataETag(out), nil
}

// add stores e and evicts least recently used entries until the cache is
// back under its limit. Output larger than the whole cache is not stored.
// Called with mu held.
func (c *rewriteCache) add(e *rewriteEntry) {
	n := int64(len(e.out))
	if n > c.maxBytes {
		return
	}
	c.entries[e.key] = c.order.PushFront(e)
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
// cache and returns it with its ETag. digest identifies in when the metadata
// cache stored it, and is empty otherwise. Cooldown filtering depends on the
// current time, so with cooldown on a cached rewrite could keep hiding a
// version past its cooldown; those rewrites always run, and when digest is
// known their ETag comes from their inputs rather than from hashing each
// output again.
func (p *Proxy) cachedRewrite(ctx context.Context, ecosystem, proxyURL, name string, in []byte, digest string, rewrite keepingRewrite) ([]byte, string, error) {
	if p.cooldownEnabled() && digest != "" {
		out, kept, err := rewrite(in)
		if err != nil {
			return nil, "", err
		}
		return out, keptVersionsETag(digest, proxyURL, name, kept), nil
	}
	rewriteOnly := func(b []byte) ([]byte, error) {
		out, _, err := rewrite(b)
		return out, err
	}
	if p.rewrites == nil || p.cooldownEnabled() {
		return rewriteWithETag(in, rewriteOnly)
	}
	return p.rewrites.rewrite(ctx, rewriteCacheKey(ecosystem, proxyURL, name, in), in, rewriteOnly)
}

func (p *Proxy) cooldownEnabled() bool {
	return p.Cooldown != nil && p.Cooldown.Enabled()
}

// rewriteLastModified returns the Last-Modified of a rewrite of metadata that
// upstream last modified at upstream, or zero when there is none to give.
// With cooldown on there is none: versions leaving cooldown change the
// rewrite while upstream stays the same.
func (p *Proxy) rewriteLastModified(upstream time.Time) time.Time {
	if upstream.IsZero() || p.cooldownEnabled() {
		return time.Time{}
	}
	if upstream.Before(processStartedAt) {
		return processStartedAt
	}
	return upstream
}

// rewrittenMetadataFor returns a fresh rewrite of metadata that upstream last
// modified at upstreamModified, with its validators.
func (p *Proxy) rewrittenMetadataFor(out []byte, etag string, upstreamModified time.Time) rewrittenMetadata {
	return rewrittenMetadata{body: out, etag: etag, lastModified: p.rewriteLastModified(upstreamModified)}
}

// storedRewrite returns the cached rewrite of the metadata stored for
// ecosystem and cacheKey, when that metadata is still within its TTL and a
// rewrite of it is cached. It reads only the cache row, never the stored
// document, so a repeated request for a large packument costs a database
// lookup instead of reading and hashing the whole document again. When it
// reports false the caller takes the usual fetch and rewrite path.
func (p *Proxy) storedRewrite(ecosystem, cacheKey, proxyURL, name string) (rewrittenMetadata, bool) {
	if p.rewrites == nil || p.cooldownEnabled() {
		return rewrittenMetadata{}, false
	}
	if !p.CacheMetadata || p.DB == nil || p.MetadataTTL <= 0 {
		return rewrittenMetadata{}, false
	}
	entry, err := p.DB.GetMetadataCache(ecosystem, cacheKey)
	if err != nil || entry == nil || !entry.ContentDigest.Valid || entry.ContentEncoding.String != "" {
		return rewrittenMetadata{}, false
	}
	if !entry.FetchedAt.Valid || time.Since(entry.FetchedAt.Time) >= p.MetadataTTL {
		return rewrittenMetadata{}, false
	}
	out, etag, ok := p.rewrites.get(rewriteCacheKeyForDigest(ecosystem, proxyURL, name, entry.ContentDigest.String))
	if !ok {
		return rewrittenMetadata{}, false
	}
	metrics.RecordCacheHit(ecosystem)
	var upstreamModified time.Time
	if entry.LastModified.Valid {
		upstreamModified = entry.LastModified.Time
	}
	return rewrittenMetadata{body: out, etag: etag, lastModified: p.rewriteLastModified(upstreamModified)}, true
}

// serveRewrittenMetadata writes rewritten metadata with its validators, or
// 304 Not Modified when they show the client's copy is current.
func serveRewrittenMetadata(w http.ResponseWriter, r *http.Request, m rewrittenMetadata) {
	w.Header().Set(headerETag, m.etag)
	if !m.lastModified.IsZero() {
		w.Header().Set(headerLastModified, m.lastModified.UTC().Format(http.TimeFormat))
	}
	if notModified(r, m.etag, m.lastModified) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set(headerContentType, contentTypeJSON)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(m.body)
}

// notModified reports whether a conditional request's validators match.
// If-Modified-Since only counts when the request has no If-None-Match.
func notModified(r *http.Request, etag string, lastModified time.Time) bool {
	if header := r.Header.Get("If-None-Match"); header != "" {
		return ifNoneMatchHits(header, etag)
	}
	if lastModified.IsZero() {
		return false
	}
	since, err := http.ParseTime(r.Header.Get("If-Modified-Since"))
	return err == nil && !lastModified.Truncate(time.Second).After(since)
}

// SetMetadataRewriteCacheSize enables the cache of rewritten metadata with
// room for maxBytes of output. Zero or less disables it.
func (p *Proxy) SetMetadataRewriteCacheSize(maxBytes int64) {
	p.rewrites = newRewriteCache(maxBytes)
}
