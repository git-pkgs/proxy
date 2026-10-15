package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/metrics"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	"golang.org/x/mod/sumdb/tlog"
)

const (
	// defaultGoSumDBName is the checksum database the go command uses when
	// GOSUMDB is not set.
	defaultGoSumDBName = "sum.golang.org"
	defaultGoSumDBURL  = "https://sum.golang.org"
	goEcosystem        = "golang"

	// goSumDBProbeBackoff is how long supported keeps answering 404 after the
	// proxy failed to reach a checksum database, before it tries again.
	goSumDBProbeBackoff = time.Minute
	// goSumDBProbeTimeout is how long supported waits for the database. The
	// go command's own request has no timeout, so a database that drops
	// packets must not hold every build for the full upstream timeout.
	goSumDBProbeTimeout = 5 * time.Second
)

// goSumDBProbes remembers when the proxy last failed to reach each checksum
// database, so a database that is unreachable does not make every go command
// wait for an upstream timeout before it falls back.
type goSumDBProbes struct {
	// timeout overrides goSumDBProbeTimeout when set (tests).
	timeout time.Duration

	mu        sync.Mutex
	failed    map[string]time.Time
	succeeded map[string]time.Time
}

func (p *goSumDBProbes) probeTimeout() time.Duration {
	if p.timeout > 0 {
		return p.timeout
	}
	return goSumDBProbeTimeout
}

func (p *goSumDBProbes) recentlyFailed(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	failedAt, ok := p.failed[name]
	return ok && time.Since(failedAt) < goSumDBProbeBackoff
}

func (p *goSumDBProbes) record(name string, reachable bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if reachable {
		delete(p.failed, name)
		if p.succeeded == nil {
			p.succeeded = make(map[string]time.Time)
		}
		p.succeeded[name] = time.Now()
		return
	}
	if p.failed == nil {
		p.failed = make(map[string]time.Time)
	}
	p.failed[name] = time.Now()
}

// recordFailure records that the database did not answer a request started
// at start, unless a probe has reached it since: a probe that timed out, or
// a lookup that fell back to the cache, must not overwrite a newer success.
func (p *goSumDBProbes) recordFailure(name string, start time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.succeeded[name].Before(start) {
		return
	}
	if p.failed == nil {
		p.failed = make(map[string]time.Time)
	}
	p.failed[name] = time.Now()
}

// SetSumDBs sets the checksum databases served at /sumdb/{name}/, mapping
// each database name to its base URL. An empty map keeps the default,
// sum.golang.org. offline makes a cached tree of any age enough to keep
// answering for a database the proxy cannot reach, for networks where
// clients have no route to the database of their own.
func (h *GoHandler) SetSumDBs(sumDBs map[string]string, offline bool) {
	h.sumDBOffline = offline
	if len(sumDBs) == 0 {
		h.sumDBs = map[string]string{defaultGoSumDBName: defaultGoSumDBURL}
		return
	}
	h.sumDBs = make(map[string]string, len(sumDBs))
	for name, sumDBURL := range sumDBs {
		h.sumDBs[name] = strings.TrimSuffix(sumDBURL, "/")
	}
}

// handleSumDB serves checksum database requests that the go command sends
// through GOPROXY as /sumdb/{name}/{path}. The module proxy upstream does not
// serve them, so they are fetched from the configured database itself.
// Unknown names get 404, which makes the go command contact the database
// directly.
func (h *GoHandler) handleSumDB(w http.ResponseWriter, r *http.Request, rest string) {
	name, path, _ := strings.Cut(rest, "/")
	sumDBURL, ok := h.sumDBs[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if path == "supported" {
		if h.sumDBReachable(r, name, sumDBURL) {
			w.WriteHeader(http.StatusOK)
		} else {
			http.NotFound(w, r)
		}
		return
	}
	immutable, valid := parseGoSumDBPath(path)
	if !valid {
		http.NotFound(w, r)
		return
	}
	// Only mirrors and auditors fetch data tiles, never the go command, and
	// together they hold the whole log, so they are not stored.
	if tile, err := tlog.ParseTilePath(path); err == nil && tile.L < 0 {
		h.proxy.proxyMetadataStream(w, r, sumDBURL+"/"+path, "identity", "*/*")
		return
	}

	cacheKey := goSumDBCacheKey(name, sumDBURL, path)
	check := goSumDBResponseCheck(path)
	served, damaged := false, false
	if immutable {
		served, damaged = h.serveCachedSumDBTile(w, r, cacheKey, check)
	}
	if served {
		return
	}
	h.proxySumDB(w, r, name, sumDBURL+"/"+path, cacheKey, check, damaged)
}

// sumDBReachable reports whether the proxy can serve a checksum database: it
// can fetch the latest signed tree, or has it cached. Once supported answers
// 200 the go command uses only the proxy for that database and never falls
// back to contacting it directly, so a database the proxy cannot reach gets
// 404 instead. The probe goes through the same cache entry as a latest
// request: with cache_metadata it costs at most one upstream request per
// metadata TTL, without it each probe fetches latest.
func (h *GoHandler) sumDBReachable(r *http.Request, name, sumDBURL string) bool {
	cacheKey := goSumDBCacheKey(name, sumDBURL, "latest")
	treeCheck := goSumDBResponseCheck("latest")
	if h.sumDBProbes.recentlyFailed(name) {
		return h.cachedTreeServes(r.Context(), cacheKey, treeCheck)
	}
	// The fetch is shared with concurrent callers and runs detached from this
	// request, so it is bounded by the upstream timeout, not by ours. Wait
	// for it only up to the probe timeout; it finishes in the background,
	// records its own outcome and fills the cache for the next probe.
	start := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- h.probeSumDB(context.WithoutCancel(r.Context()), name, cacheKey, sumDBURL, treeCheck)
	}()
	var err error
	select {
	case err = <-done:
	case <-time.After(h.sumDBProbes.probeTimeout()):
		err = fmt.Errorf("no answer within %s", h.sumDBProbes.probeTimeout())
		h.sumDBProbes.recordFailure(name, start)
	case <-r.Context().Done():
		// The client went away; that says nothing about the database.
		return false
	}
	if err == nil {
		return true
	}
	// The stale-cache fallback inside the fetch only runs once upstream has
	// failed, which a silent database delays past the probe timeout. A tree
	// cached earlier may still serve lookups, so look for it here.
	if h.cachedTreeServes(r.Context(), cacheKey, treeCheck) {
		return true
	}
	h.proxy.Logger.Info("checksum database unreachable, sending go clients to it directly",
		"name", name, "url", sumDBURL, "error", err)
	return false
}

// probeSumDB fetches the database's latest tree and records whether the
// database answered. A tree still fresh in the cache is enough for supported
// but records nothing, since the database was not asked: counting it as an
// answer would keep lookups that fail during an outage from being
// remembered. A tree served because upstream failed is not an answer.
func (h *GoHandler) probeSumDB(ctx context.Context, name, cacheKey, sumDBURL string, check func([]byte) error) error {
	if entry, _, _, ok := h.readCachedSumDB(ctx, cacheKey, check); ok && h.treeFresh(entry) {
		return nil
	}
	res := h.proxy.lookupMetadata(ctx, goEcosystem, cacheKey, sumDBURL+"/latest", "*/*", "identity", check)
	err := res.err
	if err == nil && res.stale {
		err = errors.New("checksum database did not answer, tree served from cache")
	}
	h.sumDBProbes.record(name, err == nil)
	return err
}

// cachedTreeServes reports whether a cached tree lets the proxy answer for
// the database although the database did not: always with go_sumdbs_offline,
// otherwise only while the tree is fresh.
func (h *GoHandler) cachedTreeServes(ctx context.Context, cacheKey string, check func([]byte) error) bool {
	entry, _, _, ok := h.readCachedSumDB(ctx, cacheKey, check)
	return ok && (h.sumDBOffline || h.treeFresh(entry))
}

// treeFresh reports whether a cached tree is within the metadata TTL.
func (h *GoHandler) treeFresh(entry *database.MetadataCacheEntry) bool {
	ttl := h.proxy.MetadataTTL
	return ttl > 0 && entry.FetchedAt.Valid && time.Since(entry.FetchedAt.Time) < ttl
}

// readCachedSumDB reads a checksum database response from the metadata
// cache regardless of its age. It reports false when nothing usable is
// cached.
func (h *GoHandler) readCachedSumDB(ctx context.Context, cacheKey string, check func([]byte) error) (entry *database.MetadataCacheEntry, body []byte, contentType string, ok bool) {
	p := h.proxy
	if !p.CacheMetadata || p.DB == nil {
		return nil, nil, "", false
	}
	entry, err := p.DB.GetMetadataCache(goEcosystem, cacheKey)
	if err != nil || entry == nil {
		return nil, nil, "", false
	}
	body, contentType, err = p.readCachedMetadata(ctx, entry, check)
	if err != nil {
		return nil, nil, "", false
	}
	return entry, body, contentType, true
}

// parseGoSumDBPath reports whether path is a request the checksum database
// protocol defines and whether its response can never change. Only complete
// tiles are immutable; partial tiles, latest and lookups follow the metadata
// TTL.
func parseGoSumDBPath(path string) (immutable, valid bool) {
	if path == "latest" {
		return false, true
	}
	if mod, ok := strings.CutPrefix(path, "lookup/"); ok {
		return false, validGoSumDBLookup(mod)
	}
	tile, err := tlog.ParseTilePath(path)
	if err != nil {
		return false, false
	}
	return tile.W == 1<<tile.H, true
}

// goSumDBTreePrefix starts every signed tree a checksum database serves, as
// latest and after each lookup record (tlog.FormatTree).
const goSumDBTreePrefix = "go.sum database tree\n"

// goSumDBResponseCheck returns a check for a response to path, or nil when
// path is not one the protocol defines. A 200 that is not what the protocol
// defines (a block page from an egress proxy, a cut-off response) is refused
// rather than cached. latest must look like a signed tree, since supported
// counts it as proof the database is reachable. A lookup must be a record
// followed by a signed tree, which is how the go command reads it; a block
// page cached in its place would also replace the copy kept for an outage.
// A hash tile holds exactly W hashes, and complete tiles are never
// revalidated, so a bad one would otherwise stay for good. Data tiles are
// never cached and not checked.
func goSumDBResponseCheck(path string) func([]byte) error {
	if path == "latest" {
		return func(body []byte) error {
			if !strings.HasPrefix(string(body), goSumDBTreePrefix) {
				return errors.New("checksum database latest is not a signed tree")
			}
			return nil
		}
	}
	if strings.HasPrefix(path, "lookup/") {
		return func(body []byte) error {
			_, _, tree, err := tlog.ParseRecord(body)
			if err != nil || !strings.HasPrefix(string(tree), goSumDBTreePrefix) {
				return fmt.Errorf("checksum database lookup %s is not a record with a signed tree", path)
			}
			return nil
		}
	}
	tile, err := tlog.ParseTilePath(path)
	if err != nil {
		return nil
	}
	return func(body []byte) error {
		if len(body) != tile.W*tlog.HashSize {
			return fmt.Errorf("checksum database tile %s has %d bytes, want %d", path, len(body), tile.W*tlog.HashSize)
		}
		return nil
	}
}

// validGoSumDBLookup checks an escaped module@version the way the checksum
// database does. Requiring a semantic version also keeps characters such as
// '#' and '%' out of the upstream URL.
func validGoSumDBLookup(mod string) bool {
	escPath, escVersion, ok := strings.Cut(mod, "@")
	if !ok {
		return false
	}
	if _, err := module.UnescapePath(escPath); err != nil {
		return false
	}
	version, err := module.UnescapeVersion(escVersion)
	return err == nil && semver.IsValid(version)
}

// goSumDBCacheKey hashes the database name, its URL and the request path, as
// APKHandler.metadataCacheKey does, so repointing a database at another URL
// does not serve the old database's entries.
func goSumDBCacheKey(name, sumDBURL, path string) string {
	digest := sha256.Sum256([]byte(name + "\x00" + sumDBURL + "\x00" + path))
	return "sumdb/" + hex.EncodeToString(digest[:])
}

// serveCachedSumDBTile serves a complete tile from the metadata cache
// regardless of its age, since its content never changes. It reports
// served false when the tile is not cached, and damaged as well when the
// cached copy fails check.
func (h *GoHandler) serveCachedSumDBTile(w http.ResponseWriter, r *http.Request, cacheKey string, check func([]byte) error) (served, damaged bool) {
	entry, body, contentType, ok := h.readCachedSumDB(r.Context(), cacheKey, nil)
	if !ok {
		return false, false
	}
	if check != nil && check(body) != nil {
		return false, true
	}
	metrics.RecordMetadataCacheHit(goEcosystem, len(body))

	if entry.ETag.Valid {
		w.Header().Set(headerETag, entry.ETag.String)
		if ifNoneMatchHits(r.Header.Get("If-None-Match"), entry.ETag.String) {
			w.WriteHeader(http.StatusNotModified)
			return true, false
		}
	}
	w.Header().Set(headerContentType, contentType)
	w.Header().Set(headerContentLength, strconv.Itoa(len(body)))
	if entry.ContentEncoding.Valid && entry.ContentEncoding.String != "" {
		w.Header().Set(headerContentEncoding, entry.ContentEncoding.String)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	return true, false
}

// proxySumDB fetches a checksum database response through the metadata
// cache. Unlike ProxyCached it relays the start of an upstream 404 body: the
// go command prints it, and it says why a module version is missing. check,
// when set, must accept a response before it is cached or served. damaged
// says the cached copy failed check.
func (h *GoHandler) proxySumDB(w http.ResponseWriter, r *http.Request, name, upstreamURL, cacheKey string, check func([]byte) error, damaged bool) {
	p := h.proxy
	if !p.CacheMetadata {
		p.proxyMetadataStream(w, r, upstreamURL, "identity", "*/*")
		return
	}
	// While the database is known to be unreachable, offline mode serves
	// the cached copy at once rather than letting every request wait for
	// the upstream timeout before the fetch falls back to it.
	if h.sumDBOffline && h.sumDBProbes.recentlyFailed(name) {
		if entry, body, contentType, ok := h.readCachedSumDB(r.Context(), cacheKey, check); ok {
			p.writeMetadataCachedResponseWithEncoding(w, r, goEcosystem, cacheKey, body, contentType, entry.ContentEncoding.String)
			return
		}
	}

	start := time.Now()
	res := h.fetchSumDB(r.Context(), upstreamURL, cacheKey, check, damaged)
	if res.stale {
		// The database did not answer. Remember it here too: the go command
		// asks supported only once per process, so a long build would lose
		// the shortcut above a minute into an outage.
		h.sumDBProbes.recordFailure(name, start)
	}
	if err := res.err; err != nil {
		var notFound *upstreamNotFoundError
		switch {
		case errors.As(err, &notFound) && len(notFound.body) > 0:
			w.Header().Set(headerContentType, "text/plain; charset=utf-8")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write(notFound.body)
		case errors.Is(err, ErrUpstreamNotFound):
			http.Error(w, "not found", http.StatusNotFound)
		default:
			p.Logger.Error("checksum database fetch failed", "url", upstreamURL, "error", err)
			http.Error(w, "failed to fetch from upstream", http.StatusBadGateway)
		}
		return
	}

	p.writeMetadataCachedResponseWithEncoding(w, r, goEcosystem, cacheKey, res.body, res.contentType, res.contentEncoding)
}

// fetchSumDB fetches a checksum database response through the metadata
// cache. A cached copy that failed check is fetched again without
// revalidating it: past the TTL, a database that sends an ETag would answer
// 304 and hand back the damaged copy for good.
func (h *GoHandler) fetchSumDB(ctx context.Context, upstreamURL, cacheKey string, check func([]byte) error, damaged bool) metadataResult {
	p := h.proxy
	if damaged {
		return p.fetchMetadataFromUpstream(ctx, goEcosystem, cacheKey, upstreamURL, "*/*", "identity", check, nil)
	}
	return p.lookupMetadata(ctx, goEcosystem, cacheKey, upstreamURL, "*/*", "identity", check)
}
