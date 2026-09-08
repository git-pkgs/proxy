package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gocloud.dev/blob"
)

func TestOpenBucket(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	b, err := OpenBucket(ctx, fileURLFromPath(dir))
	if err != nil {
		t.Fatalf("OpenBucket failed: %v", err)
	}
	defer func() { _ = b.Close() }()

	if b.URL() == "" {
		t.Error("URL() should not be empty")
	}
}

func TestBlobStore(t *testing.T) {
	b := createTestBlob(t)
	ctx := context.Background()
	content := "test content for blob storage"

	size, hash, err := b.Store(ctx, "npm/lodash/4.17.21/lodash.tgz", strings.NewReader(content))
	if err != nil {
		t.Fatalf("Store failed: %v", err)
	}

	if size != int64(len(content)) {
		t.Errorf("size = %d, want %d", size, len(content))
	}

	h := sha256.Sum256([]byte(content))
	wantHash := hex.EncodeToString(h[:])
	if hash != wantHash {
		t.Errorf("hash = %s, want %s", hash, wantHash)
	}
}

func TestBlobOpen(t *testing.T) {
	b := createTestBlob(t)
	ctx := context.Background()
	content := "readable content"

	_, _, _ = b.Store(ctx, "test/read.txt", strings.NewReader(content))

	r, err := b.Open(ctx, "test/read.txt")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer func() { _ = r.Close() }()

	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if string(data) != content {
		t.Errorf("content = %q, want %q", string(data), content)
	}
}

func TestBlobOpenNotFound(t *testing.T) {
	b := createTestBlob(t)
	ctx := context.Background()

	_, err := b.Open(ctx, "does/not/exist.txt")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Open non-existent = %v, want ErrNotFound", err)
	}
}

func TestBlobExists(t *testing.T) {
	b := createTestBlob(t)
	ctx := context.Background()

	exists, err := b.Exists(ctx, "test/exists.txt")
	if err != nil {
		t.Fatalf("Exists failed: %v", err)
	}
	if exists {
		t.Error("Exists returned true for non-existent file")
	}

	_, _, _ = b.Store(ctx, "test/exists.txt", strings.NewReader("content"))

	exists, err = b.Exists(ctx, "test/exists.txt")
	if err != nil {
		t.Fatalf("Exists after store failed: %v", err)
	}
	if !exists {
		t.Error("Exists returned false for existing file")
	}
}

func TestBlobDelete(t *testing.T) {
	b := createTestBlob(t)
	ctx := context.Background()

	_, _, _ = b.Store(ctx, "test/delete/nested/file.txt", strings.NewReader("content"))

	err := b.Delete(ctx, "test/delete/nested/file.txt")
	if err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	exists, _ := b.Exists(ctx, "test/delete/nested/file.txt")
	if exists {
		t.Error("file still exists after delete")
	}
}

func TestBlobDeleteNotFound(t *testing.T) {
	b := createTestBlob(t)
	ctx := context.Background()

	// Delete non-existent file should not error
	err := b.Delete(ctx, "does/not/exist.txt")
	if err != nil {
		t.Errorf("Delete non-existent = %v, want nil", err)
	}
}

func TestBlobSize(t *testing.T) {
	b := createTestBlob(t)
	ctx := context.Background()
	content := "size test content"

	_, _, _ = b.Store(ctx, "test/size.txt", strings.NewReader(content))

	size, err := b.Size(ctx, "test/size.txt")
	if err != nil {
		t.Fatalf("Size failed: %v", err)
	}
	if size != int64(len(content)) {
		t.Errorf("Size = %d, want %d", size, len(content))
	}
}

func TestBlobSizeNotFound(t *testing.T) {
	b := createTestBlob(t)
	ctx := context.Background()

	_, err := b.Size(ctx, "does/not/exist.txt")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Size non-existent = %v, want ErrNotFound", err)
	}
}

func TestBlobUsedSpace(t *testing.T) {
	b := createTestBlob(t)
	ctx := context.Background()

	// Empty storage
	used, err := b.UsedSpace(ctx)
	if err != nil {
		t.Fatalf("UsedSpace failed: %v", err)
	}
	if used != 0 {
		t.Errorf("UsedSpace empty = %d, want 0", used)
	}

	// Add some files
	_, _, _ = b.Store(ctx, "a.txt", strings.NewReader("aaaa"))    // 4 bytes
	_, _, _ = b.Store(ctx, "b.txt", strings.NewReader("bbbbbb"))  // 6 bytes
	_, _, _ = b.Store(ctx, "c/d.txt", strings.NewReader("ccccc")) // 5 bytes

	used, err = b.UsedSpace(ctx)
	if err != nil {
		t.Fatalf("UsedSpace failed: %v", err)
	}
	if used != 15 {
		t.Errorf("UsedSpace = %d, want 15", used)
	}
}

func TestBlobLargeFile(t *testing.T) {
	assertLargeFileRoundTrip(t, createTestBlob(t))
}

func TestBlobSignedURLUnsupported(t *testing.T) {
	b := createTestBlob(t)
	ctx := context.Background()

	// fileblob has no URL signer configured, so this must surface as
	// ErrSignedURLUnsupported rather than a generic error.
	_, err := b.SignedURL(ctx, "test/file.txt", time.Minute)
	if !errors.Is(err, ErrSignedURLUnsupported) {
		t.Errorf("SignedURL on fileblob = %v, want ErrSignedURLUnsupported", err)
	}
}

func TestBlobOverwrite(t *testing.T) {
	b := createTestBlob(t)
	ctx := context.Background()

	// Store initial content
	_, _, err := b.Store(ctx, "test/file.txt", strings.NewReader("initial"))
	if err != nil {
		t.Fatalf("initial Store failed: %v", err)
	}

	// Overwrite with new content
	_, _, err = b.Store(ctx, "test/file.txt", strings.NewReader("updated"))
	if err != nil {
		t.Fatalf("update Store failed: %v", err)
	}

	// Verify updated content
	r, err := b.Open(ctx, "test/file.txt")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer func() { _ = r.Close() }()

	data, _ := io.ReadAll(r)
	if string(data) != "updated" {
		t.Errorf("content = %q, want %q", string(data), "updated")
	}
}

func TestOpenBucketSetsNoTmpDir(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	b, err := OpenBucket(ctx, fileURLFromPath(dir))
	if err != nil {
		t.Fatalf("OpenBucket failed: %v", err)
	}
	defer func() { _ = b.Close() }()

	// fileblob uses os.TempDir() by default for temp files, then os.Rename to
	// the final path. This fails with "invalid cross-device link" when the bucket
	// dir and os.TempDir() are on different filesystems (e.g. Docker volumes).
	// OpenBucket must set no_tmp_dir=true so temp files are created next to the
	// final path instead.
	if !strings.Contains(b.URL(), "no_tmp_dir=true") {
		t.Errorf("URL should contain no_tmp_dir=true to avoid cross-device rename errors, got %q", b.URL())
	}

	// Verify Store still works with the parameter set
	content := "cross-device test"
	_, _, err = b.Store(ctx, "test/cross-device.txt", strings.NewReader(content))
	if err != nil {
		t.Fatalf("Store failed with no_tmp_dir=true: %v", err)
	}

	r, err := b.Open(ctx, "test/cross-device.txt")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer func() { _ = r.Close() }()

	data, _ := io.ReadAll(r)
	if string(data) != content {
		t.Errorf("content = %q, want %q", string(data), content)
	}
}

func createTestBlob(t *testing.T) *Blob {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()

	b, err := OpenBucket(ctx, fileURLFromPath(dir))
	if err != nil {
		t.Fatalf("OpenBucket failed: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	blob, ok := b.(*Blob)
	if !ok {
		t.Fatalf("OpenBucket returned %T, want *Blob", b)
	}
	return blob
}

func fileURLFromPath(path string) string {
	if runtime.GOOS == osWindows {
		// Windows paths need file:///C:/path format
		path = filepath.ToSlash(path)
		return "file:///" + path
	}
	return "file://" + path
}

func TestOpenBucketWritesNoAttrsSidecar(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	b, err := OpenBucket(ctx, fileURLFromPath(dir))
	if err != nil {
		t.Fatalf("OpenBucket failed: %v", err)
	}
	defer func() { _ = b.Close() }()

	if _, _, err := b.Store(ctx, "pkg/thing-1.0.0.tgz", strings.NewReader("content")); err != nil {
		t.Fatalf("Store failed: %v", err)
	}

	sidecars, err := filepath.Glob(filepath.Join(dir, "*", "*.attrs"))
	if err != nil {
		t.Fatalf("Glob failed: %v", err)
	}
	if len(sidecars) != 0 {
		t.Errorf("got sidecar files %v, want none: a truncated sidecar fails reads that overlap a write", sidecars)
	}
}

// A read overlapping a write to the same key must not fail. fileblob rewrote
// its ".attrs" sidecar in place, so a reader decoding it mid-write saw a
// partial file, which the proxy served as a 502 on an artifact it held.
func TestConcurrentReadsSurviveWritesToSameKey(t *testing.T) {
	const (
		key          = "pkg/thing-1.0.0.tgz"
		readers      = 4
		readsPerRead = 500
	)
	dir := t.TempDir()
	ctx := context.Background()

	b, err := OpenBucket(ctx, fileURLFromPath(dir))
	if err != nil {
		t.Fatalf("OpenBucket failed: %v", err)
	}
	defer func() { _ = b.Close() }()

	payload := strings.Repeat("x", 4096)
	if _, _, err := b.Store(ctx, key, strings.NewReader(payload)); err != nil {
		t.Fatalf("seeding Store failed: %v", err)
	}

	done := make(chan struct{})
	var writers sync.WaitGroup
	writers.Add(1)
	go func() {
		defer writers.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			if _, _, err := b.Store(ctx, key, strings.NewReader(payload)); err != nil {
				return
			}
		}
	}()

	var failures atomic.Int64
	var reading sync.WaitGroup
	for range readers {
		reading.Add(1)
		go func() {
			defer reading.Done()
			for range readsPerRead {
				r, err := b.Open(ctx, key)
				if err != nil {
					failures.Add(1)
					continue
				}
				if _, err := io.Copy(io.Discard, r); err != nil {
					failures.Add(1)
				}
				_ = r.Close()
			}
		}()
	}
	reading.Wait()
	close(done)
	writers.Wait()

	if got := failures.Load(); got != 0 {
		t.Errorf("%d of %d reads failed while one writer rewrote the same key, want 0", got, readers*readsPerRead)
	}
}

// seedLegacySidecar stores key through a bucket that still writes sidecars, as
// an earlier version did, and returns the path fileblob actually used. It is
// discovered rather than assumed, so callers test the real mapping.
func seedLegacySidecar(t *testing.T, dir, key, payload string) string {
	t.Helper()
	ctx := context.Background()

	legacy, err := blob.OpenBucket(ctx, fileURLFromPath(dir)+"?no_tmp_dir=true")
	if err != nil {
		t.Fatalf("opening legacy bucket: %v", err)
	}
	if err := legacy.WriteAll(ctx, key, []byte(payload), nil); err != nil {
		t.Fatalf("legacy WriteAll: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("closing legacy bucket: %v", err)
	}

	var found []string
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".attrs") {
			found = append(found, path)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walking %s: %v", dir, walkErr)
	}
	if len(found) != 1 {
		t.Fatalf("got sidecars %v, want exactly one", found)
	}
	return found[0]
}

// An interrupted setAttrs leaves a partial sidecar that fails every read of the
// key, and nothing rewrites one now, so a store has to clear it.
//
// One key per storage path the proxy builds: ArtifactPath across ecosystems,
// metadata blobs, and the Gradle build cache. Scoped npm names, Go's "!" case
// escaping and the ":" in OCI digests and Debian epochs are the characters
// most likely to part fileblob's mapping from a plain path join.
func TestStoreClearsLegacyAttrsSidecar(t *testing.T) {
	keys := []string{
		"npm/@babel/core/7.24.0/core-7.24.0.tgz",
		"maven/org.apache.commons/commons-lang3/3.14.0/commons-lang3-3.14.0.jar",
		"golang/github.com/!burnt!sushi/toml/v1.3.2/v1.3.2.zip",
		"oci/library/nginx/sha256:abc123def456/manifest",
		"debian/tzdata/1:2024a-1/tzdata_2024a-1_all.deb",
		"pypi/requests/2.31.0/requests-2.31.0-py3-none-any.whl",
		"cargo/serde/1.0.197/serde-1.0.197.crate",
		"julia/Example/a1b2c3/a1b2c3.tar.gz",
		"conda/numpy/1.26.4/numpy-1.26.4-py311.conda",
		"_metadata/npm/@babel/core/metadata",
		"_gradle/http-build-cache/0a1b2c3d4e5f",
	}

	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			assertStoreClearsSidecar(t, key)
		})
	}
}

func assertStoreClearsSidecar(t *testing.T, key string) {
	t.Helper()
	const payload = "payload"
	ctx := context.Background()
	dir := t.TempDir()

	sidecar := seedLegacySidecar(t, dir, key, payload)
	if err := os.WriteFile(sidecar, []byte(`{"user.content_type":"appl`), 0o600); err != nil {
		t.Fatalf("corrupting sidecar: %v", err)
	}

	b := openFileBlob(t, dir)
	if _, err := b.Open(ctx, key); err == nil {
		t.Fatal("corrupt sidecar did not fail the read, so it is not the file fileblob reads for this key")
	}

	derived := b.legacySidecarPath(key)
	if _, _, err := b.Store(ctx, key, strings.NewReader(payload)); err != nil {
		t.Fatalf("Store failed: %v", err)
	}

	if derived == "" {
		assertSidecarKept(t, sidecar)
		return
	}
	if derived != sidecar {
		t.Fatalf("derived %q, but fileblob wrote %q", derived, sidecar)
	}
	if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
		t.Errorf("sidecar still present after Store, stat err = %v", err)
	}
	assertReadsBack(t, b, key, payload)
}

// Windows rejects ":" in a local path and fileblob escapes it, so for those
// keys the mapping is not certain and the sidecar is left alone.
func assertSidecarKept(t *testing.T, sidecar string) {
	t.Helper()
	if runtime.GOOS != osWindows {
		t.Fatalf("declined a key that is a plain local path on %s", runtime.GOOS)
	}
	if _, err := os.Stat(sidecar); err != nil {
		t.Errorf("declined key should keep its sidecar, stat err = %v", err)
	}
}

func assertReadsBack(t *testing.T, b *Blob, key, want string) {
	t.Helper()
	r, err := b.Open(context.Background(), key)
	if err != nil {
		t.Fatalf("read still failing after Store cleared the sidecar: %v", err)
	}
	defer func() { _ = r.Close() }()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func openFileBlob(t *testing.T, dir string) *Blob {
	t.Helper()
	s, err := OpenBucket(context.Background(), fileURLFromPath(dir))
	if err != nil {
		t.Fatalf("OpenBucket failed: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	b, ok := s.(*Blob)
	if !ok {
		t.Fatalf("got %T, want *Blob", s)
	}
	return b
}

// fileblob escapes a non-local key in a way this cannot reproduce, and one
// holding ".." resolves outside the cache directory. Removal declines both
// rather than delete the wrong file.
func TestLegacySidecarPathDeclinesNonLocalKeys(t *testing.T) {
	b := &Blob{fileRoot: filepath.FromSlash("/var/cache/proxy")}

	for _, key := range []string{
		"npm/pkg//1.0.0/x.tgz",
		"npm/pkg/../../../../etc/passwd",
		"npm/pkg/1.0.0/",
		"/etc/passwd",
		"",
	} {
		if got := b.legacySidecarPath(key); got != "" {
			t.Errorf("legacySidecarPath(%q) = %q, want \"\"", key, got)
		}
	}

	if got := b.legacySidecarPath("npm/pkg/1.0.0/x.tgz"); got == "" {
		t.Error("a plain key must still map to a sidecar path")
	}
}

// Cloud backends have no local directory, so nothing is removed for them.
func TestLegacySidecarPathEmptyForCloudBackends(t *testing.T) {
	b := &Blob{}
	if got := b.legacySidecarPath("npm/pkg/1.0.0/x.tgz"); got != "" {
		t.Errorf("legacySidecarPath = %q, want \"\" when there is no file root", got)
	}
}
