package handler

import (
	"bytes"
	"compress/gzip"
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
	"strconv"
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
// copy without the output being hashed again on every request. Dates are not
// the cache's to give: they must agree across replicas, so the database holds
// them (see metadataDate).
//
// Each entry also keeps its output gzip-compressed, when that is smaller, so
// clients accepting gzip get it without the document being compressed again
// on every request, by the proxy or by a reverse proxy in front of it.
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
	rewrittenMetadata
}

// size is how much of the cache's room the entry takes.
func (e *rewriteEntry) size() int64 {
	return int64(len(e.body) + len(e.gzipped))
}

// inflightRewrite is one rewrite that concurrent callers share. m and err are
// written before done closes and read only after, so the close is the
// handoff.
type inflightRewrite struct {
	done chan struct{}
	m    rewrittenMetadata
	err  error
}

// metadataValidators are what a client revalidates its copy of rewritten
// metadata with. lastModified is zero when there is none to give.
type metadataValidators struct {
	etag         string
	lastModified time.Time
}

// rewrittenMetadata is a rewritten metadata document with its validators.
// gzipped is the document gzip-compressed, or nil when there is no
// compressed copy to serve.
type rewrittenMetadata struct {
	body []byte
	metadataValidators
	gzipped []byte
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

// get returns the cached rewrite for key, if there is one.
func (c *rewriteCache) get(key string) (rewrittenMetadata, bool) {
	if c == nil {
		return rewrittenMetadata{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return rewrittenMetadata{}, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*rewriteEntry).rewrittenMetadata, true
}

// rewrite returns rewrite(in) with its ETag and gzip copy, from the cache
// when it can. Callers must treat the returned bytes as read-only: a cached
// result is shared. A caller waiting on another's rewrite leaves when ctx
// ends; the rewrite itself always runs to completion, so the result is cached
// for the next request.
func (c *rewriteCache) rewrite(ctx context.Context, key string, in []byte, rewrite func([]byte) ([]byte, error)) (rewrittenMetadata, error) {
	if c == nil {
		return rewriteWithETag(in, rewrite)
	}

	c.mu.Lock()
	if el, ok := c.entries[key]; ok {
		c.order.MoveToFront(el)
		c.mu.Unlock()
		return el.Value.(*rewriteEntry).rewrittenMetadata, nil
	}
	if f, ok := c.inFlight[key]; ok {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return rewrittenMetadata{}, ctx.Err()
		case <-f.done:
			return f.m, f.err
		}
	}
	f := &inflightRewrite{done: make(chan struct{}), err: errSharedRewriteAbandoned}
	c.inFlight[key] = f
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.inFlight, key)
		if f.err == nil {
			c.add(&rewriteEntry{key: key, rewrittenMetadata: f.m})
		}
		c.mu.Unlock()
		close(f.done)
	}()
	f.m, f.err = rewriteWithETag(in, rewrite)
	if f.err == nil {
		f.m.gzipped = gzipIfSmaller(f.m.body)
	}
	return f.m, f.err
}

// rewriteWithETag runs rewrite and computes the ETag of what it returns.
func rewriteWithETag(in []byte, rewrite func([]byte) ([]byte, error)) (rewrittenMetadata, error) {
	out, err := rewrite(in)
	if err != nil {
		return rewrittenMetadata{}, err
	}
	return rewrittenMetadata{body: out, metadataValidators: metadataValidators{etag: metadataETag(out)}}, nil
}

// gzipIfSmaller returns body gzip-compressed, or nil when compressing does
// not make it smaller.
func gzipIfSmaller(body []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(body); err != nil {
		return nil
	}
	if err := zw.Close(); err != nil || buf.Len() >= len(body) {
		return nil
	}
	return buf.Bytes()
}

// add stores e and evicts least recently used entries until the cache is
// back under its limit. An entry larger than the whole cache is not stored.
// Called with mu held.
func (c *rewriteCache) add(e *rewriteEntry) {
	n := e.size()
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
		c.size -= e.size()
	}
}

// cachedRewrite rewrites a metadata document through the proxy's rewrite
// cache and returns it with its validators. digest identifies in when the
// metadata cache stored it, and is empty otherwise. Cooldown filtering depends
// on the current time, so with cooldown on a cached rewrite could keep hiding
// a version past its cooldown; those rewrites always run, with no
// Last-Modified, and when digest is known their ETag comes from their inputs
// rather than from hashing each output again. Their ETag also changes with
// every restart, so replicas would never agree on a date for them.
func (p *Proxy) cachedRewrite(ctx context.Context, ecosystem, proxyURL, name string, in []byte, digest string, rewrite keepingRewrite) (rewrittenMetadata, error) {
	if p.cooldownEnabled() && digest != "" {
		out, kept, err := rewrite(in)
		if err != nil {
			return rewrittenMetadata{}, err
		}
		return rewrittenMetadata{body: out, metadataValidators: metadataValidators{etag: keptVersionsETag(digest, proxyURL, name, kept)}}, nil
	}
	rewriteOnly := func(b []byte) ([]byte, error) {
		out, _, err := rewrite(b)
		return out, err
	}
	var m rewrittenMetadata
	var err error
	if p.rewrites == nil || p.cooldownEnabled() {
		m, err = rewriteWithETag(in, rewriteOnly)
	} else {
		m, err = p.rewrites.rewrite(ctx, rewriteCacheKey(ecosystem, proxyURL, name, in), in, rewriteOnly)
	}
	if err == nil && !p.cooldownEnabled() {
		m.lastModified = p.metadataDate(ecosystem, proxyURL, name, m.etag)
	}
	return m, err
}

// uncachedRewrite gives a rewrite made outside the cache its validators: an
// ETag of the output itself and, as cachedRewrite does, a date that replicas
// agree on.
func (p *Proxy) uncachedRewrite(ecosystem, proxyURL, name string, out []byte) rewrittenMetadata {
	v := metadataValidators{etag: metadataETag(out)}
	if !p.cooldownEnabled() {
		v.lastModified = p.metadataDate(ecosystem, proxyURL, name, v.etag)
	}
	return rewrittenMetadata{body: out, metadataValidators: v}
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
	m, ok := p.rewrites.get(rewriteCacheKeyForDigest(ecosystem, proxyURL, name, entry.ContentDigest.String))
	if !ok {
		return rewrittenMetadata{}, false
	}
	m.lastModified = p.metadataDate(ecosystem, proxyURL, name, m.etag)
	metrics.RecordCacheHit(ecosystem)
	p.recordMetadataRead(entry)
	return m, true
}

// serveRewrittenMetadata writes rewritten metadata with its validators, or
// 304 Not Modified when they show the client's copy is current. A client
// accepting gzip gets the compressed copy when there is one. Being another
// representation, it has an ETag of its own.
func serveRewrittenMetadata(w http.ResponseWriter, r *http.Request, m rewrittenMetadata) {
	body, etag := m.body, m.etag
	compressed := m.gzipped != nil && acceptsGzip(r)
	if compressed {
		body, etag = m.gzipped, gzipETag(m.etag)
	}
	w.Header().Set("Vary", headerAcceptEncoding)
	w.Header().Set(headerETag, etag)
	if !m.lastModified.IsZero() {
		w.Header().Set(headerLastModified, m.lastModified.UTC().Format(http.TimeFormat))
	}
	if notModified(r, etag, m.lastModified) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set(headerContentType, contentTypeJSON)
	if compressed {
		w.Header().Set(headerContentEncoding, "gzip")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// gzipETag returns the ETag of the gzip representation of the document
// whose ETag is etag.
func gzipETag(etag string) string {
	return strings.TrimSuffix(etag, `"`) + `-gzip"`
}

// acceptsGzip reports whether the request's Accept-Encoding allows a gzip
// response: gzip or x-gzip, or else "*", with a quality above zero.
func acceptsGzip(r *http.Request) bool {
	wildcard := false
	for _, value := range r.Header.Values(headerAcceptEncoding) {
		for coding := range strings.SplitSeq(value, ",") {
			name, params, _ := strings.Cut(coding, ";")
			switch strings.ToLower(strings.TrimSpace(name)) {
			case "gzip", "x-gzip":
				return codingQualityPositive(params)
			case "*":
				wildcard = codingQualityPositive(params)
			}
		}
	}
	return wildcard
}

// codingQualityPositive reports whether an Accept-Encoding entry's
// parameters give it a quality above zero. No q parameter means 1.
func codingQualityPositive(params string) bool {
	for param := range strings.SplitSeq(params, ";") {
		key, value, _ := strings.Cut(param, "=")
		if strings.TrimSpace(key) != "q" {
			continue
		}
		q, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		return err == nil && q > 0
	}
	return true
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
