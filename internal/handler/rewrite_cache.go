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
// Each entry keeps the validators of its output, so clients can revalidate
// their copy without the output being hashed again on every request.
type rewriteCache struct {
	maxBytes int64
	now      func() time.Time

	mu       sync.Mutex
	size     int64
	order    *list.List // most recently used at the front
	entries  map[string]*list.Element
	inFlight map[string]*inflightRewrite
	// issued holds the Last-Modified last given to a rewrite of each package,
	// keyed by its cache key without the digest, until the clock passes it.
	issued map[string]time.Time
}

type rewriteEntry struct {
	key string
	out []byte
	metadataValidators
}

// inflightRewrite is one rewrite that concurrent callers share. out, v and
// err are written before done closes and read only after, so the close is the
// handoff.
type inflightRewrite struct {
	done chan struct{}
	out  []byte
	v    metadataValidators
	err  error
}

// metadataValidators are what a client revalidates its copy of rewritten
// metadata with. lastModified is zero when there is none to give.
type metadataValidators struct {
	etag         string
	lastModified time.Time
}

// rewrittenMetadata is a rewritten metadata document with its validators.
type rewrittenMetadata struct {
	body []byte
	metadataValidators
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
		now:      time.Now,
		order:    list.New(),
		entries:  make(map[string]*list.Element),
		inFlight: make(map[string]*inflightRewrite),
		issued:   make(map[string]time.Time),
	}
}

// rewriteCacheKey identifies one rewrite: the ecosystem and proxy URL the
// handler rewrites for, the package, and the exact upstream bytes.
func rewriteCacheKey(ecosystem, proxyURL, name string, in []byte) string {
	sum := sha256.Sum256(in)
	return rewriteCacheKeyForDigest(ecosystem, proxyURL, name, "sha256:"+hex.EncodeToString(sum[:]))
}

// rewriteCacheKeyForDigest is rewriteCacheKey for upstream bytes known only by
// their digest, in the form the metadata cache records it. The digest comes
// last, so the key without it names the package the rewrite is for.
func rewriteCacheKeyForDigest(ecosystem, proxyURL, name, digest string) string {
	return strings.Join([]string{ecosystem, proxyURL, name, digest}, "\x00")
}

// get returns the cached rewrite for key and its validators, if there is one.
func (c *rewriteCache) get(key string) ([]byte, metadataValidators, bool) {
	if c == nil {
		return nil, metadataValidators{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return nil, metadataValidators{}, false
	}
	c.order.MoveToFront(el)
	e := el.Value.(*rewriteEntry)
	return e.out, e.metadataValidators, true
}

// rewrite returns rewrite(in) and its validators, from the cache when it can.
// Callers must treat the returned bytes as read-only: a cached result is
// shared. A caller waiting on another's rewrite leaves when ctx ends; the
// rewrite itself always runs to completion, so the result is cached for the
// next request.
func (c *rewriteCache) rewrite(ctx context.Context, key string, in []byte, rewrite func([]byte) ([]byte, error)) (out []byte, v metadataValidators, err error) {
	if c == nil {
		return rewriteWithETag(in, rewrite)
	}

	c.mu.Lock()
	if el, ok := c.entries[key]; ok {
		c.order.MoveToFront(el)
		e := el.Value.(*rewriteEntry)
		c.mu.Unlock()
		return e.out, e.metadataValidators, nil
	}
	if f, ok := c.inFlight[key]; ok {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, metadataValidators{}, ctx.Err()
		case <-f.done:
			return f.out, f.v, f.err
		}
	}
	f := &inflightRewrite{done: make(chan struct{}), err: errSharedRewriteAbandoned}
	c.inFlight[key] = f
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.inFlight, key)
		if f.err == nil {
			f.v.lastModified = c.issueLastModified(key)
			c.add(&rewriteEntry{key: key, out: f.out, metadataValidators: f.v})
		}
		c.mu.Unlock()
		close(f.done)
		out, v, err = f.out, f.v, f.err
	}()
	f.out, f.v, f.err = rewriteWithETag(in, rewrite)
	return f.out, f.v, f.err
}

// issueLastModified returns the Last-Modified of a rewrite that has just been
// made for key: the current second, or a second after the date last given to
// a rewrite of the same package when that is no earlier. Upstream's own date
// cannot serve, since the rewrite also depends on configuration read at
// startup, and two documents can share a second. A strictly later date for
// every new rewrite means a client holding one rewrite never gets 304 for
// another, even after its entry was evicted. Dates are remembered only until
// the clock passes them; after that the clock alone keeps them increasing.
// Called with mu held.
func (c *rewriteCache) issueLastModified(key string) time.Time {
	now := c.now().Truncate(time.Second)
	for pkg, issued := range c.issued {
		if issued.Before(now) {
			delete(c.issued, pkg)
		}
	}
	pkg := key[:strings.LastIndexByte(key, 0)]
	lastModified := now
	if issued, ok := c.issued[pkg]; ok && !issued.Before(now) {
		lastModified = issued.Add(time.Second)
	}
	c.issued[pkg] = lastModified
	return lastModified
}

// rewriteWithETag runs rewrite and computes the ETag of what it returns. It
// gives no Last-Modified: nothing records when an uncached rewrite was made.
func rewriteWithETag(in []byte, rewrite func([]byte) ([]byte, error)) ([]byte, metadataValidators, error) {
	out, err := rewrite(in)
	if err != nil {
		return nil, metadataValidators{}, err
	}
	return out, metadataValidators{etag: metadataETag(out)}, nil
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
// cache and returns it with its validators. digest identifies in when the
// metadata cache stored it, and is empty otherwise. Cooldown filtering depends
// on the current time, so with cooldown on a cached rewrite could keep hiding
// a version past its cooldown; those rewrites always run, with no
// Last-Modified, and when digest is known their ETag comes from their inputs
// rather than from hashing each output again.
func (p *Proxy) cachedRewrite(ctx context.Context, ecosystem, proxyURL, name string, in []byte, digest string, rewrite keepingRewrite) (rewrittenMetadata, error) {
	if p.cooldownEnabled() && digest != "" {
		out, kept, err := rewrite(in)
		if err != nil {
			return rewrittenMetadata{}, err
		}
		return rewrittenMetadata{out, metadataValidators{etag: keptVersionsETag(digest, proxyURL, name, kept)}}, nil
	}
	rewriteOnly := func(b []byte) ([]byte, error) {
		out, _, err := rewrite(b)
		return out, err
	}
	var out []byte
	var v metadataValidators
	var err error
	if p.rewrites == nil || p.cooldownEnabled() {
		out, v, err = rewriteWithETag(in, rewriteOnly)
	} else {
		out, v, err = p.rewrites.rewrite(ctx, rewriteCacheKey(ecosystem, proxyURL, name, in), in, rewriteOnly)
	}
	return rewrittenMetadata{out, v}, err
}

func (p *Proxy) cooldownEnabled() bool {
	return p.Cooldown != nil && p.Cooldown.Enabled()
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
	out, v, ok := p.rewrites.get(rewriteCacheKeyForDigest(ecosystem, proxyURL, name, entry.ContentDigest.String))
	if !ok {
		return rewrittenMetadata{}, false
	}
	metrics.RecordCacheHit(ecosystem)
	return rewrittenMetadata{out, v}, true
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
