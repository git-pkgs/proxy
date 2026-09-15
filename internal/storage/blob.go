package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gocloud.dev/blob"
	_ "gocloud.dev/blob/azureblob"
	_ "gocloud.dev/blob/fileblob"
	_ "gocloud.dev/blob/s3blob"
	"gocloud.dev/gcerrors"
)

const osWindows = "windows"

// attrsExt is fileblob's sidecar suffix, kept only to clear sidecars an
// earlier version wrote.
const attrsExt = ".attrs"

// Blob implements Storage using gocloud.dev/blob.
// Supports local filesystem (file://) and S3 (s3://) URLs.
type Blob struct {
	bucket *blob.Bucket
	url    string

	// fileRoot is the directory backing a file:// bucket, empty for cloud
	// backends. Used only to clear sidecars an earlier version wrote.
	fileRoot string
}

// OpenBucket opens a blob bucket from a URL.
//
// Supported URL schemes:
//   - file:///path/to/dir - Local filesystem storage
//   - s3://bucket-name - Amazon S3 (uses AWS_* environment variables)
//   - s3://bucket-name?region=us-east-1&endpoint=http://localhost:9000 - S3-compatible (MinIO, etc.)
//   - gs://bucket-name - Google Cloud Storage (uses Application Default Credentials;
//     supports Workload Identity on GKE/GCE without any extra configuration)
//   - azblob://container-name - Azure Blob Storage
//
// For local filesystem, the directory is created if it doesn't exist.
//
//nolint:ireturn // The URL scheme selects the storage implementation.
func OpenBucket(ctx context.Context, urlStr string) (Storage, error) {
	if strings.HasPrefix(urlStr, "gs://") {
		return OpenGCS(ctx, urlStr)
	}

	var fileRoot string

	// Handle file:// URLs specially to create the directory
	if strings.HasPrefix(urlStr, "file://") {
		path := strings.TrimPrefix(urlStr, "file://")

		// Handle file:/// (three slashes) for absolute paths
		if strings.HasPrefix(path, "/") && runtime.GOOS != osWindows {
			// Unix: file:///path -> /path
			// path is already correct
		} else if strings.HasPrefix(path, "/") && runtime.GOOS == osWindows {
			// Windows: file:///C:/path -> C:/path
			path = strings.TrimPrefix(path, "/")
		}

		// Convert forward slashes to native path separators for filesystem operations
		nativePath := filepath.FromSlash(path)

		// Ensure directory exists
		if err := os.MkdirAll(nativePath, dirPermissions); err != nil {
			return nil, fmt.Errorf("creating directory: %w", err)
		}

		// fileblob requires an absolute path with forward slashes
		absPath, err := filepath.Abs(nativePath)
		if err != nil {
			return nil, fmt.Errorf("resolving path: %w", err)
		}

		fileRoot = absPath

		// Convert back to URL format with forward slashes
		urlPath := filepath.ToSlash(absPath)
		if runtime.GOOS == osWindows {
			// Windows needs file:///C:/path format
			urlStr = "file:///" + urlPath
		} else {
			urlStr = "file://" + urlPath
		}

		// Create temp files next to the final path instead of in os.TempDir.
		// This avoids "invalid cross-device link" errors from os.Rename when
		// the bucket directory and os.TempDir are on different filesystems
		// (e.g. Docker volume mounts).
		//
		// Do not write fileblob's ".attrs" sidecar. It is rewritten with
		// os.Create, truncating in place outside the atomic rename that
		// protects the blob, so a read overlapping a write can decode a
		// partial file; a missing one defaults cleanly, a truncated one does
		// not. Nothing in the proxy needs it: Store sets no ContentType, and
		// Size reads os.Stat via Attributes.
		urlStr += "?no_tmp_dir=true&metadata=skip"
	}

	bucket, err := blob.OpenBucket(ctx, urlStr)
	if err != nil {
		return nil, fmt.Errorf("opening bucket: %w", err)
	}

	return &Blob{bucket: bucket, url: urlStr, fileRoot: fileRoot}, nil
}

// legacySidecarPath gives the ".attrs" path an earlier version wrote for key,
// or "" when that path would not be a file inside fileRoot.
//
// The key is escaped the way fileblob escapes it on the way to disk, so the
// sidecar is looked for where fileblob wrote it. filepath.Localize then
// validates the escaped form: it rejects an empty, absolute or ".." path, and
// "." would name fileRoot itself. What it declines are keys the proxy never
// produces.
func (b *Blob) legacySidecarPath(key string) string {
	if b.fileRoot == "" {
		return ""
	}
	rel, err := filepath.Localize(escapeKey(key))
	if err != nil || rel == "." {
		return ""
	}
	return filepath.Join(b.fileRoot, rel) + attrsExt
}

// escapeKey mirrors fileblob's unexported escapeKey, which hex-escapes a rune
// as "__0x<hex>__". Slashes stay as "/" for filepath.Localize to convert.
func escapeKey(key string) string {
	runes := []rune(key)
	var out strings.Builder
	for i, r := range runes {
		if escapeRune(runes, i) {
			fmt.Fprintf(&out, "__%#x__", r)
		} else {
			out.WriteRune(r)
		}
	}
	return out.String()
}

// escapeRune is fileblob's rule for which runes of a key to escape: control
// characters, a raw path separator, a slash that would form "../", "//" or
// end the key, and on Windows the characters its filesystem reserves.
func escapeRune(r []rune, i int) bool {
	c := r[i]
	switch {
	case c < ' ':
		return true
	case os.PathSeparator != '/' && c == os.PathSeparator:
		return true
	case i > 1 && c == '/' && r[i-1] == '.' && r[i-2] == '.':
		return true
	case i > 0 && c == '/' && r[i-1] == '/':
		return true
	case c == '/' && i == len(r)-1:
		return true
	case os.PathSeparator == '\\' && strings.ContainsRune(`<>:"|?*`, c):
		return true
	}
	return false
}

// clearLegacySidecar removes the ".attrs" file an earlier version wrote for
// key. Nothing rewrites one now, so a sidecar left partial by an interrupted
// write would fail every read of that key for good. Removing is atomic where
// the rewrite was not, so a concurrent reader gets the whole old file or
// nothing.
//
// Failure is deliberately not fatal. Usually the key never had a sidecar and
// os.Remove reports not-exist. A real failure leaves exactly the state this
// change inherited, while failing the write would turn a cleanup miss into a
// failed request. Windows makes that concrete: Go opens files without
// FILE_SHARE_DELETE, so a reader holding the sidecar open blocks deletion, and
// that reader is the very workload this change protects. The next store of the
// key retries.
func (b *Blob) clearLegacySidecar(key string) {
	if sidecar := b.legacySidecarPath(key); sidecar != "" {
		_ = os.Remove(sidecar)
	}
}

func (b *Blob) Store(ctx context.Context, path string, r io.Reader) (int64, string, error) {
	b.clearLegacySidecar(path)

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

func (b *Blob) SignedURL(ctx context.Context, path string, expiry time.Duration) (string, error) {
	url, err := b.bucket.SignedURL(ctx, path, &blob.SignedURLOptions{
		Method: http.MethodGet,
		Expiry: expiry,
	})
	if err != nil {
		if gcerrors.Code(err) == gcerrors.Unimplemented {
			return "", ErrSignedURLUnsupported
		}
		return "", fmt.Errorf("signing URL: %w", err)
	}
	return url, nil
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

// ListPrefix returns object metadata for keys under a prefix.
func (b *Blob) ListPrefix(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	iter := b.bucket.List(&blob.ListOptions{Prefix: prefix})
	objects := make([]ObjectInfo, 0)

	for {
		obj, err := iter.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("listing objects: %w", err)
		}
		if obj.IsDir {
			continue
		}

		info := ObjectInfo{
			Path:    obj.Key,
			Size:    obj.Size,
			ModTime: obj.ModTime,
		}

		objects = append(objects, info)
	}

	return objects, nil
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
