package handler

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
)

// TestMetadataNotModifiedKeepsStoredDocument checks that a 304 from upstream
// refreshes the cache row without writing the unchanged document to storage
// again, and still serves it.
func TestMetadataNotModifiedKeepsStoredDocument(t *testing.T) {
	const body = `{"v":1}`
	var notModified atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(headerETag, `"v1"`)
		if r.Header.Get("If-None-Match") == `"v1"` {
			notModified.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set(headerContentType, contentTypeJSON)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(upstream.Close)

	proxy, db, store, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Nanosecond // revalidate on every request
	proxy.HTTPClient = upstream.Client()

	ctx := context.Background()
	first := proxy.fetchMetadataDocument(ctx, "test", "pkg", upstream.URL+"/pkg", contentTypeJSON)
	if first.err != nil {
		t.Fatal(first.err)
	}
	stored, err := db.GetMetadataCache("test", "pkg")
	if err != nil || stored == nil {
		t.Fatalf("cache row after first fetch: %v, %v", stored, err)
	}

	for range 2 {
		res := proxy.fetchMetadataDocument(ctx, "test", "pkg", upstream.URL+"/pkg", contentTypeJSON)
		if res.err != nil || string(res.body) != body || res.digest != first.digest {
			t.Fatalf("revalidated fetch = %q, digest %q, err %v; want %q, digest %q", res.body, res.digest, res.err, body, first.digest)
		}
	}
	if n := notModified.Load(); n != 2 {
		t.Fatalf("upstream answered 304 %d times, want 2", n)
	}
	if store.stores != 1 {
		t.Errorf("document stored %d times, want once", store.stores)
	}
	refreshed, err := db.GetMetadataCache("test", "pkg")
	if err != nil {
		t.Fatal(err)
	}
	if !refreshed.FetchedAt.Time.After(stored.FetchedAt.Time) {
		t.Errorf("fetched_at = %v, want later than %v", refreshed.FetchedAt.Time, stored.FetchedAt.Time)
	}
	if refreshed.ContentDigest != stored.ContentDigest || refreshed.ETag != stored.ETag {
		t.Errorf("row validators changed: %+v, want %+v", refreshed, stored)
	}
}

// TestMetadataNotModifiedStoresRowWithoutDigest checks that a 304 for a row
// written before digests were recorded stores the document as before, so the
// row gains its digest.
func TestMetadataNotModifiedStoresRowWithoutDigest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		t.Errorf("unconditional upstream request")
	}))
	t.Cleanup(upstream.Close)

	proxy, db, store, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Nanosecond
	proxy.HTTPClient = upstream.Client()

	path := metadataStoragePath("test", "pkg")
	store.files[path] = []byte(`{"v":1}`)
	if err := db.UpsertMetadataCache(&database.MetadataCacheEntry{
		Ecosystem:   "test",
		Name:        "pkg",
		StoragePath: path,
		ETag:        sql.NullString{String: `"v1"`, Valid: true},
		FetchedAt:   sql.NullTime{Time: time.Now().Add(-time.Hour), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}

	res := proxy.fetchMetadataDocument(context.Background(), "test", "pkg", upstream.URL+"/pkg", contentTypeJSON)
	if res.err != nil || string(res.body) != `{"v":1}` {
		t.Fatalf("fetch = %q, %v", res.body, res.err)
	}
	if want := "sha256:" + sha256Hex(`{"v":1}`); res.digest != want {
		t.Errorf("digest = %q, want %q", res.digest, want)
	}
	if store.stores != 1 {
		t.Errorf("document stored %d times, want once", store.stores)
	}
}
