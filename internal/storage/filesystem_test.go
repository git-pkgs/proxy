package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewFilesystem(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "cache")

	fs, err := NewFilesystem(root)
	if err != nil {
		t.Fatalf("NewFilesystem failed: %v", err)
	}

	if _, err := os.Stat(root); err != nil {
		t.Errorf("root directory not created: %v", err)
	}

	if fs.Root() != root {
		t.Errorf("Root() = %q, want %q", fs.Root(), root)
	}
}

func TestFilesystemStore(t *testing.T) {
	fs := createTestFilesystem(t)
	ctx := context.Background()
	content := "test content for storage"

	size, hash, err := fs.Store(ctx, "npm/lodash/4.17.21/lodash.tgz", strings.NewReader(content))
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

	// Verify file exists on disk
	fullPath := fs.FullPath("npm/lodash/4.17.21/lodash.tgz")
	data, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("reading stored file: %v", err)
	}
	if string(data) != content {
		t.Errorf("stored content = %q, want %q", string(data), content)
	}
}

func TestFilesystemStoreAtomic(t *testing.T) {
	fs := createTestFilesystem(t)
	ctx := context.Background()

	// Store initial content
	_, _, err := fs.Store(ctx, "test/file.txt", strings.NewReader("initial"))
	if err != nil {
		t.Fatalf("initial Store failed: %v", err)
	}

	// Overwrite with new content
	_, _, err = fs.Store(ctx, "test/file.txt", strings.NewReader("updated"))
	if err != nil {
		t.Fatalf("update Store failed: %v", err)
	}

	// Verify updated content
	r, err := fs.Open(ctx, "test/file.txt")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer func() { _ = r.Close() }()

	data, _ := io.ReadAll(r)
	if string(data) != "updated" {
		t.Errorf("content = %q, want %q", string(data), "updated")
	}
}

func TestFilesystemOpen(t *testing.T) {
	fs := createTestFilesystem(t)
	ctx := context.Background()
	content := "readable content"

	_, _, _ = fs.Store(ctx, "test/read.txt", strings.NewReader(content))

	r, err := fs.Open(ctx, "test/read.txt")
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

func TestFilesystemOpenNotFound(t *testing.T) {
	fs := createTestFilesystem(t)
	ctx := context.Background()

	_, err := fs.Open(ctx, "does/not/exist.txt")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Open non-existent = %v, want ErrNotFound", err)
	}
}

func TestFilesystemExists(t *testing.T) {
	fs := createTestFilesystem(t)
	ctx := context.Background()

	exists, err := fs.Exists(ctx, "test/exists.txt")
	if err != nil {
		t.Fatalf("Exists failed: %v", err)
	}
	if exists {
		t.Error("Exists returned true for non-existent file")
	}

	_, _, _ = fs.Store(ctx, "test/exists.txt", strings.NewReader("content"))

	exists, err = fs.Exists(ctx, "test/exists.txt")
	if err != nil {
		t.Fatalf("Exists after store failed: %v", err)
	}
	if !exists {
		t.Error("Exists returned false for existing file")
	}
}

func TestFilesystemDelete(t *testing.T) {
	fs := createTestFilesystem(t)
	ctx := context.Background()

	_, _, _ = fs.Store(ctx, "test/delete/nested/file.txt", strings.NewReader("content"))

	err := fs.Delete(ctx, "test/delete/nested/file.txt")
	if err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	exists, _ := fs.Exists(ctx, "test/delete/nested/file.txt")
	if exists {
		t.Error("file still exists after delete")
	}

	// Empty parent directories should be cleaned up
	nestedDir := fs.FullPath("test/delete/nested")
	if _, err := os.Stat(nestedDir); !os.IsNotExist(err) {
		t.Error("empty nested directory not cleaned up")
	}
}

func TestFilesystemDeleteNotFound(t *testing.T) {
	fs := createTestFilesystem(t)
	ctx := context.Background()

	// Delete non-existent file should not error
	err := fs.Delete(ctx, "does/not/exist.txt")
	if err != nil {
		t.Errorf("Delete non-existent = %v, want nil", err)
	}
}

func TestFilesystemSize(t *testing.T) {
	fs := createTestFilesystem(t)
	ctx := context.Background()
	content := "size test content"

	_, _, _ = fs.Store(ctx, "test/size.txt", strings.NewReader(content))

	size, err := fs.Size(ctx, "test/size.txt")
	if err != nil {
		t.Fatalf("Size failed: %v", err)
	}
	if size != int64(len(content)) {
		t.Errorf("Size = %d, want %d", size, len(content))
	}
}

func TestFilesystemSizeNotFound(t *testing.T) {
	fs := createTestFilesystem(t)
	ctx := context.Background()

	_, err := fs.Size(ctx, "does/not/exist.txt")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Size non-existent = %v, want ErrNotFound", err)
	}
}

func TestFilesystemUsedSpace(t *testing.T) {
	fs := createTestFilesystem(t)
	ctx := context.Background()

	// Empty storage
	used, err := fs.UsedSpace(ctx)
	if err != nil {
		t.Fatalf("UsedSpace failed: %v", err)
	}
	if used != 0 {
		t.Errorf("UsedSpace empty = %d, want 0", used)
	}

	// Add some files
	_, _, _ = fs.Store(ctx, "a.txt", strings.NewReader("aaaa"))     // 4 bytes
	_, _, _ = fs.Store(ctx, "b.txt", strings.NewReader("bbbbbb"))   // 6 bytes
	_, _, _ = fs.Store(ctx, "c/d.txt", strings.NewReader("ccccc"))  // 5 bytes

	used, err = fs.UsedSpace(ctx)
	if err != nil {
		t.Fatalf("UsedSpace failed: %v", err)
	}
	if used != 15 {
		t.Errorf("UsedSpace = %d, want 15", used)
	}
}

func TestFilesystemLargeFile(t *testing.T) {
	fs := createTestFilesystem(t)
	ctx := context.Background()

	// 1MB of data
	data := bytes.Repeat([]byte("x"), 1024*1024)

	size, hash, err := fs.Store(ctx, "large/file.bin", bytes.NewReader(data))
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
	r, _ := fs.Open(ctx, "large/file.bin")
	defer func() { _ = r.Close() }()
	readBack, _ := io.ReadAll(r)
	if !bytes.Equal(readBack, data) {
		t.Error("large file content mismatch")
	}
}

func createTestFilesystem(t *testing.T) *Filesystem {
	t.Helper()
	dir := t.TempDir()

	fs, err := NewFilesystem(dir)
	if err != nil {
		t.Fatalf("NewFilesystem failed: %v", err)
	}
	return fs
}
