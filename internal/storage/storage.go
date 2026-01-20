// Package storage provides artifact storage backends for the proxy cache.
package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
)

var (
	ErrNotFound = errors.New("artifact not found")
)

// Storage defines the interface for artifact storage backends.
type Storage interface {
	// Store writes content from r to the given path.
	// Returns the number of bytes written and the SHA256 hash of the content.
	Store(ctx context.Context, path string, r io.Reader) (size int64, hash string, err error)

	// Open returns a reader for the content at path.
	// The caller must close the reader when done.
	// Returns ErrNotFound if the path does not exist.
	Open(ctx context.Context, path string) (io.ReadCloser, error)

	// Exists returns true if content exists at path.
	Exists(ctx context.Context, path string) (bool, error)

	// Delete removes the content at path.
	// Returns nil if the path does not exist.
	Delete(ctx context.Context, path string) error

	// Size returns the size in bytes of content at path.
	// Returns ErrNotFound if the path does not exist.
	Size(ctx context.Context, path string) (int64, error)

	// UsedSpace returns the total bytes used by all stored content.
	UsedSpace(ctx context.Context) (int64, error)
}

// ArtifactPath builds a storage path for an artifact.
// Format: {ecosystem}/{namespace}/{name}/{version}/{filename}
// For packages without namespace: {ecosystem}/{name}/{version}/{filename}
func ArtifactPath(ecosystem, namespace, name, version, filename string) string {
	if namespace != "" {
		return ecosystem + "/" + namespace + "/" + name + "/" + version + "/" + filename
	}
	return ecosystem + "/" + name + "/" + version + "/" + filename
}

// HashingReader wraps a reader and computes SHA256 hash as content is read.
type HashingReader struct {
	r    io.Reader
	hash []byte
	h    interface{ Sum([]byte) []byte }
	size int64
	done bool
}

func NewHashingReader(r io.Reader) *HashingReader {
	h := sha256.New()
	return &HashingReader{
		r: io.TeeReader(r, h),
		h: h,
	}
}

func (hr *HashingReader) Read(p []byte) (n int, err error) {
	n, err = hr.r.Read(p)
	hr.size += int64(n)
	if err == io.EOF {
		hr.done = true
		hr.hash = hr.h.Sum(nil)
	}
	return
}

func (hr *HashingReader) Sum() string {
	if !hr.done {
		hr.hash = hr.h.Sum(nil)
		hr.done = true
	}
	return hex.EncodeToString(hr.hash)
}

func (hr *HashingReader) Size() int64 {
	return hr.size
}
