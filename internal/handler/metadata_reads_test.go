package handler

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/storage"
)

// openGateStorage counts opens of cached metadata and holds each until
// release closes.
type openGateStorage struct {
	storage.Storage
	opens   atomic.Int32
	release chan struct{}
}

func (s *openGateStorage) Open(ctx context.Context, path string) (io.ReadCloser, error) {
	if strings.HasPrefix(path, "_metadata/") {
		s.opens.Add(1)
		<-s.release
	}
	return s.Storage.Open(ctx, path)
}

// metadataReadWaiters reports how many callers wait for a shared read.
func metadataReadWaiters(p *Proxy) int {
	p.metaReads.mu.Lock()
	defer p.metaReads.mu.Unlock()
	n := 0
	for _, r := range p.metaReads.reads {
		n += r.waiters
	}
	return n
}

func TestConcurrentMetadataCacheHitsShareOneRead(t *testing.T) {
	const callers = 20
	doc := bytes.Repeat([]byte("x"), 1<<20)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(doc)
	}))
	t.Cleanup(upstream.Close)

	proxy, _, store, _ := setupTestProxy(t)
	gate := &openGateStorage{Storage: store, release: make(chan struct{})}
	proxy.Storage = gate
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.HTTPClient = upstream.Client()
	ctx := context.Background()

	if _, _, err := proxy.FetchOrCacheMetadata(ctx, "test", "big", upstream.URL); err != nil {
		t.Fatalf("populating cache: %v", err)
	}

	bodies := make([][]byte, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			body, _, err := proxy.FetchOrCacheMetadata(ctx, "test", "big", upstream.URL)
			if err != nil {
				t.Errorf("cache hit %d: %v", i, err)
			}
			bodies[i] = body
		})
	}
	// Hold the read until every caller has either joined it or opened the
	// document itself.
	deadline := time.Now().Add(5 * time.Second)
	for int(gate.opens.Load())+metadataReadWaiters(proxy) < callers && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(gate.release)
	wg.Wait()

	if got := gate.opens.Load(); got != 1 {
		t.Errorf("%d concurrent cache hits opened the document %d times, want 1", callers, got)
	}
	for i, body := range bodies {
		if !bytes.Equal(body, doc) {
			t.Fatalf("cache hit %d returned %d bytes, want the %d-byte document", i, len(body), len(doc))
		}
		if &body[0] != &bodies[0][0] {
			t.Errorf("cache hit %d got its own copy of the document", i)
		}
	}

	// A request after the read finished reuses the bytes while earlier
	// requests still hold them.
	body, _, err := proxy.FetchOrCacheMetadata(ctx, "test", "big", upstream.URL)
	if err != nil {
		t.Fatalf("later cache hit: %v", err)
	}
	if got := gate.opens.Load(); got != 1 {
		t.Errorf("later cache hit opened the document again (%d opens), want it to reuse the held copy", got)
	}
	if &body[0] != &bodies[0][0] {
		t.Error("later cache hit got its own copy of the document")
	}
	runtime.KeepAlive(bodies)

	// Once nobody holds the document its entry goes away, and the next hit
	// reads it again.
	bodies, body = nil, nil
	for i := 0; i < 50 && metadataReadEntries(proxy) > 0; i++ {
		runtime.GC()
		time.Sleep(time.Millisecond)
	}
	if n := metadataReadEntries(proxy); n != 0 {
		t.Fatalf("%d shared read entries left after the document was dropped", n)
	}
	body, _, err = proxy.FetchOrCacheMetadata(ctx, "test", "big", upstream.URL)
	if err != nil || !bytes.Equal(body, doc) {
		t.Fatalf("cache hit after drop: %d bytes, err %v", len(body), err)
	}
	if got := gate.opens.Load(); got != 2 {
		t.Errorf("opens after the document was dropped = %d, want 2", got)
	}
}

func metadataReadEntries(p *Proxy) int {
	p.metaReads.mu.Lock()
	defer p.metaReads.mu.Unlock()
	return len(p.metaReads.reads)
}
