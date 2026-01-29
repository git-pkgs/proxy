package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"gocloud.dev/blob"
	_ "gocloud.dev/blob/fileblob"
	_ "gocloud.dev/blob/s3blob"
	"gocloud.dev/gcerrors"
)

// Blob implements Storage using gocloud.dev/blob.
// Supports local filesystem (file://) and S3 (s3://) URLs.
type Blob struct {
	bucket *blob.Bucket
	url    string
}

// OpenBucket opens a blob bucket from a URL.
//
// Supported URL schemes:
//   - file:///path/to/dir - Local filesystem storage
//   - s3://bucket-name - Amazon S3 (uses AWS_* environment variables)
//   - s3://bucket-name?region=us-east-1&endpoint=http://localhost:9000 - S3-compatible (MinIO, etc.)
//
// For local filesystem, the directory is created if it doesn't exist.
func OpenBucket(ctx context.Context, urlStr string) (*Blob, error) {
	// Handle file:// URLs specially to create the directory
	if strings.HasPrefix(urlStr, "file://") {
		parsed, err := url.Parse(urlStr)
		if err != nil {
			return nil, fmt.Errorf("parsing URL: %w", err)
		}

		path := parsed.Path
		if path == "" {
			path = parsed.Opaque
		}

		// Ensure directory exists
		if err := os.MkdirAll(path, 0755); err != nil {
			return nil, fmt.Errorf("creating directory: %w", err)
		}

		// fileblob requires an absolute path
		absPath, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolving path: %w", err)
		}

		urlStr = "file://" + absPath
	}

	bucket, err := blob.OpenBucket(ctx, urlStr)
	if err != nil {
		return nil, fmt.Errorf("opening bucket: %w", err)
	}

	return &Blob{bucket: bucket, url: urlStr}, nil
}

func (b *Blob) Store(ctx context.Context, path string, r io.Reader) (int64, string, error) {
	// Compute hash while writing
	h := sha256.New()
	tee := io.TeeReader(r, h)

	opts := &blob.WriterOptions{}
	w, err := b.bucket.NewWriter(ctx, path, opts)
	if err != nil {
		return 0, "", fmt.Errorf("creating writer: %w", err)
	}

	size, err := io.Copy(w, tee)
	if err != nil {
		_ = w.Close()
		return 0, "", fmt.Errorf("writing content: %w", err)
	}

	if err := w.Close(); err != nil {
		return 0, "", fmt.Errorf("closing writer: %w", err)
	}

	hash := hex.EncodeToString(h.Sum(nil))
	return size, hash, nil
}

func (b *Blob) Open(ctx context.Context, path string) (io.ReadCloser, error) {
	r, err := b.bucket.NewReader(ctx, path, nil)
	if err != nil {
		if isNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("opening reader: %w", err)
	}
	return r, nil
}

func (b *Blob) Exists(ctx context.Context, path string) (bool, error) {
	exists, err := b.bucket.Exists(ctx, path)
	if err != nil {
		return false, fmt.Errorf("checking existence: %w", err)
	}
	return exists, nil
}

func (b *Blob) Delete(ctx context.Context, path string) error {
	err := b.bucket.Delete(ctx, path)
	if err != nil && !isNotExist(err) {
		return fmt.Errorf("deleting object: %w", err)
	}
	return nil
}

func (b *Blob) Size(ctx context.Context, path string) (int64, error) {
	attrs, err := b.bucket.Attributes(ctx, path)
	if err != nil {
		if isNotExist(err) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("getting attributes: %w", err)
	}
	return attrs.Size, nil
}

func (b *Blob) UsedSpace(ctx context.Context) (int64, error) {
	var total int64

	iter := b.bucket.List(nil)
	for {
		obj, err := iter.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("listing objects: %w", err)
		}
		total += obj.Size
	}

	return total, nil
}

func (b *Blob) Close() error {
	return b.bucket.Close()
}

func (b *Blob) URL() string {
	return b.url
}

func isNotExist(err error) bool {
	return gcerrors.Code(err) == gcerrors.NotFound
}
