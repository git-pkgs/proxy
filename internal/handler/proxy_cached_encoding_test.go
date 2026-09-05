package handler

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// gzipWhenAskedUpstream serves compressed bytes with Content-Encoding: gzip
// when the request advertises gzip, plain bytes otherwise, like a CDN that
// compresses on the fly. It records the last Accept-Encoding it saw and counts
// every request before the availability gate so a cache-miss refetch during a
// simulated outage is observable.
type gzipWhenAskedUpstream struct {
	*httptest.Server
	available      atomic.Bool
	requests       atomic.Int32
	acceptEncoding atomic.Value // string
}

func newGzipWhenAskedUpstream(plain, compressed []byte) *gzipWhenAskedUpstream {
	u := &gzipWhenAskedUpstream{}
	u.available.Store(true)
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.requests.Add(1)
		u.acceptEncoding.Store(r.Header.Get(headerAcceptEncoding))
		if !u.available.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set(headerContentType, contentTypeJSON)
		if strings.Contains(r.Header.Get(headerAcceptEncoding), "gzip") {
			w.Header().Set(headerContentEncoding, "gzip")
			_, _ = w.Write(compressed)
			return
		}
		_, _ = w.Write(plain)
	}))
	return u
}

func (u *gzipWhenAskedUpstream) sawAcceptEncoding() string {
	s, _ := u.acceptEncoding.Load().(string)
	return s
}

// serveGzip issues one request through proxyCachedWithEncoding asking the
// upstream for gzip.
func serveGzip(proxy *Proxy, upstreamURL string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/index.json", nil)
	proxy.proxyCachedWithEncoding(w, r, upstreamURL, "gzip-test", "index", "gzip", "*/*")
	return w
}

func assertGzipResponse(t *testing.T, label string, w *httptest.ResponseRecorder, compressed []byte) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("%s: status = %d, want 200: %s", label, w.Code, w.Body.String())
	}
	if !bytes.Equal(w.Body.Bytes(), compressed) {
		t.Errorf("%s: body is not the compressed bytes (got %d, want %d)", label, w.Body.Len(), len(compressed))
	}
	if got := w.Header().Get(headerContentEncoding); got != "gzip" {
		t.Errorf("%s: Content-Encoding = %q, want %q", label, got, "gzip")
	}
	if got := w.Header().Get(headerContentLength); got != strconv.Itoa(len(compressed)) {
		t.Errorf("%s: Content-Length = %q, want %d", label, got, len(compressed))
	}
}

// TestProxyCachedWithEncoding_GzipCachesAndReplays covers the cached path:
// requesting gzip upstream stores the compressed bytes plus Content-Encoding
// and replays both from cache without contacting the upstream again.
func TestProxyCachedWithEncoding_GzipCachesAndReplays(t *testing.T) {
	plain := []byte(`{"packages":{}}`)
	compressed := gzipPayload(t, plain)
	upstream := newGzipWhenAskedUpstream(plain, compressed)
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.HTTPClient = upstream.Client()

	first := serveGzip(proxy, upstream.URL+"/index.json")
	assertGzipResponse(t, "first", first, compressed)
	if got := upstream.sawAcceptEncoding(); got != "gzip" {
		t.Errorf("upstream Accept-Encoding = %q, want %q", got, "gzip")
	}

	before := upstream.requests.Load()
	upstream.available.Store(false)
	cached := serveGzip(proxy, upstream.URL+"/index.json")
	assertGzipResponse(t, "cached", cached, compressed)
	if upstream.requests.Load() != before {
		t.Errorf("cached replay hit upstream: requests %d -> %d", before, upstream.requests.Load())
	}
}

// TestProxyCachedWithEncoding_GzipStreamPath covers the cache_metadata=false
// branch: the streaming path must request gzip and forward Content-Encoding.
func TestProxyCachedWithEncoding_GzipStreamPath(t *testing.T) {
	plain := []byte(`{"packages":{}}`)
	compressed := gzipPayload(t, plain)
	upstream := newGzipWhenAskedUpstream(plain, compressed)
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = false
	proxy.HTTPClient = upstream.Client()

	w := serveGzip(proxy, upstream.URL+"/index.json")
	assertGzipResponse(t, "stream", w, compressed)
	if got := upstream.sawAcceptEncoding(); got != "gzip" {
		t.Errorf("stream path upstream Accept-Encoding = %q, want %q", got, "gzip")
	}
}

// TestProxyCachedWithEncoding_GzipSurvivesCacheWriteFailure covers the failure
// the gzip mode makes reachable: when the metadata cache write fails the
// freshly fetched body is still served, so its Content-Encoding must come from
// the fetch and not from the (unwritten) cache row -- otherwise gzip bytes go
// out labelled application/json with no Content-Encoding.
func TestProxyCachedWithEncoding_GzipSurvivesCacheWriteFailure(t *testing.T) {
	plain := []byte(`{"packages":{}}`)
	compressed := gzipPayload(t, plain)
	upstream := newGzipWhenAskedUpstream(plain, compressed)
	defer upstream.Close()

	proxy, _, store, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.HTTPClient = upstream.Client()
	store.storeErr = errors.New("disk full")

	w := serveGzip(proxy, upstream.URL+"/index.json")
	assertGzipResponse(t, "store-failure", w, compressed)
}

// TestProxyCachedWithEncoding_GzipStaleFallbackKeepsEncoding pins the
// stale-fallback return: when the upstream fails after the entry has expired,
// the stored gzip blob is served with its Content-Encoding taken from the
// cache row.
func TestProxyCachedWithEncoding_GzipStaleFallbackKeepsEncoding(t *testing.T) {
	plain := []byte(`{"packages":{}}`)
	compressed := gzipPayload(t, plain)
	upstream := newGzipWhenAskedUpstream(plain, compressed)
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = 0 // every request revalidates; an upstream failure falls back to the stale row
	proxy.HTTPClient = upstream.Client()

	first := serveGzip(proxy, upstream.URL+"/index.json")
	assertGzipResponse(t, "first", first, compressed)

	upstream.available.Store(false)
	stale := serveGzip(proxy, upstream.URL+"/index.json")
	assertGzipResponse(t, "stale", stale, compressed)
}
