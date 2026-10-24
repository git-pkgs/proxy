package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/storage"
	"github.com/git-pkgs/registries/fetch"
)

func TestMetadataStoragePathKeepsCaseApart(t *testing.T) {
	for _, pair := range [][2]string{
		{"github.com/Sirupsen/logrus", "github.com/sirupsen/logrus"},
		{"JSONStream", "jsonstream"},
	} {
		a, b := metadataStoragePath("go", pair[0]), metadataStoragePath("go", pair[1])
		if strings.EqualFold(a, b) {
			t.Errorf("%q and %q map to paths %q and %q, which a case-insensitive filesystem treats as one", pair[0], pair[1], a, b)
		}
	}
	for _, key := range []string{"CON", "nul", "trailing.", "a/../b"} {
		p := metadataStoragePath("npm", key)
		if strings.Contains(p, key) {
			t.Errorf("path %q contains the raw key %q", p, key)
		}
	}
	// Keys that already are digests keep their path, so existing entries
	// stay valid.
	sum := sha256.Sum256([]byte("generic"))
	digest := hex.EncodeToString(sum[:])
	if got, want := metadataStoragePath("generic", digest), "_metadata/generic/"+digest+"/metadata"; got != want {
		t.Errorf("digest key path = %q, want %q", got, want)
	}
}

// fileStorageProxy returns a proxy whose metadata is cached on the local
// filesystem, as with file:// storage.
func fileStorageProxy(t *testing.T, client *http.Client) (*Proxy, *database.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Create(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := storage.OpenBucket(context.Background(), "file://"+filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	proxy := NewProxy(db, store, &mockFetcher{}, fetch.NewResolver(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	proxy.HTTPClient = client
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	return proxy, db
}

// TestMetadataKeysDifferingInCaseCachedApart caches metadata for two keys
// that differ only in case on file storage. On a case-insensitive filesystem
// they shared one file, and the first key was served the second's document.
func TestMetadataKeysDifferingInCaseCachedApart(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, "document for "+r.URL.Path)
	}))
	defer upstream.Close()
	proxy, _ := fileStorageProxy(t, upstream.Client())

	keys := []string{"github.com/Sirupsen/logrus", "github.com/sirupsen/logrus"}
	for range 2 {
		for _, key := range keys {
			body, _, err := proxy.FetchOrCacheMetadata(context.Background(), "go", key, upstream.URL+"/"+key)
			if err != nil {
				t.Fatal(err)
			}
			if want := "document for /" + key; string(body) != want {
				t.Errorf("%s: served %q, want %q", key, body, want)
			}
		}
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("upstream requests = %d, want 2", got)
	}
}

// seedLegacyMetadata stores blob at key's legacy path and a row pointing at
// it, fetched at fetchedAt. The row's digest is that of document, or unset
// when document is empty.
func seedLegacyMetadata(t *testing.T, proxy *Proxy, db *database.DB, key, blob, document string, fetchedAt time.Time) string {
	t.Helper()
	legacy := legacyMetadataStoragePath("go", key)
	if _, _, err := proxy.Storage.Store(context.Background(), legacy, strings.NewReader(blob)); err != nil {
		t.Fatal(err)
	}
	entry := &database.MetadataCacheEntry{
		Ecosystem:   "go",
		Name:        key,
		StoragePath: legacy,
		ETag:        sql.NullString{String: `"v1"`, Valid: true},
		Size:        sql.NullInt64{Int64: int64(len(blob)), Valid: true},
		FetchedAt:   sql.NullTime{Time: fetchedAt, Valid: true},
	}
	if document != "" {
		sum := sha256.Sum256([]byte(document))
		entry.ContentDigest = sql.NullString{String: "sha256:" + hex.EncodeToString(sum[:]), Valid: true}
	}
	if err := db.UpsertMetadataCache(entry); err != nil {
		t.Fatal(err)
	}
	return legacy
}

func pendingDeletes(t *testing.T, db *database.DB) []string {
	t.Helper()
	due, err := db.GetDuePendingDeletes(time.Now().Add(time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	return due
}

func metadataRowPath(t *testing.T, db *database.DB, key string) string {
	t.Helper()
	entry, err := db.GetMetadataCache("go", key)
	if err != nil || entry == nil {
		t.Fatalf("cache row for %s: %v %v", key, entry, err)
	}
	return entry.StoragePath
}

// TestLegacyMetadataMatchingBlobServedWhenUpstreamDown checks that a row at
// its legacy path whose blob matches its digest is still served as a stale
// fallback, and is moved to the hashed path.
func TestLegacyMetadataMatchingBlobServedWhenUpstreamDown(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer upstream.Close()
	proxy, db := fileStorageProxy(t, upstream.Client())

	const key = "github.com/Sirupsen/logrus"
	legacy := seedLegacyMetadata(t, proxy, db, key, "old", "old", time.Now().Add(-2*time.Hour))

	for range 2 {
		body, _, err := proxy.FetchOrCacheMetadata(context.Background(), "go", key, upstream.URL+"/x")
		if err != nil {
			t.Fatalf("legacy document not served while upstream is down: %v", err)
		}
		if string(body) != "old" {
			t.Fatalf("served %q, want %q", body, "old")
		}
	}
	if got, want := metadataRowPath(t, db, key), metadataStoragePath("go", key); got != want {
		t.Errorf("storage path = %q, want %q", got, want)
	}
	if due := pendingDeletes(t, db); !slices.Contains(due, legacy) {
		t.Errorf("pending deletes = %v, want %q", due, legacy)
	}
}

// TestLegacyMetadataMatchingBlobServedFromCache checks that a fresh row at
// its legacy path whose blob matches is served without asking upstream.
func TestLegacyMetadataMatchingBlobServedFromCache(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, "fresh")
	}))
	defer upstream.Close()
	proxy, db := fileStorageProxy(t, upstream.Client())

	const key = "github.com/Sirupsen/logrus"
	seedLegacyMetadata(t, proxy, db, key, "old", "old", time.Now())

	body, _, err := proxy.FetchOrCacheMetadata(context.Background(), "go", key, upstream.URL+"/x")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "old" || requests.Load() != 0 {
		t.Errorf("served %q after %d upstream requests, want the cached document", body, requests.Load())
	}
}

// TestLegacyMetadataMismatchedBlobNotServed checks that a legacy blob that
// does not match the row's digest, as when another key's document
// overwrote it, is neither served nor used as a fallback.
func TestLegacyMetadataMismatchedBlobNotServed(t *testing.T) {
	var down atomic.Bool
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if down.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if r.Header.Get("If-None-Match") != "" {
			t.Errorf("revalidated a document that was dropped")
		}
		_, _ = io.WriteString(w, "fresh")
	}))
	defer upstream.Close()
	proxy, db := fileStorageProxy(t, upstream.Client())

	const key = "github.com/Sirupsen/logrus"
	seedLegacyMetadata(t, proxy, db, key, "other key's document", "old", time.Now())
	down.Store(true)
	if body, _, err := proxy.FetchOrCacheMetadata(context.Background(), "go", key, upstream.URL+"/x"); err == nil {
		t.Fatalf("served %q, want an error while upstream is down", body)
	}

	down.Store(false)
	body, _, err := proxy.FetchOrCacheMetadata(context.Background(), "go", key, upstream.URL+"/x")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "fresh" {
		t.Errorf("served %q, want %q", body, "fresh")
	}
	if got, want := metadataRowPath(t, db, key), metadataStoragePath("go", key); got != want {
		t.Errorf("storage path = %q, want %q", got, want)
	}
}

// TestLegacyMetadataSharedPathKeptForCaseVariant checks that a legacy blob is
// not queued for deletion while a key differing only in case still reads its
// own legacy path, which on a case-insensitive filesystem is the same file,
// and that both are queued once neither does.
func TestLegacyMetadataSharedPathKeptForCaseVariant(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer upstream.Close()
	proxy, db := fileStorageProxy(t, upstream.Client())

	// Both rows hold the same document, so the test runs alike whether or
	// not the filesystem folds case.
	upper := seedLegacyMetadata(t, proxy, db, "github.com/Sirupsen/logrus", "doc", "doc", time.Now())
	lower := seedLegacyMetadata(t, proxy, db, "github.com/sirupsen/logrus", "doc", "doc", time.Now())

	if _, _, err := proxy.FetchOrCacheMetadata(context.Background(), "go", "github.com/Sirupsen/logrus", upstream.URL+"/x"); err != nil {
		t.Fatal(err)
	}
	if due := pendingDeletes(t, db); len(due) != 0 {
		t.Fatalf("pending deletes = %v while a case variant still reads its legacy blob", due)
	}
	if _, _, err := proxy.FetchOrCacheMetadata(context.Background(), "go", "github.com/sirupsen/logrus", upstream.URL+"/x"); err != nil {
		t.Fatal(err)
	}
	due := pendingDeletes(t, db)
	if !slices.Contains(due, upper) || !slices.Contains(due, lower) {
		t.Errorf("pending deletes = %v, want %q and %q", due, upper, lower)
	}
}

// TestLegacyMetadataPathRefetched checks that a row at its legacy path with
// no digest to check its blob against is fetched again, moved to the hashed
// path, and its old blob queued for deletion.
func TestLegacyMetadataPathRefetched(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, "fresh")
	}))
	defer upstream.Close()
	proxy, db := fileStorageProxy(t, upstream.Client())

	const key = "github.com/Sirupsen/logrus"
	legacy := legacyMetadataStoragePath("go", key)
	if _, _, err := proxy.Storage.Store(context.Background(), legacy, bytes.NewReader([]byte("old"))); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertMetadataCache(&database.MetadataCacheEntry{
		Ecosystem:   "go",
		Name:        key,
		StoragePath: legacy,
		Size:        sql.NullInt64{Int64: 3, Valid: true},
		FetchedAt:   sql.NullTime{Time: time.Now(), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}

	body, _, err := proxy.FetchOrCacheMetadata(context.Background(), "go", key, upstream.URL+"/x")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "fresh" || requests.Load() != 1 {
		t.Fatalf("served %q after %d upstream requests, want a fresh fetch", body, requests.Load())
	}
	entry, err := db.GetMetadataCache("go", key)
	if err != nil || entry == nil {
		t.Fatalf("cache row: %v %v", entry, err)
	}
	if entry.StoragePath != metadataStoragePath("go", key) {
		t.Errorf("storage path = %q, want %q", entry.StoragePath, metadataStoragePath("go", key))
	}
	due, err := db.GetDuePendingDeletes(time.Now().Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(due, legacy) {
		t.Errorf("pending deletes = %v, want %q", due, legacy)
	}
}
