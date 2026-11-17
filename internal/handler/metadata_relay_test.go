package handler

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/git-pkgs/proxy/internal/metrics"
)

// relayedMetadata is a document served through proxyCachedWithEncoding, with
// an upstream that answers a request revalidating its ETag with 304.
type relayedMetadata struct {
	proxy *Proxy
	// upstreamURL is the URL the document is fetched from.
	upstreamURL string
	srv         *httptest.Server
	// full and conditional count the requests upstream answered with the
	// document and with 304.
	full, conditional atomic.Int32
}

// newRelayedMetadata serves doc under key "doc" of ecosystem "test", which
// the proxy asks upstream for with acceptEncoding. Upstream gzips it for a
// request that accepts gzip.
func newRelayedMetadata(t *testing.T, doc []byte, maxSize int64, acceptEncoding string) *relayedMetadata {
	t.Helper()
	rm := &relayedMetadata{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			rm.conditional.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		rm.full.Add(1)
		w.Header().Set(headerContentType, "application/x-test")
		w.Header().Set(headerETag, `"v1"`)
		if r.Header.Get(headerAcceptEncoding) == "gzip" {
			w.Header().Set(headerContentEncoding, "gzip")
			_, _ = w.Write(gzipBytes(t, doc))
			return
		}
		_, _ = w.Write(doc)
	}))
	t.Cleanup(upstream.Close)
	rm.upstreamURL = upstream.URL + "/doc"

	rm.proxy, _, _, _ = setupTestProxy(t)
	rm.proxy.CacheMetadata = true
	rm.proxy.MetadataTTL = time.Hour
	rm.proxy.MetadataMaxSize = maxSize
	rm.proxy.HTTPClient = upstream.Client()
	rm.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rm.proxy.proxyCachedWithEncoding(w, r, rm.upstreamURL, "test", "doc", acceptEncoding, nil, "*/*")
	}))
	t.Cleanup(rm.srv.Close)
	return rm
}

// get requests the document, and returns the status, the bytes as sent,
// without decompressing them, and the Content-Encoding.
func (rm *relayedMetadata) get(t *testing.T) (int, []byte, string) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	resp, err := client.Get(rm.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body, resp.Header.Get(headerContentEncoding)
}

// expire ages the cache row past its TTL, so the next request revalidates it.
func (rm *relayedMetadata) expire(t *testing.T) {
	t.Helper()
	entry, err := rm.proxy.DB.GetMetadataCache("test", "doc")
	if err != nil || entry == nil {
		t.Fatalf("no cache row: %v", err)
	}
	if _, err := rm.proxy.DB.RefreshMetadataCache("test", "doc", entry.ContentDigest.String, time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
}

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A document the proxy only relays is streamed from upstream into storage
// and served from there, so metadata_max_size, which bounds documents read
// into memory, doesn't apply to it.
func TestRelayedMetadataOverMaxSizeIsStreamed(t *testing.T) {
	doc := bytes.Repeat([]byte("x"), 4096)
	rm := newRelayedMetadata(t, doc, 1024, "identity")

	for i := range 2 {
		status, body, _ := rm.get(t)
		if status != http.StatusOK || !bytes.Equal(body, doc) {
			t.Fatalf("request %d: status %d, %d bytes; want 200 and the %d-byte document", i+1, status, len(body), len(doc))
		}
	}
	if got := rm.full.Load(); got != 1 {
		t.Errorf("upstream sent the document %d times, want once and then a cache hit", got)
	}
}

// When upstream confirms a relayed document with 304, the stored copy is
// served without being read into memory, so one over metadata_max_size is
// neither refused nor fetched again.
func TestRelayedMetadataRevalidatedWithoutBuffering(t *testing.T) {
	doc := bytes.Repeat([]byte("y"), 4096)
	rm := newRelayedMetadata(t, doc, 1024, "identity")
	if status, _, _ := rm.get(t); status != http.StatusOK {
		t.Fatalf("populating the cache: status %d", status)
	}
	rm.expire(t)

	status, body, _ := rm.get(t)
	if status != http.StatusOK || !bytes.Equal(body, doc) {
		t.Fatalf("after revalidation: status %d, %d bytes; want 200 and the document", status, len(body))
	}
	if rm.full.Load() != 1 || rm.conditional.Load() != 1 {
		t.Errorf("upstream sent the document %d times and 304 %d times, want once each", rm.full.Load(), rm.conditional.Load())
	}
}

// A document relayed gzip-compressed is stored and served as upstream sent
// it, and revalidated with its ETag like an uncompressed one.
func TestRelayedGzipMetadataIsRevalidated(t *testing.T) {
	doc := bytes.Repeat([]byte("z"), 4096)
	rm := newRelayedMetadata(t, doc, 1<<20, "gzip")
	want := gzipBytes(t, doc)

	for i, expire := range []bool{false, true} {
		if expire {
			rm.expire(t)
		}
		status, body, encoding := rm.get(t)
		if status != http.StatusOK || encoding != "gzip" || !bytes.Equal(body, want) {
			t.Fatalf("request %d: status %d, encoding %q, %d bytes; want 200 and the gzip bytes upstream sent", i+1, status, encoding, len(body))
		}
	}
	if rm.full.Load() != 1 || rm.conditional.Load() != 1 {
		t.Errorf("upstream sent the document %d times and 304 %d times, want once each", rm.full.Load(), rm.conditional.Load())
	}
}

// A relayed document records the URL it was fetched from, and a 304 records
// it on a row that has none, as the OS index refresh lists documents by it.
func TestRelayedMetadataRecordsUpstreamURL(t *testing.T) {
	rm := newRelayedMetadata(t, []byte("doc"), 1<<20, "identity")
	upstreamURL := func() string {
		t.Helper()
		entry, err := rm.proxy.DB.GetMetadataCache("test", "doc")
		if err != nil || entry == nil {
			t.Fatalf("no cache row: %v", err)
		}
		return entry.UpstreamURL.String
	}

	rm.get(t)
	if got := upstreamURL(); got != rm.upstreamURL {
		t.Fatalf("upstream_url = %q, want %q", got, rm.upstreamURL)
	}
	if _, err := rm.proxy.DB.Exec(`UPDATE metadata_cache SET upstream_url = NULL`); err != nil {
		t.Fatal(err)
	}
	rm.expire(t)
	rm.get(t)
	if rm.conditional.Load() != 1 {
		t.Fatalf("upstream answered 304 %d times, want once", rm.conditional.Load())
	}
	if got := upstreamURL(); got != rm.upstreamURL {
		t.Errorf("upstream_url after a 304 = %q, want %q", got, rm.upstreamURL)
	}
}

// A relayed document counts one cache miss when it is fetched and one hit
// each time the stored copy is served.
func TestRelayedMetadataCountsHitsOnce(t *testing.T) {
	rm := newRelayedMetadata(t, bytes.Repeat([]byte("h"), 4096), 1024, "identity")
	hits := func() float64 {
		return testutil.ToFloat64(metrics.CacheHits.WithLabelValues("test", metrics.CacheKindMetadata))
	}
	misses := func() float64 {
		return testutil.ToFloat64(metrics.CacheMisses.WithLabelValues("test", metrics.CacheKindMetadata))
	}
	hits0, misses0 := hits(), misses()

	rm.get(t)
	if h, m := hits()-hits0, misses()-misses0; h != 0 || m != 1 {
		t.Errorf("after the fetch: %v hits and %v misses, want 0 and 1", h, m)
	}
	rm.get(t)
	if h, m := hits()-hits0, misses()-misses0; h != 1 || m != 1 {
		t.Errorf("after the hit: %v hits and %v misses, want 1 and 1", h, m)
	}
}
