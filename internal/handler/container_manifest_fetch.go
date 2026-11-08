package handler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/git-pkgs/proxy/internal/metrics"
)

// manifestRequest is one client manifest request as the upstream sees it.
type manifestRequest struct {
	registryURL string
	name        string
	reference   string
	accept      string
	cacheAccept string
	method      string
}

func (req manifestRequest) immutable() bool {
	return manifestDigestReferencePattern.MatchString(req.reference)
}

// coalesceKey identifies requests that can share one upstream exchange. The
// normalized Accept is part of it because registries answer a tag with an
// index or a single manifest depending on it, and the method because a HEAD
// never fetches a manifest body the client did not ask for.
func (req manifestRequest) coalesceKey() string {
	return strings.Join([]string{containerManifestCacheEcosystem, req.method, req.registryURL, req.name, req.reference, req.cacheAccept}, "\x00")
}

func (req manifestRequest) url(reference string) string {
	return fmt.Sprintf("%s/v2/%s/manifests/%s", req.registryURL, req.name, reference)
}

// manifestOutcome is what callers sharing a manifest request write. Exactly
// one of manifest, relay and digestInvalid is set; otherwise err is the
// upstream failure, answered from the caller's stale copy if it has one.
type manifestOutcome struct {
	manifest      *cachedContainerManifest
	relay         *capturedResponse
	digestInvalid bool
	err           error
}

// capturedResponse is an upstream response other than a manifest, such as a
// 404 or 401, read in full so every caller sharing the request can relay it.
// A body that could not be read to the end is relayed up to that point and
// then fails the same way, so clients still see the truncation.
type capturedResponse struct {
	status  int
	header  http.Header
	body    []byte
	readErr error
	trailer http.Header
}

func (h *ContainerHandler) captureResponse(resp *http.Response) *capturedResponse {
	captured := &capturedResponse{status: resp.StatusCode, header: resp.Header.Clone()}
	if resp.Request != nil && resp.Request.Method == http.MethodHead {
		return captured
	}
	limit := h.proxy.MetadataMaxSize
	if limit <= 0 {
		limit = defaultMetadataMaxSize
	}
	captured.body, captured.readErr = io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if captured.readErr == nil && int64(len(captured.body)) > limit {
		captured.body, captured.readErr = captured.body[:limit], ErrMetadataTooLarge
	}
	captured.trailer = resp.Trailer.Clone()
	return captured
}

type failingReader struct{ err error }

func (f failingReader) Read([]byte) (int, error) { return 0, f.err }

func (c *capturedResponse) write(p *Proxy, w http.ResponseWriter, r *http.Request) {
	var body io.Reader = bytes.NewReader(c.body)
	if c.readErr != nil {
		body = io.MultiReader(body, failingReader{c.readErr})
	}
	resp := &http.Response{
		StatusCode: c.status,
		Header:     c.header.Clone(),
		Trailer:    c.trailer.Clone(),
		Body:       io.NopCloser(body),
	}
	p.relayResponse(w, r, resp, copyContainerManifestHeaders)
}

// coalescedManifest runs the upstream part of a manifest request once for
// concurrent callers with the same key. Like coalesceMetadata, it runs on a
// context detached from the first caller, and each waiter still leaves when
// its own context ends. The result is a whole manifest with its headers,
// which a metadataResult does not carry, hence the separate group.
func (h *ContainerHandler) coalescedManifest(ctx context.Context, req manifestRequest) manifestOutcome {
	results := h.manifestFlight.DoChan(req.coalesceKey(), func() (any, error) {
		return h.fetchManifest(context.WithoutCancel(ctx), req), nil
	})
	select {
	case <-ctx.Done():
		return manifestOutcome{err: ctx.Err()}
	case res := <-results:
		outcome, ok := res.Val.(manifestOutcome)
		if !ok {
			return manifestOutcome{err: errSharedFetchAbandoned}
		}
		return outcome
	}
}

// fetchManifest answers a manifest request that the cache could not. Other
// instances sharing the database take turns through a fetch lease, so a tag
// that expired everywhere at once is revalidated once.
func (h *ContainerHandler) fetchManifest(ctx context.Context, req manifestRequest) (outcome manifestOutcome) {
	usable := func(m *cachedContainerManifest) bool {
		return m != nil && (req.immutable() || h.containerManifestFresh(m))
	}
	// A shared request that finished since the caller looked has refreshed
	// the entry already.
	cached, fromFallback := h.loadContainerManifestForAccept(ctx, req.registryURL, req.name, req.reference, req.accept, req.cacheAccept)
	if usable(cached) {
		return manifestOutcome{manifest: cached}
	}

	// Without a TTL nothing another instance stores is fresh, so there is
	// nothing to wait for.
	if req.immutable() || h.proxy.MetadataTTL > 0 {
		var stored *cachedContainerManifest
		release, lease, err := h.proxy.awaitFetchLease(ctx, metrics.CacheKindMetadata, req.coalesceKey(), func() bool {
			stored, _ = h.loadContainerManifestForAccept(ctx, req.registryURL, req.name, req.reference, req.accept, req.cacheAccept)
			return usable(stored)
		})
		if err != nil {
			return manifestOutcome{err: err}
		}
		switch lease {
		case leaseStored:
			return manifestOutcome{manifest: stored}
		case leaseFailed:
			if cached != nil {
				return manifestOutcome{err: errFetchFailedElsewhere}
			}
		case leaseFetch:
		}
		defer func() { release(outcome.err != nil) }()
	}

	switch {
	case req.method == http.MethodHead:
		return h.headManifest(ctx, req, cached, fromFallback)
	case req.immutable():
		return h.getManifest(ctx, req, req.reference, cached, fromFallback)
	default:
		return h.revalidateTag(ctx, req, cached, fromFallback)
	}
}

// revalidateTag answers a GET for a tag that is stale or not cached. Docker
// Hub counts manifest GETs against its pull limit but not HEADs, so a stale
// tag is resolved with a HEAD first and the manifest fetched by digest only
// when it is not cached yet. A registry whose HEAD gives no digest gets the
// GET for the tag, as before. A tag never seen is fetched with one GET: a
// HEAD first would only add a round trip.
func (h *ContainerHandler) revalidateTag(ctx context.Context, req manifestRequest, cached *cachedContainerManifest, fromFallback bool) manifestOutcome {
	if cached == nil {
		return h.getManifest(ctx, req, req.reference, nil, false)
	}
	resp, err := h.requestManifest(ctx, http.MethodHead, req, req.reference, "")
	if err != nil {
		return manifestOutcome{err: err}
	}
	_ = resp.Body.Close()

	digest := resp.Header.Get("Docker-Content-Digest")
	switch {
	case resp.StatusCode == http.StatusOK && manifestDigestReferencePattern.MatchString(digest):
		if outcome, ok := h.tagFromCachedDigest(ctx, req, cached, fromFallback, resp, digest); ok {
			return outcome
		}
		if outcome := h.getManifest(ctx, req, digest, nil, false); outcome.manifest != nil {
			return outcome
		}
		// The tag may have moved again, or the registry may not serve
		// manifests by digest to this client; ask for the tag itself.
	case shouldServeStaleManifest(resp.StatusCode):
		return manifestOutcome{err: fmt.Errorf("upstream status %d", resp.StatusCode)}
	}
	return h.getManifest(ctx, req, req.reference, cached, fromFallback)
}

// headManifest answers a client HEAD. When the digest upstream names is
// cached, the tag entry is pointed at it, so the next GET is a cache hit.
// Otherwise the upstream headers are passed on and nothing is stored.
func (h *ContainerHandler) headManifest(ctx context.Context, req manifestRequest, cached *cachedContainerManifest, fromFallback bool) manifestOutcome {
	resp, err := h.requestManifest(ctx, http.MethodHead, req, req.reference, "")
	if err != nil {
		return manifestOutcome{err: err}
	}
	_ = resp.Body.Close()

	digest := resp.Header.Get("Docker-Content-Digest")
	switch {
	case resp.StatusCode == http.StatusOK && manifestDigestReferencePattern.MatchString(digest):
		if req.immutable() && digestMismatch(req.reference, digest) {
			h.proxy.Logger.Error("upstream manifest HEAD names another digest",
				"name", req.name, "reference", req.reference, "upstream", digest)
			return manifestOutcome{digestInvalid: true}
		}
		if !req.immutable() {
			if outcome, ok := h.tagFromCachedDigest(ctx, req, cached, fromFallback, resp, digest); ok {
				return outcome
			}
		}
		size := int64(-1)
		if value := resp.Header.Get(headerContentLength); value != "" {
			if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
				size = parsed
			}
		}
		return manifestOutcome{manifest: &cachedContainerManifest{
			contentType:   resp.Header.Get(headerContentType),
			contentDigest: digest,
			etag:          resp.Header.Get(headerETag),
			size:          size,
			lastModified:  parseHTTPTime(resp.Header.Get(headerLastModified)),
		}}
	case shouldServeStaleManifest(resp.StatusCode) && (cached != nil || referrersTagPattern.MatchString(req.reference)):
		return manifestOutcome{err: fmt.Errorf("upstream status %d", resp.StatusCode)}
	}
	return manifestOutcome{relay: h.captureResponse(resp)}
}

// digestMismatch reports whether two digests of the same algorithm differ.
// Digests of different algorithms can't be compared without the body.
func digestMismatch(requested, upstream string) bool {
	reqAlg, reqHex, _ := strings.Cut(requested, ":")
	upAlg, upHex, _ := strings.Cut(upstream, ":")
	return reqAlg == upAlg && !strings.EqualFold(reqHex, upHex)
}

// tagFromCachedDigest answers a tag from the cache once a HEAD named the
// digest it points to: the cached tag entry if the tag did not move, or the
// manifest stored under the new digest. It reports false when neither is
// cached.
func (h *ContainerHandler) tagFromCachedDigest(ctx context.Context, req manifestRequest, cached *cachedContainerManifest, fromFallback bool, resp *http.Response, digest string) (manifestOutcome, bool) {
	if cached != nil && cached.contentDigest == digest {
		cached.fetchedAt = time.Now()
		h.refreshManifest(ctx, req, cached, fromFallback)
		return manifestOutcome{manifest: cached}, true
	}

	stored, err := h.loadContainerManifest(ctx, h.containerManifestSharedCacheKey(req.registryURL, req.name, digest, req.cacheAccept))
	if err != nil || stored == nil || stored.contentDigest != digest || !containerManifestCacheCompatible(req.accept, stored) {
		return manifestOutcome{}, false
	}
	moved := *stored
	moved.fetchedAt = time.Now()
	if etag := resp.Header.Get(headerETag); etag != "" {
		moved.etag = etag
	}
	if lastModified := parseHTTPTime(resp.Header.Get(headerLastModified)); !lastModified.IsZero() {
		moved.lastModified = lastModified
	}
	h.storeContainerManifestForAccept(ctx, req.registryURL, req.name, req.reference, req.accept, req.cacheAccept, &moved)
	return manifestOutcome{manifest: &moved}, true
}

// refreshManifest stores cached again with its new fetch time.
func (h *ContainerHandler) refreshManifest(ctx context.Context, req manifestRequest, cached *cachedContainerManifest, fromFallback bool) {
	if fromFallback {
		// The shared entry holds another representation for other
		// clients; only this client's own entries are refreshed.
		h.storeContainerManifestLegacyEntries(ctx, req.registryURL, req.name, req.reference, req.accept, req.cacheAccept, cached)
		return
	}
	h.storeContainerManifestForAccept(ctx, req.registryURL, req.name, req.reference, req.accept, req.cacheAccept, cached)
}

// getManifest GETs fetchRef, the requested reference or the digest a tag
// resolved to, and stores the manifest under the requested reference and
// its digest. cached, if set, is revalidated with its ETag.
func (h *ContainerHandler) getManifest(ctx context.Context, req manifestRequest, fetchRef string, cached *cachedContainerManifest, fromFallback bool) manifestOutcome {
	etag := ""
	if cached != nil {
		etag = cached.etag
	}
	resp, err := h.requestManifest(ctx, http.MethodGet, req, fetchRef, etag)
	if err != nil {
		return manifestOutcome{err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotModified && cached != nil {
		cached.fetchedAt = time.Now()
		h.refreshManifest(ctx, req, cached, fromFallback)
		return manifestOutcome{manifest: cached}
	}
	if resp.StatusCode != http.StatusOK {
		if shouldServeStaleManifest(resp.StatusCode) && (cached != nil || referrersTagPattern.MatchString(req.reference)) {
			return manifestOutcome{err: fmt.Errorf("upstream status %d", resp.StatusCode)}
		}
		return manifestOutcome{relay: h.captureResponse(resp)}
	}

	body, err := h.proxy.ReadMetadata(resp.Body)
	if err != nil {
		return manifestOutcome{err: fmt.Errorf("reading manifest: %w", err)}
	}
	contentDigest := resp.Header.Get("Docker-Content-Digest")
	// The references fetched and stored under, and the digest the upstream
	// says the manifest has, are all cache keys, so each digest among them
	// must be the manifest's own. One the proxy can't compute is refused
	// rather than cached unchecked.
	for _, expected := range []string{req.reference, fetchRef, contentDigest} {
		if !strings.Contains(expected, ":") {
			continue
		}
		if err := verifyDigest(expected, body); err != nil {
			h.proxy.Logger.Error("upstream manifest failed digest verification",
				"name", req.name, "reference", req.reference, "expected", expected, "error", err)
			return manifestOutcome{digestInvalid: true}
		}
	}
	if contentDigest == "" {
		contentDigest = sha256Digest(body)
	}

	manifest := &cachedContainerManifest{
		body:          body,
		contentType:   resp.Header.Get(headerContentType),
		contentDigest: contentDigest,
		etag:          resp.Header.Get(headerETag),
		size:          int64(len(body)),
		lastModified:  parseHTTPTime(resp.Header.Get(headerLastModified)),
		fetchedAt:     time.Now(),
	}
	h.storeContainerManifestForAccept(ctx, req.registryURL, req.name, req.reference, req.accept, req.cacheAccept, manifest)
	if manifest.contentDigest != req.reference && manifestDigestReferencePattern.MatchString(manifest.contentDigest) {
		h.storeContainerManifestForAccept(ctx, req.registryURL, req.name, manifest.contentDigest, req.accept, req.cacheAccept, manifest)
	}
	return manifestOutcome{manifest: manifest}
}

// requestManifest sends one manifest request upstream with the client's
// Accept header. The registry client adds credentials and follows bearer
// challenges for HEAD as for GET.
func (h *ContainerHandler) requestManifest(ctx context.Context, method string, req manifestRequest, reference, etag string) (*http.Response, error) {
	upstream, err := http.NewRequestWithContext(ctx, method, req.url(reference), nil)
	if err != nil {
		return nil, fmt.Errorf("creating manifest request: %w", err)
	}
	upstream.Header.Set("Accept", req.accept)
	if etag != "" {
		upstream.Header.Set("If-None-Match", etag)
	}
	return h.proxy.HTTPClient.Do(upstream)
}
