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
	bucket *gcstorage.Bucket
	url    string
}

// OpenGCS opens a Google Cloud Storage bucket from a gs:// URL.
func OpenGCS(ctx context.Context, urlStr string) (*GCS, error) {
	bucket, err := gcstorage.OpenBucket(ctx, urlStr)
	if err != nil {
		return nil, err
	}
	return &GCS{bucket: bucket, url: urlStr}, nil
}

func (g *GCS) Store(ctx context.Context, path string, r io.Reader) (int64, string, error) {
	h := sha256.New()
	size, err := g.bucket.Write(ctx, path, io.TeeReader(r, h))
	if err != nil {
		return 0, "", err
	}
	return size, hex.EncodeToString(h.Sum(nil)), nil
}

func (g *GCS) Open(ctx context.Context, path string) (io.ReadCloser, error) {
	r, err := g.bucket.Open(ctx, path)
	if errors.Is(err, gcstorage.ErrNotFound) {
		return nil, ErrNotFound
	}
	return r, err
}

func (g *GCS) Exists(ctx context.Context, path string) (bool, error) {
	return g.bucket.Exists(ctx, path)
}

func (g *GCS) Delete(ctx context.Context, path string) error {
	return g.bucket.Delete(ctx, path)
}

func (g *GCS) Size(ctx context.Context, path string) (int64, error) {
	size, err := g.bucket.Size(ctx, path)
	if errors.Is(err, gcstorage.ErrNotFound) {
		return 0, ErrNotFound
	}
	return size, err
}

func (g *GCS) SignedURL(ctx context.Context, path string, expiry time.Duration) (string, error) {
	u, err := g.bucket.SignedURL(ctx, path, expiry)
	if errors.Is(err, gcstorage.ErrSignedURLUnsupported) {
		return "", ErrSignedURLUnsupported
	}
	return u, err
}

func (g *GCS) UsedSpace(ctx context.Context) (int64, error) {
	return g.bucket.UsedSpace(ctx)
}

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

func (g *GCS) Close() error {
	return nil
}

func (g *GCS) URL() string {
	return g.url
}
