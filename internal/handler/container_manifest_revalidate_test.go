package handler

import (
	"context"
	"database/sql/driver"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"
)

const revalidateRepo = "/v2/library/app/manifests/"

// countingRegistry serves manifests by tag and digest and counts GET and HEAD
// requests per reference, since Docker Hub counts only GETs as pulls.
type countingRegistry struct {
	server *httptest.Server

	mu         sync.Mutex
	docs       map[string]string // by digest
	tags       map[string]string // tag -> digest
	gets       map[string]int
	heads      map[string]int
	headAccept string
	// headDigest is false for a registry whose HEAD has no
	// Docker-Content-Digest.
	headDigest bool
	// hold delays each response until holdFor requests arrived, or
	// holdTimeout passed, so concurrent callers overlap upstream.
	holdFor     int
	holdTimeout time.Duration
	arrived     int
	release     chan struct{}
}

func newCountingRegistry(t *testing.T) *countingRegistry {
	t.Helper()
	registry := &countingRegistry{
		docs:       make(map[string]string),
		tags:       make(map[string]string),
		gets:       make(map[string]int),
		heads:      make(map[string]int),
		headDigest: true,
		release:    make(chan struct{}),
	}
	registry.server = httptest.NewServer(http.HandlerFunc(registry.serve))
	t.Cleanup(registry.server.Close)
	return registry
}

func (c *countingRegistry) serve(w http.ResponseWriter, r *http.Request) {
	reference, ok := strings.CutPrefix(r.URL.Path, revalidateRepo)
	if !ok {
		http.NotFound(w, r)
		return
	}
	c.mu.Lock()
	if r.Method == http.MethodHead {
		c.heads[reference]++
		c.headAccept = r.Header.Get("Accept")
	} else {
		c.gets[reference]++
	}
	hold := c.holdFor > 0
	if hold {
		c.arrived++
		if c.arrived == c.holdFor {
			close(c.release)
		}
	}
	digest, isTag := c.tags[reference]
	if !isTag {
		digest = reference
	}
	body, found := c.docs[digest]
	headDigest := c.headDigest
	c.mu.Unlock()

	if hold {
		select {
		case <-c.release:
		case <-time.After(c.holdTimeout):
		}
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	etag := `"` + digest + `"`
	w.Header().Set("Content-Type", ociManifestMediaType)
	w.Header().Set("ETag", etag)
	if r.Method != http.MethodHead || headDigest {
		w.Header().Set("Docker-Content-Digest", digest)
	}
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, body)
	}
}

// add stores a manifest and returns its digest.
func (c *countingRegistry) add(body string) string {
	digest := sha256Digest([]byte(body))
	c.mu.Lock()
	c.docs[digest] = body
	c.mu.Unlock()
	return digest
}

// pointLatest points the latest tag at digest.
func (c *countingRegistry) pointLatest(digest string) {
	c.mu.Lock()
	c.tags["latest"] = digest
	c.mu.Unlock()
}

// counts returns the GETs and HEADs seen so far, all references together.
func (c *countingRegistry) counts() (gets, heads int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, n := range c.gets {
		gets += n
	}
	for _, n := range c.heads {
		heads += n
	}
	return gets, heads
}

func (c *countingRegistry) getsOf(reference string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gets[reference]
}

func newRevalidateTestHandler(t *testing.T, registry *countingRegistry) *ContainerHandler {
	t.Helper()
	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = registry.server.Client()
	proxy.MetadataTTL = time.Hour
	return &ContainerHandler{proxy: proxy, registryURL: registry.server.URL, proxyURL: "http://localhost:8080"}
}

// ageLatest moves the cached entry of the latest tag past the TTL.
func ageLatest(t *testing.T, h *ContainerHandler) {
	t.Helper()
	key := h.containerManifestSharedCacheKey(h.registryURL, "library/app", "latest", normalizeContainerManifestAccept(dockerManifestAccept))
	entry, err := h.proxy.DB.GetMetadataCache(containerManifestCacheEcosystem, key)
	if err != nil || entry == nil {
		t.Fatalf("cached tag row = %v, %v, want one", entry, err)
	}
	entry.FetchedAt.Time = time.Now().Add(-2 * time.Hour)
	if err := h.proxy.DB.UpsertMetadataCache(entry); err != nil {
		t.Fatalf("ageing cached tag: %v", err)
	}
}

func pullManifest(t *testing.T, h *ContainerHandler, method, reference string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "/library/app/manifests/"+reference, nil)
	request.Header.Set("Accept", dockerManifestAccept)
	response := httptest.NewRecorder()
	h.Routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("%s %s status = %d: %s", method, reference, response.Code, response.Body.String())
	}
	return response
}

func manifestDoc(n string) string {
	return `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","annotations":{"n":"` + n + `"}}`
}

func TestContainerHandler_StaleTagUnchangedRevalidatesWithHead(t *testing.T) {
	registry := newCountingRegistry(t)
	digest := registry.add(manifestDoc("a"))
	registry.pointLatest(digest)
	h := newRevalidateTestHandler(t, registry)

	pullManifest(t, h, http.MethodGet, "latest")
	ageLatest(t, h)
	response := pullManifest(t, h, http.MethodGet, "latest")

	if response.Body.String() != manifestDoc("a") {
		t.Errorf("body = %q, want the cached manifest", response.Body.String())
	}
	if gets, heads := registry.counts(); gets != 1 || heads != 1 {
		t.Errorf("upstream GETs = %d, HEADs = %d; want 1 GET for the first pull and 1 HEAD to revalidate", gets, heads)
	}
	registry.mu.Lock()
	headAccept := registry.headAccept
	registry.mu.Unlock()
	if headAccept != dockerManifestAccept {
		t.Errorf("HEAD Accept = %q, want the client's %q", headAccept, dockerManifestAccept)
	}
	// The HEAD refreshed the entry, so the next pull stays local.
	pullManifest(t, h, http.MethodGet, "latest")
	if gets, heads := registry.counts(); gets != 1 || heads != 1 {
		t.Errorf("after a fresh pull: GETs = %d, HEADs = %d, want 1 and 1", gets, heads)
	}
}

func TestContainerHandler_StaleTagMovedToCachedDigestNeedsNoGet(t *testing.T) {
	registry := newCountingRegistry(t)
	first := registry.add(manifestDoc("a"))
	second := registry.add(manifestDoc("b"))
	registry.pointLatest(first)
	h := newRevalidateTestHandler(t, registry)

	pullManifest(t, h, http.MethodGet, "latest")
	pullManifest(t, h, http.MethodGet, second)
	registry.pointLatest(second)
	ageLatest(t, h)

	response := pullManifest(t, h, http.MethodGet, "latest")
	if response.Body.String() != manifestDoc("b") {
		t.Errorf("body = %q, want the manifest the tag moved to", response.Body.String())
	}
	if got := response.Header().Get("Docker-Content-Digest"); got != second {
		t.Errorf("Docker-Content-Digest = %q, want %q", got, second)
	}
	if gets, heads := registry.counts(); gets != 2 || heads != 1 {
		t.Errorf("upstream GETs = %d, HEADs = %d; want only the 2 setup GETs and 1 HEAD", gets, heads)
	}
}

func TestContainerHandler_StaleTagMovedToNewDigestGetsByDigest(t *testing.T) {
	registry := newCountingRegistry(t)
	first := registry.add(manifestDoc("a"))
	registry.pointLatest(first)
	h := newRevalidateTestHandler(t, registry)

	pullManifest(t, h, http.MethodGet, "latest")
	second := registry.add(manifestDoc("b"))
	registry.pointLatest(second)
	ageLatest(t, h)

	response := pullManifest(t, h, http.MethodGet, "latest")
	if response.Body.String() != manifestDoc("b") {
		t.Errorf("body = %q, want the manifest the tag moved to", response.Body.String())
	}
	if got := registry.getsOf("latest"); got != 1 {
		t.Errorf("GETs of the tag = %d, want only the first pull's", got)
	}
	if got := registry.getsOf(second); got != 1 {
		t.Errorf("GETs of the new digest = %d, want 1", got)
	}
	if _, heads := registry.counts(); heads != 1 {
		t.Errorf("HEADs = %d, want 1", heads)
	}
	// Both the tag and the digest are now cached.
	pullManifest(t, h, http.MethodGet, "latest")
	pullManifest(t, h, http.MethodGet, second)
	if gets, heads := registry.counts(); gets != 2 || heads != 1 {
		t.Errorf("after cached pulls: GETs = %d, HEADs = %d, want 2 and 1", gets, heads)
	}
}

func TestContainerHandler_ConcurrentStaleTagRevalidatesOnce(t *testing.T) {
	const pulls = 8
	registry := newCountingRegistry(t)
	digest := registry.add(manifestDoc("a"))
	registry.pointLatest(digest)
	h := newRevalidateTestHandler(t, registry)

	pullManifest(t, h, http.MethodGet, "latest")
	ageLatest(t, h)
	setupGets, setupHeads := registry.counts()
	registry.mu.Lock()
	// Hold upstream answers until every pull reached upstream, or long
	// enough that a coalesced request is the only one.
	registry.holdFor = pulls
	registry.holdTimeout = 300 * time.Millisecond
	registry.mu.Unlock()

	var wg sync.WaitGroup
	for range pulls {
		wg.Go(func() {
			request := httptest.NewRequest(http.MethodGet, "/library/app/manifests/latest", nil)
			request.Header.Set("Accept", dockerManifestAccept)
			response := httptest.NewRecorder()
			h.Routes().ServeHTTP(response, request)
			if response.Code != http.StatusOK || response.Body.String() != manifestDoc("a") {
				t.Errorf("concurrent pull = %d %q", response.Code, response.Body.String())
			}
		})
	}
	wg.Wait()

	gets, heads := registry.counts()
	if requests := gets - setupGets + heads - setupHeads; requests != 1 {
		t.Errorf("upstream requests for %d concurrent pulls = %d, want 1", pulls, requests)
	}
}

func TestContainerHandler_StaleTagHeadWithoutDigestFallsBackToGet(t *testing.T) {
	registry := newCountingRegistry(t)
	first := registry.add(manifestDoc("a"))
	registry.pointLatest(first)
	registry.headDigest = false
	h := newRevalidateTestHandler(t, registry)

	pullManifest(t, h, http.MethodGet, "latest")
	second := registry.add(manifestDoc("b"))
	registry.pointLatest(second)
	ageLatest(t, h)

	response := pullManifest(t, h, http.MethodGet, "latest")
	if response.Body.String() != manifestDoc("b") {
		t.Errorf("body = %q, want the manifest the tag moved to", response.Body.String())
	}
	if got := registry.getsOf("latest"); got != 2 {
		t.Errorf("GETs of the tag = %d, want 2: the HEAD gave no digest", got)
	}
}

func TestContainerHandler_ClientHeadPointsTagAtCachedDigest(t *testing.T) {
	registry := newCountingRegistry(t)
	first := registry.add(manifestDoc("a"))
	second := registry.add(manifestDoc("b"))
	registry.pointLatest(first)
	h := newRevalidateTestHandler(t, registry)

	pullManifest(t, h, http.MethodGet, "latest")
	pullManifest(t, h, http.MethodGet, second)
	registry.pointLatest(second)
	ageLatest(t, h)

	head := pullManifest(t, h, http.MethodHead, "latest")
	if got := head.Header().Get("Docker-Content-Digest"); got != second {
		t.Errorf("HEAD Docker-Content-Digest = %q, want %q", got, second)
	}
	// The HEAD moved the cached tag, so the GET that follows it in a pull
	// is a cache hit.
	gets, heads := registry.counts()
	response := pullManifest(t, h, http.MethodGet, "latest")
	if response.Body.String() != manifestDoc("b") {
		t.Errorf("body = %q, want the manifest the tag moved to", response.Body.String())
	}
	if afterGets, afterHeads := registry.counts(); afterGets != gets || afterHeads != heads {
		t.Errorf("GET after HEAD went upstream: GETs %d -> %d, HEADs %d -> %d", gets, afterGets, heads, afterHeads)
	}
}

func TestContainerHandler_ClientHeadOfUncachedDigestIsNotStored(t *testing.T) {
	registry := newCountingRegistry(t)
	first := registry.add(manifestDoc("a"))
	registry.pointLatest(first)
	h := newRevalidateTestHandler(t, registry)

	pullManifest(t, h, http.MethodGet, "latest")
	second := registry.add(manifestDoc("b"))
	registry.pointLatest(second)
	ageLatest(t, h)

	head := pullManifest(t, h, http.MethodHead, "latest")
	if got := head.Header().Get("Docker-Content-Digest"); got != second {
		t.Errorf("HEAD Docker-Content-Digest = %q, want %q", got, second)
	}
	if got := head.Header().Get("Content-Length"); got != strconv.Itoa(len(manifestDoc("b"))) {
		t.Errorf("HEAD Content-Length = %q, want the upstream manifest size", got)
	}
	if gets, _ := registry.counts(); gets != 1 {
		t.Errorf("GETs = %d, want 1: a client HEAD must not cost a pull", gets)
	}
}

func TestContainerHandler_ConcurrentDigestMissFetchesOnce(t *testing.T) {
	const pulls = 8
	registry := newCountingRegistry(t)
	digest := registry.add(manifestDoc("a"))
	registry.holdFor = pulls
	registry.holdTimeout = 300 * time.Millisecond
	h := newRevalidateTestHandler(t, registry)

	var wg sync.WaitGroup
	for range pulls {
		wg.Go(func() {
			request := httptest.NewRequest(http.MethodGet, "/library/app/manifests/"+digest, nil)
			request.Header.Set("Accept", dockerManifestAccept)
			response := httptest.NewRecorder()
			h.Routes().ServeHTTP(response, request)
			if response.Code != http.StatusOK || response.Body.String() != manifestDoc("a") {
				t.Errorf("concurrent pull = %d %q", response.Code, response.Body.String())
			}
		})
	}
	wg.Wait()

	if got := registry.getsOf(digest); got != 1 {
		t.Errorf("upstream GETs for %d concurrent pulls by digest = %d, want 1", pulls, got)
	}
}

func TestContainerHandler_StaleTagDeletedUpstreamRelaysNotFound(t *testing.T) {
	registry := newCountingRegistry(t)
	registry.pointLatest(registry.add(manifestDoc("a")))
	h := newRevalidateTestHandler(t, registry)

	pullManifest(t, h, http.MethodGet, "latest")
	registry.mu.Lock()
	delete(registry.tags, "latest")
	registry.mu.Unlock()
	ageLatest(t, h)

	request := httptest.NewRequest(http.MethodGet, "/library/app/manifests/latest", nil)
	request.Header.Set("Accept", dockerManifestAccept)
	response := httptest.NewRecorder()
	h.Routes().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want the upstream 404", response.Code)
	}
	if !strings.Contains(response.Body.String(), "not found") {
		t.Errorf("body = %q, want the upstream error body", response.Body.String())
	}
}

// Docker resolves a tag with HEAD, then fetches only by digest. The HEAD
// must survive a restart even though the digest wasn't stored at that point.
func TestContainerHandler_HeadThenDigestOffline(t *testing.T) {
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		t.Run(method, func(t *testing.T) {
			registry := newCountingRegistry(t)
			body := manifestDoc("head-first")
			digest := registry.add(body)
			registry.pointLatest(digest)
			h := newRevalidateTestHandler(t, registry)
			pullManifest(t, h, http.MethodHead, "latest")
			if gets, _ := registry.counts(); gets != 0 {
				t.Fatal("HEAD fetched an unsolicited body")
			}
			pullManifest(t, h, http.MethodGet, digest)
			registry.server.Close()
			// New handler, same persisted cache, and expired tag TTL.
			h = &ContainerHandler{proxy: h.proxy, registryURL: h.registryURL}
			h.proxy.MetadataTTL = 0
			response := pullManifest(t, h, method, "latest")
			if response.Header().Get("Docker-Content-Digest") != digest || response.Header().Get("Content-Type") != ociManifestMediaType {
				t.Fatalf("lost manifest headers: %v", response.Header())
			}
			if method == http.MethodGet && response.Body.String() != body {
				t.Fatalf("body = %q, want %q", response.Body.String(), body)
			}
			if response.Header().Get("Warning") == "" {
				t.Fatal("missing stale warning")
			}
		})
	}
}

func TestContainerHandler_HeadResolutionUpstreamFailures(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusTooManyRequests, http.StatusNotFound, http.StatusUnauthorized} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			registry := newCountingRegistry(t)
			digest := registry.add(manifestDoc("a"))
			registry.pointLatest(digest)
			h := newRevalidateTestHandler(t, registry)
			pullManifest(t, h, http.MethodHead, "latest")
			pullManifest(t, h, http.MethodGet, digest)
			h.proxy.MetadataTTL = 0
			h.proxy.HTTPClient = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("unavailable")), Request: r}, nil
			})}
			for _, method := range []string{http.MethodHead, http.MethodGet} {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(method, "/library/app/manifests/latest", nil)
				req.Header.Set("Accept", dockerManifestAccept)
				h.Routes().ServeHTTP(rec, req)
				want := status
				if shouldServeStaleManifest(status) {
					want = http.StatusOK
					if rec.Header().Get("Docker-Content-Digest") != digest || rec.Header().Get("Warning") == "" {
						t.Fatalf("lost stale resolution: %v", rec.Header())
					}
				}
				if rec.Code != want {
					t.Errorf("%s status = %d, want %d", method, rec.Code, want)
				}
			}
		})
	}
}

func TestContainerHandler_HeadResolutionMovesBeforeDigestFetched(t *testing.T) {
	registry := newCountingRegistry(t)
	first := registry.add(manifestDoc("a"))
	second := registry.add(manifestDoc("b"))
	registry.pointLatest(first)
	h := newRevalidateTestHandler(t, registry)
	pullManifest(t, h, http.MethodGet, "latest")
	h.proxy.MetadataTTL = 0
	registry.pointLatest(second)
	pullManifest(t, h, http.MethodHead, "latest")
	pullManifest(t, h, http.MethodGet, second)
	registry.server.Close()
	if got := pullManifest(t, h, http.MethodGet, "latest"); got.Body.String() != manifestDoc("b") {
		t.Fatalf("tag still points at old manifest: %s", got.Body.String())
	}
}

func TestContainerHandler_HeadResolutionWithoutBodyDoesNotInventManifest(t *testing.T) {
	registry := newCountingRegistry(t)
	digest := registry.add(manifestDoc("a"))
	registry.pointLatest(digest)
	h := newRevalidateTestHandler(t, registry)
	pullManifest(t, h, http.MethodHead, "latest")
	registry.server.Close()
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/library/app/manifests/latest", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("unfetched body answered %d: %s", rec.Code, rec.Body.String())
	}
}

func TestContainerHandler_HeadResolutionTimeoutServesStoredTag(t *testing.T) {
	registry := newCountingRegistry(t)
	digest := registry.add(manifestDoc("a"))
	registry.pointLatest(digest)
	h := newRevalidateTestHandler(t, registry)
	pullManifest(t, h, http.MethodHead, "latest")
	pullManifest(t, h, http.MethodGet, digest)
	registry.mu.Lock()
	registry.holdFor = 10
	registry.holdTimeout = 50 * time.Millisecond
	registry.mu.Unlock()
	h.proxy.MetadataTTL = 0
	h.proxy.HTTPClient.Timeout = 5 * time.Millisecond
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		if got := pullManifest(t, h, method, "latest"); got.Header().Get("Docker-Content-Digest") != digest {
			t.Fatalf("lost digest on timeout: %v", got.Header())
		}
	}
}

func TestContainerHandler_HeadThenDigestWithinTTL(t *testing.T) {
	registry := newCountingRegistry(t)
	body := manifestDoc("fresh-head")
	digest := registry.add(body)
	registry.pointLatest(digest)
	h := newRevalidateTestHandler(t, registry)
	pullManifest(t, h, http.MethodHead, "latest")
	pullManifest(t, h, http.MethodGet, digest)
	gets, heads := registry.counts()
	got := pullManifest(t, h, http.MethodGet, "latest")
	if got.Body.String() != body || got.Header().Get("Warning") != "" {
		t.Fatalf("fresh resolved tag = %s, headers %v", got.Body.String(), got.Header())
	}
	if g, h := registry.counts(); g != gets || h != heads {
		t.Fatalf("upstream requests changed from %d/%d to %d/%d", gets, heads, g, h)
	}
}

// Observe SELECTs of the resolution row, not storage opens: resolutions have
// no body. A view calls the counter only when that row's digest is selected.
func observeResolutionReads(t *testing.T, h *ContainerHandler, fn string) {
	t.Helper()
	if _, err := h.proxy.DB.Exec(`ALTER TABLE metadata_cache RENAME TO observed_metadata_cache`); err != nil {
		t.Fatal(err)
	}
	_, err := h.proxy.DB.Exec(`CREATE VIEW metadata_cache AS SELECT
		id, ecosystem, name, storage_path, etag, link, content_type, content_encoding,
		CASE WHEN ecosystem = 'oci-manifest-tag' THEN ` + fn + `(content_digest) ELSE content_digest END AS content_digest,
		upstream_url, size, last_modified, fetched_at, last_accessed_at, created_at, updated_at
		FROM observed_metadata_cache`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestContainerHandler_FreshTagSkipsResolutionRead(t *testing.T) {
	var reads atomic.Int32
	fn := fmt.Sprintf("resolution_reads_%d", time.Now().UnixNano())
	// Functions are installed when connections open, so register before
	// creating this test's database.
	if err := sqlite.RegisterScalarFunction(fn, 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		reads.Add(1)
		return args[0], nil
	}); err != nil {
		t.Fatal(err)
	}
	registry := newCountingRegistry(t)
	digest := registry.add(manifestDoc("fresh"))
	registry.pointLatest(digest)
	h := newRevalidateTestHandler(t, registry)
	// Retain an actual resolution row, then cache a fresh tag body.
	pullManifest(t, h, http.MethodHead, "latest")
	pullManifest(t, h, http.MethodGet, "latest")
	observeResolutionReads(t, h, fn)
	pullManifest(t, h, http.MethodGet, "latest")
	if got := reads.Load(); got != 0 {
		t.Fatalf("fresh tag read the resolution row %d times", got)
	}
	// Prove the observer sees a direct resolution read.
	key := h.containerManifestSharedCacheKey(h.registryURL, "library/app", "latest", normalizeContainerManifestAccept(dockerManifestAccept))
	if _, err := h.proxy.DB.GetMetadataCache(containerManifestTagEcosystem, key); err != nil {
		t.Fatal(err)
	}
	if reads.Load() != 1 {
		t.Fatal("resolution read observer was not exercised")
	}
}

func TestContainerHandler_HeadResolutionHasNoStoredBody(t *testing.T) {
	registry := newCountingRegistry(t)
	digest := registry.add(manifestDoc("head-only"))
	registry.pointLatest(digest)
	h := newRevalidateTestHandler(t, registry)
	pullManifest(t, h, http.MethodHead, "latest")
	key := h.containerManifestSharedCacheKey(h.registryURL, "library/app", "latest", normalizeContainerManifestAccept(dockerManifestAccept))
	entry, err := h.proxy.DB.GetMetadataCache(containerManifestTagEcosystem, key)
	if err != nil || entry == nil {
		t.Fatalf("resolution row = %v, %v", entry, err)
	}
	if entry.ContentDigest.String != digest || entry.StoragePath != "" || entry.Size.Int64 != 0 {
		t.Fatalf("resolution should be bodiless: %+v", entry)
	}
	if exists, err := h.proxy.Storage.Exists(context.Background(), metadataStoragePath(containerManifestTagEcosystem, key)); err != nil || exists {
		t.Fatalf("resolution storage object exists = %t, %v", exists, err)
	}
}

func TestContainerHandler_HeadWithCachedDigestSkipsResolutionWrite(t *testing.T) {
	registry := newCountingRegistry(t)
	digest := registry.add(manifestDoc("cached-digest"))
	registry.pointLatest(digest)
	h := newRevalidateTestHandler(t, registry)
	pullManifest(t, h, http.MethodGet, digest)
	pullManifest(t, h, http.MethodHead, "latest")
	key := h.containerManifestSharedCacheKey(h.registryURL, "library/app", "latest", normalizeContainerManifestAccept(dockerManifestAccept))
	entry, err := h.proxy.DB.GetMetadataCache(containerManifestTagEcosystem, key)
	if err != nil || entry != nil {
		t.Fatalf("redundant resolution row = %v, %v", entry, err)
	}
}

func TestContainerHandler_ResolutionComparesCompatibleCandidate(t *testing.T) {
	for _, newerCompatible := range []bool{false, true} {
		t.Run(fmt.Sprintf("newer-compatible=%t", newerCompatible), func(t *testing.T) {
			registry := newCountingRegistry(t)
			h := newRevalidateTestHandler(t, registry)
			h.proxy.MetadataTTL = time.Hour
			accept := dockerManifestListMediaType
			cacheAccept := normalizeContainerManifestAccept(accept)
			key := h.containerManifestSharedCacheKey(h.registryURL, "library/app", "latest", cacheAccept)
			old := &cachedContainerManifest{body: []byte("old-list"), contentType: accept, contentDigest: sha256Digest([]byte("old-list")), fetchedAt: time.Now().Add(-4 * time.Hour)}
			if newerCompatible {
				old.fetchedAt = time.Now().Add(-2 * time.Hour)
			}
			h.storeContainerManifestLegacyEntries(t.Context(), h.registryURL, "library/app", "latest", accept, cacheAccept, old)
			incompatible := &cachedContainerManifest{body: []byte("oci-index"), contentType: ociImageIndexMediaType, contentDigest: sha256Digest([]byte("oci-index")), fetchedAt: time.Now().Add(-time.Hour)}
			if err := h.storeContainerManifest(t.Context(), key, incompatible); err != nil {
				t.Fatal(err)
			}
			resolved := &cachedContainerManifest{body: []byte("resolved-list"), contentType: accept, contentDigest: sha256Digest([]byte("resolved-list")), fetchedAt: time.Now().Add(-3 * time.Hour)}
			digestKey := h.containerManifestSharedCacheKey(h.registryURL, "library/app", resolved.contentDigest, cacheAccept)
			if err := h.storeContainerManifest(t.Context(), digestKey, resolved); err != nil {
				t.Fatal(err)
			}
			// Seed both formats so the regression also runs against the old
			// body-backed implementation, before the bodiless-row fix.
			if _, err := h.storeContainerMetadata(t.Context(), containerManifestTagEcosystem, key, []byte(resolved.contentDigest), "", "", "text/plain", resolved.contentDigest, time.Time{}, resolved.fetchedAt); err != nil {
				t.Fatal(err)
			}
			got, _ := h.loadContainerManifestForAccept(t.Context(), h.registryURL, "library/app", "latest", accept, cacheAccept)
			want := resolved
			if newerCompatible {
				want = old
			}
			if got == nil || string(got.body) != string(want.body) {
				t.Fatalf("cached compatible candidate = %+v, want %s", got, want.body)
			}
		})
	}
}
