package handler

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/storage"
)

// stallingStorage hands out cached metadata readers that return the first
// head bytes and then block until release closes, so a test can see whether a
// response starts before the whole document has been read.
type stallingStorage struct {
	storage.Storage
	head    int
	release chan struct{}
}

func (s *stallingStorage) Open(ctx context.Context, path string) (io.ReadCloser, error) {
	rc, err := s.Storage.Open(ctx, path)
	if err != nil || !strings.HasPrefix(path, "_metadata/") {
		return rc, err
	}
	return &stallingReader{ReadCloser: rc, left: s.head, release: s.release}, nil
}

type stallingReader struct {
	io.ReadCloser
	left    int
	release chan struct{}
}

func (r *stallingReader) Read(p []byte) (int, error) {
	if r.left == 0 {
		<-r.release
		return r.ReadCloser.Read(p)
	}
	if len(p) > r.left {
		p = p[:r.left]
	}
	n, err := r.ReadCloser.Read(p)
	r.left -= n
	return n, err
}

// cachedMetadataServer serves ProxyCached for one document, cached on the
// first request.
func cachedMetadataServer(t *testing.T, doc []byte) (*Proxy, *mockStorage, *httptest.Server) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(headerContentType, "application/x-test")
		w.Header().Set(headerETag, `"v1"`)
		_, _ = w.Write(doc)
	}))
	t.Cleanup(upstream.Close)

	proxy, _, store, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.HTTPClient = upstream.Client()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy.ProxyCached(w, r, upstream.URL+"/doc", "test", "doc", "*/*")
	}))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("populating cache: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("populating cache: status %d", resp.StatusCode)
	}
	return proxy, store, srv
}

func TestConcurrentMetadataCacheHitsStream(t *testing.T) {
	const callers = 10
	doc := bytes.Repeat([]byte("0123456789abcdef"), 1<<18) // 4 MB
	proxy, store, srv := cachedMetadataServer(t, doc)

	gate := &stallingStorage{Storage: store, head: 64 << 10, release: make(chan struct{})}
	proxy.Storage = gate
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate.release) }) }
	t.Cleanup(release)

	// Every hit has to start its response while its reader is stalled after
	// the first 64 KB. A hit that reads the document into memory first can't
	// send anything until release.
	type result struct {
		resp  *http.Response
		first []byte
		err   error
	}
	started := make(chan result, callers)
	for range callers {
		go func() {
			resp, err := http.Get(srv.URL)
			if err != nil {
				started <- result{err: err}
				return
			}
			first := make([]byte, 1024)
			_, err = io.ReadFull(resp.Body, first)
			started <- result{resp: resp, first: first, err: err}
		}()
	}
	results := make([]result, 0, callers)
	timeout := time.After(5 * time.Second)
	streamed := 0
	for len(results) < callers {
		select {
		case res := <-started:
			results = append(results, res)
		case <-timeout:
			// Let the stalled hits finish, so the server can shut down.
			release()
			timeout = nil
			streamed = len(results)
		}
	}
	release()
	t.Cleanup(func() {
		for _, res := range results {
			if res.resp != nil {
				_ = res.resp.Body.Close()
			}
		}
	})
	if timeout == nil {
		t.Fatalf("%d of %d concurrent cache hits started their response before the document was read to its end", streamed, callers)
	}

	for _, res := range results {
		if res.err != nil {
			t.Fatalf("cache hit: %v", res.err)
		}
		rest, err := io.ReadAll(res.resp.Body)
		_ = res.resp.Body.Close()
		if err != nil {
			t.Fatalf("reading cache hit: %v", err)
		}
		if body := slices.Concat(res.first, rest); !bytes.Equal(body, doc) {
			t.Fatalf("cache hit returned %d bytes, want the %d-byte document", len(body), len(doc))
		}
		if got := res.resp.Header.Get(headerContentType); got != "application/x-test" {
			t.Errorf("Content-Type = %q, want application/x-test", got)
		}
		if got := res.resp.Header.Get(headerContentLength); got != strconv.Itoa(len(doc)) {
			t.Errorf("Content-Length = %q, want %d", got, len(doc))
		}
		if got := res.resp.Header.Get(headerETag); got != `"v1"` {
			t.Errorf("ETag = %q, want %q", got, `"v1"`)
		}
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("If-None-Match", `"v1"`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("conditional cache hit: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotModified {
		t.Errorf("conditional cache hit: status %d, want 304", resp.StatusCode)
	}
}

func TestCachedMetadataHitAbortsOnDigestMismatch(t *testing.T) {
	doc := bytes.Repeat([]byte("metadata "), 1<<14)
	_, store, srv := cachedMetadataServer(t, doc)

	corrupt := bytes.Clone(doc)
	corrupt[len(corrupt)-1] ^= 0xff
	store.mu.Lock()
	store.files[metadataStoragePath("test", "doc")] = corrupt
	store.mu.Unlock()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("cache hit: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Fatalf("cache hit completed with %d bytes of a document that doesn't match its digest, want the response aborted", len(body))
	}
	if len(body) >= len(doc) {
		t.Errorf("aborted response still delivered %d of %d bytes", len(body), len(doc))
	}
}
