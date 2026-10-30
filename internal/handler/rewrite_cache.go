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
// the key, and the denylist is loaded at startup. The exception is cooldown,
// which depends on the time of the request: with cooldown on, entries expire
// after Proxy.cooldownRewriteTTL, so a version is held back at most that long
// past its cooldown.
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
	// now reads the time entries expire by; nil means time.Now.
	now func() time.Time

	mu       sync.Mutex
	size     int64
	order    *list.List // most recently used at the front
	entries  map[string]*list.Element
	inFlight map[string]*inflightRewrite
}

type rewriteEntry struct {
	key string
	rewrittenMetadata
	// expires is when the entry stops being served, or zero for never.
	expires time.Time
}

// maxCooldownRewriteTTL is the longest a rewrite made with cooldown on is
// served. Versions leave cooldown as time passes, so a cached rewrite would
// otherwise keep hiding them. Cooldowns run to days, and a package manager
// that checks every lockfile entry on each install, as pnpm 12 does, would
// otherwise have the whole lockfile rewritten again on every install more
// than a minute after the last.
const maxCooldownRewriteTTL = time.Hour

// minCooldownRewriteTTL is the shortest a rewrite made with cooldown on is
// served, however short the cooldown.
const minCooldownRewriteTTL = time.Minute

// cooldownRewriteShare is the share of the shortest cooldown a rewrite made
// with cooldown on is served for, as a divisor.
const cooldownRewriteShare = 24

// cooldownRewriteTTL returns how long a rewrite made with cooldown on is
// served: a 24th of the shortest cooldown configured, so a version is held
// back at most about 4% past its cooldown, kept between a minute and an hour.
func (p *Proxy) cooldownRewriteTTL() time.Duration {
	ttl := p.ShortestCooldown / cooldownRewriteShare
	switch {
	case ttl <= 0 || ttl > maxCooldownRewriteTTL:
		return maxCooldownRewriteTTL
	case ttl < minCooldownRewriteTTL:
		return minCooldownRewriteTTL
	}
	return ttl
}

// size is how much of the cache's room the entry takes.
func (e *rewriteEntry) size() int64 {
	return int64(len(e.plain) + len(e.gzipped))
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
// compressed copy to serve. The cache keeps only gzipped when there is one,
// so plain is nil then; content returns the document either way.
type rewrittenMetadata struct {
	plain []byte
	metadataValidators
	gzipped []byte
	// size is the length of the uncompressed document.
	size int
}

// content returns the document uncompressed. A cached document is kept
// only gzip-compressed, and is decompressed for each caller that needs the
// plain bytes: clients not accepting gzip, and handlers reading the document.
// The compressed copy was made in memory by gzipIfSmaller, so reading it back
// fails only on a bug; content returns nil then.
func (m rewrittenMetadata) content() []byte {
	if m.plain != nil || m.gzipped == nil {
		return m.plain
	}
	zr, err := gzip.NewReader(bytes.NewReader(m.gzipped))
	if err != nil {
		return nil
	}
	out := bytes.NewBuffer(make([]byte, 0, m.size))
	if _, err := io.Copy(out, zr); err != nil {
		return nil
	}
	return out.Bytes()
}

// identity returns m to be served without its gzip copy.
func (m rewrittenMetadata) identity() rewrittenMetadata {
	m.plain = m.content()
	m.gzipped = nil
	return m
}

// compact returns m as the cache keeps it: only the gzip copy when there is
// one.
func (m rewrittenMetadata) compact() rewrittenMetadata {
	if m.gzipped != nil {
		m.plain = nil
	}
	return m
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
	e, ok := c.lookup(key, c.clock())
	if !ok {
		return rewrittenMetadata{}, false
	}
	return e.rewrittenMetadata, true
}

// lookup returns the live entry for key and marks it recently used. An
// expired entry is dropped. Called with mu held.
func (c *rewriteCache) lookup(key string, now time.Time) (*rewriteEntry, bool) {
	el, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	e := el.Value.(*rewriteEntry)
	if !e.expires.IsZero() && !now.Before(e.expires) {
		c.remove(el)
		return nil, false
	}
	c.order.MoveToFront(el)
	return e, true
}

func (c *rewriteCache) clock() time.Time {
	if c.now == nil {
		return time.Now()
	}
	return c.now()
}

// remove drops an entry. Called with mu held.
func (c *rewriteCache) remove(el *list.Element) {
	e := el.Value.(*rewriteEntry)
	c.order.Remove(el)
	delete(c.entries, e.key)
	c.size -= e.size()
}

// rewrite returns rewrite(in) with its ETag and gzip copy, from the cache
// when it can. A ttl above zero makes the result expire after that long.
// Callers must treat the returned bytes as read-only: a cached result is
// shared. A caller waiting on another's rewrite leaves when ctx ends; the
// rewrite itself always runs to completion, so the result is cached for the
// next request.
func (c *rewriteCache) rewrite(ctx context.Context, key string, in []byte, ttl time.Duration, rewrite func([]byte) ([]byte, error)) (rewrittenMetadata, error) {
	if c == nil {
		return rewriteWithETag(in, rewrite)
	}

	c.mu.Lock()
	if e, ok := c.lookup(key, c.clock()); ok {
		c.mu.Unlock()
		return e.rewrittenMetadata, nil
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

	// The entry expires counting from before the rewrite, so it never
	// outlives the time its cooldown decisions were made at by more than ttl.
	var expires time.Time
	if ttl > 0 {
		expires = c.clock().Add(ttl)
	}
	defer func() {
		c.mu.Lock()
		delete(c.inFlight, key)
		if f.err == nil {
			c.add(&rewriteEntry{key: key, rewrittenMetadata: f.m.compact(), expires: expires})
		}
		c.mu.Unlock()
		close(f.done)
	}()
	f.m, f.err = rewriteWithETag(in, rewrite)
	if f.err == nil {
		f.m.gzipped = gzipIfSmaller(f.m.plain)
	}
	return f.m, f.err
}

// rewriteWithETag runs rewrite and computes the ETag of what it returns.
func rewriteWithETag(in []byte, rewrite func([]byte) ([]byte, error)) (rewrittenMetadata, error) {
	out, err := rewrite(in)
	if err != nil {
		return rewrittenMetadata{}, err
	}
	return rewrittenMetadata{plain: out, metadataValidators: metadataValidators{etag: metadataETag(out)}, size: len(out)}, nil
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
	if old, ok := c.entries[e.key]; ok {
		c.remove(old)
	}
	c.entries[e.key] = c.order.PushFront(e)
	c.size += n
	for c.size > c.maxBytes {
		c.remove(c.order.Back())
	}
}

// cachedRewrite rewrites a metadata document through the proxy's rewrite
// cache and returns it with its validators. digest identifies in when the
// metadata cache stored it, and is empty otherwise; when known it keys the
// cache, so the document isn't hashed again.
//
// With cooldown on, the cached rewrite expires after p.cooldownRewriteTTL. Its
// ETag is the hash of its output, and it is dated like any other, so clients
// revalidate with either validator and get the gzip copy.
//
// Without the rewrite cache, cooldown rewrites run on every request and go
// undated: when digest is known their ETag comes from their inputs rather
// than from hashing each output again, and that ETag changes with every
// restart, so replicas would never agree on a date for it.
func (p *Proxy) cachedRewrite(ctx context.Context, ecosystem, proxyURL, name string, in []byte, digest string, rewrite keepingRewrite) (rewrittenMetadata, error) {
	rewriteOnly := func(b []byte) ([]byte, error) {
		out, _, err := rewrite(b)
		return out, err
	}
	cooldown := p.cooldownEnabled()

	if p.rewrites != nil {
		key := rewriteCacheKeyForDigest(ecosystem, proxyURL, name, digest)
		if digest == "" {
			key = rewriteCacheKey(ecosystem, proxyURL, name, in)
		}
		var ttl time.Duration
		if cooldown {
			ttl = p.cooldownRewriteTTL()
		}
		m, err := p.rewrites.rewrite(ctx, key, in, ttl, rewriteOnly)
		if err == nil {
			m.lastModified = p.metadataDate(ecosystem, proxyURL, name, m.etag)
		}
		return m, err
	}

	if cooldown && digest != "" {
		out, kept, err := rewrite(in)
		if err != nil {
			return rewrittenMetadata{}, err
		}
		return rewrittenMetadata{plain: out, metadataValidators: metadataValidators{etag: keptVersionsETag(digest, proxyURL, name, kept)}, size: len(out)}, nil
	}
	m, err := rewriteWithETag(in, rewriteOnly)
	if err == nil && !cooldown {
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
	return rewrittenMetadata{plain: out, metadataValidators: v, size: len(out)}
}

func (p *Proxy) cooldownEnabled() bool {
	return p.Cooldown != nil && p.Cooldown.Enabled()
}

// storedRewrite returns the cached rewrite of the metadata stored for
// ecosystem and cacheKey, when that metadata is still within its TTL and a
// rewrite of it is cached and has not expired. It reads only the cache row,
// never the stored document, so a repeated request for a large packument
// costs a database lookup instead of reading and hashing the whole document
// again. When it reports false the caller takes the usual fetch and rewrite
// path.
func (p *Proxy) storedRewrite(ecosystem, cacheKey, proxyURL, name string) (rewrittenMetadata, bool) {
	m, _, ok := p.storedRewriteVariant(ecosystem, cacheKey, proxyURL, name, nil)
	return m, ok
}

// storedRewriteVariant is storedRewrite for a rewrite whose output depends on
// more than the stored document. variant is given the stored content type
// and returns the rest of what the rewrite depends on, as passed to
// variantDigest; nil means nothing else. It also returns the stored content
// type, as a fetch of the document would.
func (p *Proxy) storedRewriteVariant(ecosystem, cacheKey, proxyURL, name string, variant func(contentType string) []string) (rewrittenMetadata, string, bool) {
	if p.rewrites == nil {
		return rewrittenMetadata{}, "", false
	}
	if !p.CacheMetadata || p.DB == nil || p.MetadataTTL <= 0 {
		return rewrittenMetadata{}, "", false
	}
	entry, err := p.DB.GetMetadataCache(ecosystem, cacheKey)
	if err != nil || entry == nil || !entry.ContentDigest.Valid || entry.ContentEncoding.String != "" {
		return rewrittenMetadata{}, "", false
	}
	if !entry.FetchedAt.Valid || time.Since(entry.FetchedAt.Time) >= p.MetadataTTL {
		return rewrittenMetadata{}, "", false
	}
	contentType := contentTypeJSON
	if entry.ContentType.Valid {
		contentType = entry.ContentType.String
	}
	digest := entry.ContentDigest.String
	if variant != nil {
		digest = variantDigest(digest, nil, variant(contentType)...)
	}
	m, ok := p.rewrites.get(rewriteCacheKeyForDigest(ecosystem, proxyURL, name, digest))
	if !ok {
		return rewrittenMetadata{}, "", false
	}
	m.lastModified = p.metadataDate(ecosystem, proxyURL, name, m.etag)
	metrics.RecordMetadataCacheHit(ecosystem, m.size)
	p.recordMetadataRead(entry)
	return m, contentType, true
}

// variantDigest returns a digest for a rewrite of in that also depends on
// parts, such as the content type or the versions a filter removes, so that
// rewrites differing in them are cached apart. digest is in's digest as the
// metadata cache records it, or empty to hash in. Passed to cachedRewrite in
// place of the document's digest, it keeps the metadata date keyed by the
// package alone.
func variantDigest(digest string, in []byte, parts ...string) string {
	if digest == "" {
		sum := sha256.Sum256(in)
		digest = "sha256:" + hex.EncodeToString(sum[:])
	}
	h := sha256.New()
	var length []byte
	for _, part := range parts {
		length = binary.AppendUvarint(length[:0], uint64(len(part)))
		_, _ = h.Write(length)
		_, _ = io.WriteString(h, part)
	}
	return digest + "+" + hex.EncodeToString(h.Sum(nil))
}

// serveRewrittenMetadata writes rewritten metadata with its validators, or
// 304 Not Modified when they show the client's copy is current. A client
// accepting gzip gets the compressed copy when there is one. Being another
// representation, it has an ETag of its own.
func serveRewrittenMetadata(w http.ResponseWriter, r *http.Request, m rewrittenMetadata) {
	serveRewrittenMetadataAs(w, r, m, contentTypeJSON)
}

// serveRewrittenMetadataAs is serveRewrittenMetadata for a document of
// another content type.
func serveRewrittenMetadataAs(w http.ResponseWriter, r *http.Request, m rewrittenMetadata, contentType string) {
	etag := m.etag
	compressed := m.gzipped != nil && acceptsGzip(r)
	if compressed {
		etag = gzipETag(m.etag)
	}
	ensureVary(w.Header(), headerAcceptEncoding)
	w.Header().Set(headerETag, etag)
	if !m.lastModified.IsZero() {
		w.Header().Set(headerLastModified, m.lastModified.UTC().Format(http.TimeFormat))
	}
	if notModified(r, etag, m.lastModified) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body := m.gzipped
	if !compressed {
		body = m.content()
	}
	w.Header().Set(headerContentType, contentType)
	if compressed {
		w.Header().Set(headerContentEncoding, "gzip")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// ensureVary adds field to the Vary header unless it is listed already.
func ensureVary(header http.Header, field string) {
	for _, value := range header.Values("Vary") {
		for listed := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(listed), field) {
				return
			}
		}
	}
	header.Add("Vary", field)
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
	if p.rewrites != nil {
		p.rewrites.now = p.clock
	}
}
