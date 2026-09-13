package handler

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/storage"
	"github.com/git-pkgs/registries/fetch"
)

// fetchHoldTime holds each stub fetch open long enough that concurrent callers
// reliably overlap inside it. The exact value is not significant.
const fetchHoldTime = 50 * time.Millisecond

// countingFetcher counts upstream fetches and holds each one open.
type countingFetcher struct {
	calls   atomic.Int64
	content string
	delay   time.Duration
}

func (f *countingFetcher) Fetch(ctx context.Context, url string) (*fetch.Artifact, error) {
	return f.FetchWithHeaders(ctx, url, nil)
}

func (f *countingFetcher) FetchWithHeaders(_ context.Context, _ string, _ http.Header) (*fetch.Artifact, error) {
	f.calls.Add(1)
	time.Sleep(f.delay)
	return &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader(f.content)),
		ContentType: "application/gzip",
	}, nil
}

func (f *countingFetcher) Head(context.Context, string) (int64, string, error) {
	return 0, "", nil
}

// TestGetOrFetchArtifactFromURL_ConcurrentMissesCoalesce asserts that N
// simultaneous misses for one artifact produce a single upstream fetch. That is
// the CI shape: parallel jobs installing overlapping dependencies cold.
func TestGetOrFetchArtifactFromURL_ConcurrentMissesCoalesce(t *testing.T) {
	const goroutines = 8
	const content = "left-pad tarball bytes"

	proxy, _, _, _ := setupTestProxy(t)
	fetcher := &countingFetcher{content: content, delay: fetchHoldTime}
	proxy.Fetcher = fetcher

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	bodies := make([]string, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, err := proxy.GetOrFetchArtifactFromURL(context.Background(),
				"npm", "left-pad", "1.3.0", "left-pad-1.3.0.tgz",
				"https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz")
			if err != nil {
				errs[i] = err
				return
			}
			defer func() { _ = res.Reader.Close() }()
			b, err := io.ReadAll(res.Reader)
			errs[i] = err
			bodies[i] = string(b)
		}(i)
	}

	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: unexpected error: %v", i, err)
		}
	}
	// Every caller must get its own intact copy of the bytes.
	for i, b := range bodies {
		if b != content {
			t.Errorf("goroutine %d: body = %q, want %q", i, b, content)
		}
	}
	if got := fetcher.calls.Load(); got != 1 {
		t.Errorf("upstream fetches = %d, want 1 (%d concurrent callers stampeded the upstream)", got, goroutines)
	}
}

// TestGetOrFetchArtifactFromURL_ConcurrentMissesFileStorage runs the same
// scenario against the real file:// backend, the default in production.
//
// Uncoalesced this fails outright, not merely wastefully. Every caller stores
// to one key, and fileblob rewrites a ".attrs" sidecar per key with os.Create,
// truncating in place outside the rename that protects the blob. Decoding that
// sidecar mid-truncate gives "opening reader: EOF", served as a 502.
//
// Only the fetcher is stubbed, because the real one refuses loopback so an
// httptest upstream is unreachable. The storage, where this fails, is real.
func TestGetOrFetchArtifactFromURL_ConcurrentMissesFileStorage(t *testing.T) {
	const goroutines = 16
	content := bytes.Repeat([]byte("tarball-bytes-"), 512)

	ctx := context.Background()
	dir := t.TempDir()

	db, err := database.Create(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store, err := storage.OpenBucket(ctx, "file://"+filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	fetcher := &countingFetcher{content: string(content), delay: fetchHoldTime}
	proxy := NewProxy(db, store, fetcher, fetch.NewResolver(),
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	bodies := make([][]byte, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, err := proxy.GetOrFetchArtifactFromURL(ctx,
				"npm", "left-pad", "1.3.0", "left-pad-1.3.0.tgz",
				"https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz")
			if err != nil {
				errs[i] = err
				return
			}
			defer func() { _ = res.Reader.Close() }()
			body, readErr := io.ReadAll(res.Reader)
			errs[i] = readErr
			bodies[i] = body
		}(i)
	}

	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d failed: %v", i, err)
		}
	}
	for i, body := range bodies {
		if !bytes.Equal(body, content) {
			t.Errorf("caller %d got %d bytes, want %d", i, len(body), len(content))
		}
	}
	if got := fetcher.calls.Load(); got != 1 {
		t.Errorf("upstream fetches = %d, want 1", got)
	}
}
