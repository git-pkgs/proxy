package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/metrics"
	"github.com/git-pkgs/proxy/internal/storage"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakeStorage is a minimal storage.Storage for probe tests with per-step failure injection.
type fakeStorage struct {
	mu sync.Mutex

	storeCalls  atomic.Int64
	openCalls   atomic.Int64
	closeCalls  atomic.Int64
	deleteCalls atomic.Int64

	paths    []string
	payloads [][]byte

	// Failure injection.
	storeErr  error
	openErr   error
	readErr   error  // returned by the io.ReadCloser.Read after partial bytes
	deleteErr error

	// Misbehavior knobs.
	sizeDelta    int64  // added to the reported size from Store
	readOverride []byte // if non-nil, Open returns a reader yielding these bytes instead of stored content

	// storeBlock, if non-nil, causes Store to block until the channel is closed or ctx is done.
	storeBlock chan struct{}

	stored map[string][]byte
}

func newFakeStorage() *fakeStorage { return &fakeStorage{stored: map[string][]byte{}} }

func (f *fakeStorage) Store(ctx context.Context, path string, r io.Reader) (int64, string, error) {
	f.storeCalls.Add(1)
	if f.storeErr != nil {
		return 0, "", f.storeErr
	}
	if f.storeBlock != nil {
		select {
		case <-f.storeBlock:
		case <-ctx.Done():
			return 0, "", ctx.Err()
		}
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return 0, "", err
	}
	f.mu.Lock()
	f.stored[path] = data
	f.paths = append(f.paths, path)
	f.payloads = append(f.payloads, data)
	f.mu.Unlock()
	return int64(len(data)) + f.sizeDelta, "fakehash", nil
}

type fakeReadCloser struct {
	data    []byte
	pos     int
	readErr error
	closed  *atomic.Int64
}

func (rc *fakeReadCloser) Read(p []byte) (int, error) {
	if rc.pos >= len(rc.data) {
		if rc.readErr != nil {
			return 0, rc.readErr
		}
		return 0, io.EOF
	}
	n := copy(p, rc.data[rc.pos:])
	rc.pos += n
	if rc.pos >= len(rc.data) && rc.readErr != nil {
		return n, rc.readErr
	}
	return n, nil
}

func (rc *fakeReadCloser) Close() error { rc.closed.Add(1); return nil }

func (f *fakeStorage) Open(ctx context.Context, path string) (io.ReadCloser, error) {
	f.openCalls.Add(1)
	if f.openErr != nil {
		return nil, f.openErr
	}
	f.mu.Lock()
	data := f.stored[path]
	f.mu.Unlock()
	if f.readOverride != nil {
		data = f.readOverride
	}
	return &fakeReadCloser{data: data, readErr: f.readErr, closed: &f.closeCalls}, nil
}

func (f *fakeStorage) Exists(ctx context.Context, path string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.stored[path]
	return ok, nil
}

func (f *fakeStorage) Delete(ctx context.Context, path string) error {
	f.deleteCalls.Add(1)
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.mu.Lock()
	delete(f.stored, path)
	f.mu.Unlock()
	return nil
}

func (f *fakeStorage) Size(ctx context.Context, path string) (int64, error) { return 0, nil }
func (f *fakeStorage) SignedURL(ctx context.Context, path string, expiry time.Duration) (string, error) {
	return "", storage.ErrSignedURLUnsupported
}
func (f *fakeStorage) UsedSpace(ctx context.Context) (int64, error) { return 0, nil }
func (f *fakeStorage) URL() string                                   { return "fake://" }
func (f *fakeStorage) Close() error                                  { return nil }

// --- Tests follow. First test: happy path ---

func TestStorageProbe_HappyPath(t *testing.T) {
	fs := newFakeStorage()
	if err := storageProbe(context.Background(), fs); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := fs.storeCalls.Load(); got != 1 {
		t.Errorf("Store calls = %d, want 1", got)
	}
	if got := fs.openCalls.Load(); got != 1 {
		t.Errorf("Open calls = %d, want 1", got)
	}
	if got := fs.closeCalls.Load(); got != 1 {
		t.Errorf("Close calls = %d, want 1", got)
	}
	if got := fs.deleteCalls.Load(); got != 1 {
		t.Errorf("Delete calls = %d, want 1", got)
	}
	if len(fs.paths) != 1 || !strings.HasPrefix(fs.paths[0], ".healthcheck/") {
		t.Errorf("unexpected probe path: %v", fs.paths)
	}
}

func TestStorageProbe_WriteFails(t *testing.T) {
	fs := newFakeStorage()
	fs.storeErr = errors.New("disk full")
	err := storageProbe(context.Background(), fs)
	var pe *probeError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *probeError, got %T: %v", err, err)
	}
	if pe.step != "write" {
		t.Errorf("step = %q, want write", pe.step)
	}
	if fs.openCalls.Load() != 0 {
		t.Errorf("Open should not be called after write failure")
	}
}

func TestStorageProbe_SizeMismatch(t *testing.T) {
	fs := newFakeStorage()
	fs.sizeDelta = -1 // Report 1 byte fewer than actually written
	err := storageProbe(context.Background(), fs)
	var pe *probeError
	if !errors.As(err, &pe) || pe.step != "size" {
		t.Fatalf("step = %v, want size; err = %v", pe, err)
	}
	if fs.openCalls.Load() != 0 {
		t.Errorf("Open should not be called after size mismatch")
	}
}

func TestStorageProbe_OpenFails(t *testing.T) {
	fs := newFakeStorage()
	fs.openErr = errors.New("access denied")
	err := storageProbe(context.Background(), fs)
	var pe *probeError
	if !errors.As(err, &pe) || pe.step != "read" {
		t.Fatalf("step = %v, want read; err = %v", pe, err)
	}
}

func TestStorageProbe_ReadMidStreamFails(t *testing.T) {
	fs := newFakeStorage()
	fs.readErr = errors.New("connection reset")
	err := storageProbe(context.Background(), fs)
	var pe *probeError
	if !errors.As(err, &pe) || pe.step != "read" {
		t.Fatalf("step = %v, want read (NOT verify); err = %v", pe, err)
	}
}

func TestStorageProbe_ContentMismatch(t *testing.T) {
	fs := newFakeStorage()
	fs.readOverride = []byte("wrong content")
	err := storageProbe(context.Background(), fs)
	var pe *probeError
	if !errors.As(err, &pe) || pe.step != "verify" {
		t.Fatalf("step = %v, want verify; err = %v", pe, err)
	}
}

func TestStorageProbe_DeleteFails(t *testing.T) {
	fs := newFakeStorage()
	fs.deleteErr = errors.New("permission denied")
	err := storageProbe(context.Background(), fs)
	var pe *probeError
	if !errors.As(err, &pe) || pe.step != "delete" {
		t.Fatalf("step = %v, want delete; err = %v", pe, err)
	}
}

// TestStorageProbe_CleanupOnNonDeleteFailure asserts that the probe object is
// deleted even when a step after Store (size/open/read/verify) fails, so
// probe artifacts don't accumulate in the storage backend.
func TestStorageProbe_CleanupOnNonDeleteFailure(t *testing.T) {
	cases := []struct {
		name    string
		inject  func(*fakeStorage)
		wantErr string
	}{
		{"size mismatch", func(fs *fakeStorage) { fs.sizeDelta = -1 }, "size"},
		{"open fails", func(fs *fakeStorage) { fs.openErr = errors.New("open boom") }, "read"},
		{"read mid-stream", func(fs *fakeStorage) { fs.readErr = errors.New("mid-stream boom") }, "read"},
		{"content mismatch", func(fs *fakeStorage) { fs.readOverride = []byte("wrong") }, "verify"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeStorage()
			tc.inject(fs)
			err := storageProbe(context.Background(), fs)
			var pe *probeError
			if !errors.As(err, &pe) || pe.step != tc.wantErr {
				t.Fatalf("step = %v, want %q; err = %v", pe, tc.wantErr, err)
			}
			if got := fs.deleteCalls.Load(); got != 1 {
				t.Errorf("deleteCalls = %d, want 1 (cleanup should run on non-delete failures)", got)
			}
		})
	}
}

func TestStorageProbe_ReaderClosedOnReadFailure(t *testing.T) {
	fs := newFakeStorage()
	fs.readErr = errors.New("read error")
	_ = storageProbe(context.Background(), fs)
	if got := fs.closeCalls.Load(); got != fs.openCalls.Load() {
		t.Errorf("closeCalls = %d, openCalls = %d (should match)", got, fs.openCalls.Load())
	}
}

func TestStorageProbe_PathUniqueness(t *testing.T) {
	fs := newFakeStorage()
	for i := 0; i < 100; i++ {
		if err := storageProbe(context.Background(), fs); err != nil {
			t.Fatalf("probe %d: %v", i, err)
		}
	}
	seen := make(map[string]bool)
	for _, p := range fs.paths {
		if !strings.HasPrefix(p, ".healthcheck/") {
			t.Errorf("path missing prefix: %q", p)
		}
		if seen[p] {
			t.Errorf("duplicate path: %q", p)
		}
		seen[p] = true
	}
}

// helper: a healthCache wired to a fakeStorage and a discard logger.
func newTestCache(fs *fakeStorage, interval time.Duration) *healthCache {
	return &healthCache{
		storage:      fs,
		interval:     interval,
		probeTimeout: 5 * time.Second,
		logger:       discardLogger(),
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestHealthCache_CacheHit(t *testing.T) {
	fs := newFakeStorage()
	c := newTestCache(fs, 30*time.Second)
	if err := c.Check(context.Background()); err != nil {
		t.Fatalf("first check: %v", err)
	}
	if err := c.Check(context.Background()); err != nil {
		t.Fatalf("second check: %v", err)
	}
	if got := fs.storeCalls.Load(); got != 1 {
		t.Errorf("storeCalls = %d, want 1 (second call should be cached)", got)
	}
}

func TestHealthCache_MissAfterTTL(t *testing.T) {
	fs := newFakeStorage()
	c := newTestCache(fs, 10*time.Millisecond)
	_ = c.Check(context.Background())
	time.Sleep(20 * time.Millisecond)
	_ = c.Check(context.Background())
	if got := fs.storeCalls.Load(); got != 2 {
		t.Errorf("storeCalls = %d, want 2", got)
	}
}

func TestHealthCache_Disabled(t *testing.T) {
	fs := newFakeStorage()
	c := newTestCache(fs, 0) // interval = 0 means probe every call
	_ = c.Check(context.Background())
	_ = c.Check(context.Background())
	if got := fs.storeCalls.Load(); got != 2 {
		t.Errorf("storeCalls = %d, want 2", got)
	}
}

func TestHealthCache_LastAtNotAdvancedOnHit(t *testing.T) {
	fs := newFakeStorage()
	c := newTestCache(fs, 30*time.Second)
	for i := 0; i < 100; i++ {
		_ = c.Check(context.Background())
	}
	if got := fs.storeCalls.Load(); got != 1 {
		t.Errorf("storeCalls = %d, want 1 across 100 hits", got)
	}
}

func TestHealthCache_ConcurrentSingleFlight(t *testing.T) {
	fs := newFakeStorage()
	c := newTestCache(fs, 30*time.Second)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = c.Check(context.Background()) }()
	}
	wg.Wait()
	if got := fs.storeCalls.Load(); got != 1 {
		t.Errorf("storeCalls = %d, want 1 with 20 concurrent callers", got)
	}
}

func TestHealthCache_CallerCancellationNotPoisoning(t *testing.T) {
	fs := newFakeStorage()
	c := newTestCache(fs, 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Already cancelled before the call
	if err := c.Check(ctx); err != nil {
		t.Fatalf("Check with cancelled caller ctx should still succeed: %v", err)
	}
}

func TestHealthCache_FailureCounterIncrement(t *testing.T) {
	fs := newFakeStorage()
	fs.storeErr = errors.New("boom")
	c := newTestCache(fs, 30*time.Second)

	before := testutil.ToFloat64(metrics.HealthProbeFailures.WithLabelValues("write"))

	// First call: fresh probe → counter +1
	_ = c.Check(context.Background())
	afterFirst := testutil.ToFloat64(metrics.HealthProbeFailures.WithLabelValues("write"))
	if afterFirst-before != 1 {
		t.Errorf("counter delta after first call = %v, want 1", afterFirst-before)
	}

	// Second call: cache hit → counter NOT re-incremented
	_ = c.Check(context.Background())
	afterSecond := testutil.ToFloat64(metrics.HealthProbeFailures.WithLabelValues("write"))
	if afterSecond != afterFirst {
		t.Errorf("counter changed on cache hit: %v → %v", afterFirst, afterSecond)
	}
}

func TestHealthCache_ProbeTimeout(t *testing.T) {
	fs := newFakeStorage()
	fs.storeBlock = make(chan struct{}) // Store will block until channel is closed (or never)
	t.Cleanup(func() { close(fs.storeBlock) })

	c := &healthCache{
		storage:      fs,
		interval:     30 * time.Second,
		probeTimeout: 50 * time.Millisecond,
		logger:       discardLogger(),
	}
	start := time.Now()
	err := c.Check(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("probe took %v, expected ~50ms (timeout not respected)", elapsed)
	}
}

func TestHealthCache_TransitionLogging(t *testing.T) {
	fs := newFakeStorage()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	c := &healthCache{
		storage:      fs,
		interval:     0, // probe every call
		probeTimeout: 5 * time.Second,
		logger:       logger,
	}

	// Steady ok state — should not log
	_ = c.Check(context.Background())
	_ = c.Check(context.Background())
	if got := strings.Count(buf.String(), "storage probe"); got != 0 {
		t.Errorf("steady-state logs = %d, want 0; output: %s", got, buf.String())
	}

	// ok → err transition: exactly one Error log
	buf.Reset()
	fs.storeErr = errors.New("boom")
	_ = c.Check(context.Background())
	if !strings.Contains(buf.String(), "storage probe failed") {
		t.Errorf("missing failure log on transition; output: %s", buf.String())
	}

	// err steady state — should not log again
	buf.Reset()
	_ = c.Check(context.Background())
	if buf.Len() != 0 {
		t.Errorf("steady-err logs = %q, want empty", buf.String())
	}

	// err → ok transition: exactly one Info log
	buf.Reset()
	fs.storeErr = nil
	_ = c.Check(context.Background())
	if !strings.Contains(buf.String(), "storage probe recovered") {
		t.Errorf("missing recovery log on transition; output: %s", buf.String())
	}
}
