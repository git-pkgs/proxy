package handler

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// gzipPayload returns a gzip-compressed copy of data, simulating an origin
// that stores pre-compressed index files.
func gzipPayload(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatalf("compressing payload: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing gzip writer: %v", err)
	}
	return buf.Bytes()
}

// TestProxyCached_PreservesContentEncodedBytes covers issue #300: an upstream
// that serves a signed index with Content-Encoding: gzip must have its bytes
// cached and re-served verbatim, with the encoding header replayed, instead of
// being transparently decompressed by the HTTP client.
func TestProxyCached_PreservesContentEncodedBytes(t *testing.T) {
	raw := gzipPayload(t, []byte("signed index payload"))

	var available atomic.Bool
	available.Store(true)
	var sawAcceptEncoding atomic.Value
	var upstreamRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !available.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		upstreamRequests.Add(1)
		sawAcceptEncoding.Store(r.Header.Get(headerAcceptEncoding))
		w.Header().Set(headerContentType, "application/octet-stream")
		w.Header().Set(headerContentEncoding, "gzip")
		_, _ = w.Write(raw)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.HTTPClient = upstream.Client()

	serve := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/index", nil)
		proxy.ProxyCached(w, r, upstream.URL+"/index", "apk", "index-key", "*/*")
		return w
	}

	first := serve()
	if first.Code != http.StatusOK {
		t.Fatalf("first response status = %d, want 200: %s", first.Code, first.Body.String())
	}
	if got, _ := sawAcceptEncoding.Load().(string); got != "identity" {
		t.Errorf("upstream saw Accept-Encoding %q, want %q", got, "identity")
	}
	if !bytes.Equal(first.Body.Bytes(), raw) {
		t.Errorf("first response altered the upstream bytes: got %d bytes, want %d", first.Body.Len(), len(raw))
	}
	if got := first.Header().Get(headerContentEncoding); got != "gzip" {
		t.Errorf("first response Content-Encoding = %q, want %q", got, "gzip")
	}

	// Within the TTL and with the upstream down, the cached copy must be
	// served with the same bytes and encoding.
	available.Store(false)
	second := serve()
	if second.Code != http.StatusOK {
		t.Fatalf("cached response status = %d, want 200: %s", second.Code, second.Body.String())
	}
	if !bytes.Equal(second.Body.Bytes(), raw) {
		t.Errorf("cached response altered the stored bytes")
	}
	if got := second.Header().Get(headerContentEncoding); got != "gzip" {
		t.Errorf("cached response Content-Encoding = %q, want %q", got, "gzip")
	}
	if got := upstreamRequests.Load(); got != 1 {
		t.Errorf("upstream requests = %d, want 1", got)
	}
}

// TestProxyCached_NoEncodingHeaderForIdentityResponses pins that ordinary
// responses do not grow a spurious Content-Encoding header.
func TestProxyCached_NoEncodingHeaderForIdentityResponses(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(headerContentType, contentTypeJSON)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.HTTPClient = upstream.Client()

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/meta", nil)
	proxy.ProxyCached(w, r, upstream.URL+"/meta", "npm", "meta-key", contentTypeJSON)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get(headerContentEncoding); got != "" {
		t.Errorf("Content-Encoding = %q, want empty", got)
	}
}

// TestProxyMetadataStream_PreservesSignedBytesWithoutClientEncoding pins the
// uncached streaming path (the default, since cache_metadata is off) for the
// realistic client that sends no Accept-Encoding: the proxy must request
// identity upstream so Go does not transparently decompress a signed index,
// and the raw bytes plus the Content-Encoding header must reach the client.
func TestProxyMetadataStream_PreservesSignedBytesWithoutClientEncoding(t *testing.T) {
	raw := gzipPayload(t, []byte("streamed index payload"))
	var sawAcceptEncoding string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAcceptEncoding = r.Header.Get(headerAcceptEncoding)
		w.Header().Set(headerContentType, "application/octet-stream")
		w.Header().Set(headerContentEncoding, "gzip")
		_, _ = w.Write(raw)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = false
	proxy.HTTPClient = upstream.Client()

	w := httptest.NewRecorder()
	// No Accept-Encoding on the client request -- the apk/apt/dnf case.
	r := httptest.NewRequest(http.MethodGet, "/index", nil)
	proxy.ProxyCached(w, r, upstream.URL+"/index", "apk", "stream-key", "*/*")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if sawAcceptEncoding != "identity" {
		t.Errorf("upstream saw Accept-Encoding %q, want %q", sawAcceptEncoding, "identity")
	}
	if !bytes.Equal(w.Body.Bytes(), raw) {
		t.Errorf("streamed response altered the upstream bytes: got %d bytes, want %d", w.Body.Len(), len(raw))
	}
	if got := w.Header().Get(headerContentEncoding); got != "gzip" {
		t.Errorf("Content-Encoding = %q, want %q", got, "gzip")
	}
}

// TestFetchOrCacheMetadata_DirectCallersKeepTransparentCompression pins that
// the parsing/rewriting ecosystems (npm, pypi, cargo, helm, ...) that call
// FetchOrCacheMetadata directly are NOT forced to identity: they keep Go's
// transparent transfer compression and receive decoded bytes, so a gzip-only
// upstream does not regress them (no wire-size blowup, no parse failures).
func TestFetchOrCacheMetadata_DirectCallersKeepTransparentCompression(t *testing.T) {
	plaintext := []byte(`{"name":"demo","versions":{"1.0.0":{}}}`)
	var sawAcceptEncoding string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAcceptEncoding = r.Header.Get(headerAcceptEncoding)
		// Serve gzip only when the client accepts it, like a real CDN.
		if strings.Contains(r.Header.Get(headerAcceptEncoding), "gzip") {
			w.Header().Set(headerContentType, contentTypeJSON)
			w.Header().Set(headerContentEncoding, "gzip")
			_, _ = w.Write(gzipPayload(t, plaintext))
			return
		}
		w.Header().Set(headerContentType, contentTypeJSON)
		_, _ = w.Write(plaintext)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.HTTPClient = upstream.Client()

	body, _, err := proxy.FetchOrCacheMetadata(t.Context(), "npm", "demo", upstream.URL+"/demo", contentTypeJSON)
	if err != nil {
		t.Fatalf("FetchOrCacheMetadata() error = %v", err)
	}
	// The default transport adds Accept-Encoding: gzip and transparently
	// decompresses, so the caller sees decoded JSON regardless of the wire form.
	if sawAcceptEncoding == "identity" {
		t.Errorf("direct caller forced identity; want transparent compression")
	}
	if !bytes.Equal(body, plaintext) {
		t.Errorf("direct caller got %q, want decoded %q", body, plaintext)
	}
}
