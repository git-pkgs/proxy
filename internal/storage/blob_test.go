package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
	b := createTestBlob(t)
	ctx := context.Background()

	// 1MB of data
	data := bytes.Repeat([]byte("x"), 1024*1024)

	size, hash, err := b.Store(ctx, "large/file.bin", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Store large file failed: %v", err)
	}
	if size != int64(len(data)) {
		t.Errorf("size = %d, want %d", size, len(data))
	}

	h := sha256.Sum256(data)
	wantHash := hex.EncodeToString(h[:])
	if hash != wantHash {
		t.Errorf("hash mismatch for large file")
	}

	// Read it back
	r, _ := b.Open(ctx, "large/file.bin")
	defer func() { _ = r.Close() }()
	readBack, _ := io.ReadAll(r)
	if !bytes.Equal(readBack, data) {
		t.Error("large file content mismatch")
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

func createTestBlob(t *testing.T) *Blob {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()

	b, err := OpenBucket(ctx, fileURLFromPath(dir))
	if err != nil {
		t.Fatalf("OpenBucket failed: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func fileURLFromPath(path string) string {
	if runtime.GOOS == "windows" {
		// Windows paths need file:///C:/path format
		path = filepath.ToSlash(path)
		return "file:///" + path
	}
	return "file://" + path
}
