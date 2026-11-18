package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
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
