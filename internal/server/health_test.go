package server

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/storage"
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

	stored map[string][]byte
}

func newFakeStorage() *fakeStorage { return &fakeStorage{stored: map[string][]byte{}} }

func (f *fakeStorage) Store(ctx context.Context, path string, r io.Reader) (int64, string, error) {
	f.storeCalls.Add(1)
	if f.storeErr != nil {
		return 0, "", f.storeErr
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
