package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"time"

	gcstorage "github.com/git-pkgs/gcs"
)

// GCS adapts a Google Cloud Storage bucket to Storage.
type GCS struct {
	bucket       *gcstorage.Bucket
	url          string
	signEndpoint string
}

// OpenGCS opens a Google Cloud Storage bucket from a gs:// URL.
func OpenGCS(ctx context.Context, urlStr string) (*GCS, error) {
	bucket, err := gcstorage.OpenBucket(ctx, urlStr)
	if err != nil {
		return nil, err
	}
	return &GCS{bucket: bucket, url: urlStr}, nil
}

// Store writes the contents of r to path and returns the byte count and hex SHA-256 digest.
func (g *GCS) Store(ctx context.Context, path string, r io.Reader) (int64, string, error) {
	h := sha256.New()
	size, err := g.bucket.Write(ctx, path, io.TeeReader(r, h))
	if err != nil {
		return 0, "", err
	}
	return size, hex.EncodeToString(h.Sum(nil)), nil
}

// Open returns a reader for the object stored at path.
func (g *GCS) Open(ctx context.Context, path string) (io.ReadCloser, error) {
	r, err := g.bucket.Open(ctx, path)
	if errors.Is(err, gcstorage.ErrNotFound) {
		return nil, ErrNotFound
	}
	return r, err
}

// Exists reports whether an object exists at path.
func (g *GCS) Exists(ctx context.Context, path string) (bool, error) {
	return g.bucket.Exists(ctx, path)
}

// Delete removes the object at path.
func (g *GCS) Delete(ctx context.Context, path string) error {
	return g.bucket.Delete(ctx, path)
}

// Size returns the size in bytes of the object at path.
func (g *GCS) Size(ctx context.Context, path string) (int64, error) {
	size, err := g.bucket.Size(ctx, path)
	if errors.Is(err, gcstorage.ErrNotFound) {
		return 0, ErrNotFound
	}
	return size, err
}

// SignedURL returns a presigned URL granting time-limited GET access to path,
// rewriting the scheme and host to signEndpoint when configured.
func (g *GCS) SignedURL(ctx context.Context, path string, expiry time.Duration) (string, error) {
	u, err := g.bucket.SignedURL(ctx, path, expiry)
	if errors.Is(err, gcstorage.ErrSignedURLUnsupported) {
		return "", ErrSignedURLUnsupported
	}
	if err != nil {
		return "", err
	}
	return rewriteSignedURLHost(u, g.signEndpoint), nil
}

// UsedSpace returns the total bytes stored across all objects in the bucket.
func (g *GCS) UsedSpace(ctx context.Context) (int64, error) {
	return g.bucket.UsedSpace(ctx)
}

// ListPrefix returns object metadata for keys under a prefix.
func (g *GCS) ListPrefix(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	objects, err := g.bucket.ListPrefix(ctx, prefix)
	if err != nil {
		return nil, err
	}

	result := make([]ObjectInfo, 0, len(objects))
	for _, object := range objects {
		result = append(result, ObjectInfo{
			Path:    object.Name,
			Size:    object.Size,
			ModTime: object.ModTime,
		})
	}
	return result, nil
}

// Close releases any resources held by the GCS client.
func (g *GCS) Close() error {
	return nil
}

// URL returns the storage backend URL.
func (g *GCS) URL() string {
	return g.url
}
