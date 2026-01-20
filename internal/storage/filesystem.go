package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Filesystem implements Storage using the local filesystem.
type Filesystem struct {
	root string
}

// NewFilesystem creates a new filesystem storage rooted at the given directory.
// The directory will be created if it does not exist.
func NewFilesystem(root string) (*Filesystem, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolving root path: %w", err)
	}

	if err := os.MkdirAll(absRoot, 0755); err != nil {
		return nil, fmt.Errorf("creating root directory: %w", err)
	}

	return &Filesystem{root: absRoot}, nil
}

func (fs *Filesystem) fullPath(path string) string {
	return filepath.Join(fs.root, filepath.FromSlash(path))
}

func (fs *Filesystem) Store(ctx context.Context, path string, r io.Reader) (int64, string, error) {
	fullPath := fs.fullPath(path)

	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return 0, "", fmt.Errorf("creating directory: %w", err)
	}

	// Write to temp file first for atomic operation
	tmpFile, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return 0, "", fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	// Clean up temp file on error
	success := false
	defer func() {
		if !success {
			_ = tmpFile.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	// Write content and compute hash
	h := sha256.New()
	w := io.MultiWriter(tmpFile, h)

	size, err := io.Copy(w, r)
	if err != nil {
		return 0, "", fmt.Errorf("writing content: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		return 0, "", fmt.Errorf("closing temp file: %w", err)
	}

	// Atomic rename
	if err := os.Rename(tmpPath, fullPath); err != nil {
		return 0, "", fmt.Errorf("renaming temp file: %w", err)
	}

	success = true
	hash := hex.EncodeToString(h.Sum(nil))
	return size, hash, nil
}

func (fs *Filesystem) Open(ctx context.Context, path string) (io.ReadCloser, error) {
	fullPath := fs.fullPath(path)

	f, err := os.Open(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("opening file: %w", err)
	}

	return f, nil
}

func (fs *Filesystem) Exists(ctx context.Context, path string) (bool, error) {
	fullPath := fs.fullPath(path)

	_, err := os.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("checking file: %w", err)
	}

	return true, nil
}

func (fs *Filesystem) Delete(ctx context.Context, path string) error {
	fullPath := fs.fullPath(path)

	err := os.Remove(fullPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing file: %w", err)
	}

	// Try to clean up empty parent directories
	dir := filepath.Dir(fullPath)
	for dir != fs.root {
		if err := os.Remove(dir); err != nil {
			break // Directory not empty or other error
		}
		dir = filepath.Dir(dir)
	}

	return nil
}

func (fs *Filesystem) Size(ctx context.Context, path string) (int64, error) {
	fullPath := fs.fullPath(path)

	info, err := os.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("stat file: %w", err)
	}

	return info.Size(), nil
}

func (fs *Filesystem) UsedSpace(ctx context.Context) (int64, error) {
	var total int64

	err := filepath.Walk(fs.root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("walking directory: %w", err)
	}

	return total, nil
}

// Root returns the root directory of the storage.
func (fs *Filesystem) Root() string {
	return fs.root
}

// FullPath returns the full filesystem path for a storage path.
// Useful for serving files directly or debugging.
func (fs *Filesystem) FullPath(path string) string {
	return fs.fullPath(path)
}
