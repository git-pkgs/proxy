package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
)

// ageMetadataAccess moves a cache row's last access an hour back and returns
// that time.
func ageMetadataAccess(t *testing.T, db *database.DB, ecosystem, key string) time.Time {
	t.Helper()
	old := time.Now().Add(-time.Hour).UTC()
	query := db.Rebind(`UPDATE metadata_cache SET last_accessed_at = ? WHERE ecosystem = ? AND name = ?`)
	if _, err := db.Exec(query, old, ecosystem, key); err != nil {
		t.Fatalf("ageing last access: %v", err)
	}
	return old
}

func assertMetadataAccessedAfter(t *testing.T, db *database.DB, ecosystem, key string, old time.Time) {
	t.Helper()
	entry, err := db.GetMetadataCache(ecosystem, key)
	if err != nil || entry == nil {
		t.Fatalf("GetMetadataCache: %v, %v", entry, err)
	}
	if !entry.LastAccessedAt.Valid || !entry.LastAccessedAt.Time.After(old.Add(time.Minute)) {
		t.Errorf("last_accessed_at = %v, want the time it was served from cache", entry.LastAccessedAt)
	}
}

func TestFetchOrCacheMetadata_CacheHitMarksAccess(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"v":1}`))
	}))
	t.Cleanup(upstream.Close)

	proxy, db, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.HTTPClient = upstream.Client()
	ctx := context.Background()

	if _, _, err := proxy.FetchOrCacheMetadata(ctx, "test", "pkg", upstream.URL+"/pkg"); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	old := ageMetadataAccess(t, db, "test", "pkg")
	if _, _, err := proxy.FetchOrCacheMetadata(ctx, "test", "pkg", upstream.URL+"/pkg"); err != nil {
		t.Fatalf("cached fetch: %v", err)
	}
	assertMetadataAccessedAfter(t, db, "test", "pkg", old)
}

func TestProxyCached_NotModifiedMarksAccess(t *testing.T) {
	const body = `{"v":1}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(upstream.Close)

	proxy, db, store, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Nanosecond
	proxy.HTTPClient = upstream.Client()

	for range 2 {
		response := httptest.NewRecorder()
		proxy.ProxyCached(response, httptest.NewRequest(http.MethodGet, "/pkg", nil), upstream.URL+"/pkg", "test", "pkg")
		if response.Code != http.StatusOK || response.Body.String() != body {
			t.Fatalf("response = %d %q, want 200 %q", response.Code, response.Body.String(), body)
		}
		if store.stores != 1 {
			t.Fatalf("document stored %d times, want once", store.stores)
		}
		assertMetadataAccessedAfter(t, db, "test", "pkg", time.Now().Add(-time.Hour))
		ageMetadataAccess(t, db, "test", "pkg")
	}
}

func TestLoadContainerMetadata_MarksAccess(t *testing.T) {
	proxy, db, _, _ := setupTestProxy(t)
	h := NewContainerHandlerWithRegistry(proxy, "http://proxy.example.test", "")
	ctx := context.Background()

	if _, err := h.storeContainerMetadata(ctx, "oci-test", "key", []byte("{}"), "", "", "application/json", "", time.Time{}, time.Now()); err != nil {
		t.Fatalf("storing: %v", err)
	}
	old := ageMetadataAccess(t, db, "oci-test", "key")
	if entry, _, err := h.loadContainerMetadata(ctx, "oci-test", "key"); err != nil || entry == nil {
		t.Fatalf("loading: %v, %v", entry, err)
	}
	assertMetadataAccessedAfter(t, db, "oci-test", "key", old)
}

func TestLoadContainerManifest_MarksAccess(t *testing.T) {
	proxy, db, _, _ := setupTestProxy(t)
	h := NewContainerHandlerWithRegistry(proxy, "http://proxy.example.test", "")
	ctx := context.Background()

	manifest := &cachedContainerManifest{body: []byte("{}"), contentType: "application/json", fetchedAt: time.Now()}
	if err := h.storeContainerManifest(ctx, "key", manifest); err != nil {
		t.Fatalf("storing: %v", err)
	}
	old := ageMetadataAccess(t, db, containerManifestCacheEcosystem, "key")
	if got, err := h.loadContainerManifest(ctx, "key"); err != nil || got == nil {
		t.Fatalf("loading: %v, %v", got, err)
	}
	assertMetadataAccessedAfter(t, db, containerManifestCacheEcosystem, "key", old)
}

func TestLoadContainerTags_MarksAccess(t *testing.T) {
	proxy, db, _, _ := setupTestProxy(t)
	h := NewContainerHandlerWithRegistry(proxy, "http://proxy.example.test", "")
	ctx := context.Background()

	tags := &cachedContainerTags{body: []byte(`{"tags":[]}`), contentType: "application/json", fetchedAt: time.Now()}
	if err := h.storeContainerTags(ctx, "key", tags); err != nil {
		t.Fatalf("storing: %v", err)
	}
	old := ageMetadataAccess(t, db, containerTagsCacheEcosystem, "key")
	if got, err := h.loadContainerTags(ctx, "key"); err != nil || got == nil {
		t.Fatalf("loading: %v, %v", got, err)
	}
	assertMetadataAccessedAfter(t, db, containerTagsCacheEcosystem, "key", old)
}

func TestStoredRewrite_MarksAccess(t *testing.T) {
	proxy, db, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.SetMetadataRewriteCacheSize(1 << 20)

	body := []byte(`{}`)
	proxy.cacheMetadataBlob(t.Context(), "npm", "demo", metadataStoragePath("npm", "demo"), &upstreamMetadata{body: body})
	key := rewriteCacheKey("npm", "http://proxy", "demo", body)
	if _, err := proxy.rewrites.rewrite(t.Context(), key, body, func(b []byte) ([]byte, error) { return b, nil }); err != nil {
		t.Fatal(err)
	}
	old := ageMetadataAccess(t, db, "npm", "demo")
	if _, ok := proxy.storedRewrite("npm", "demo", "http://proxy", "demo"); !ok {
		t.Fatal("expected a stored rewrite")
	}
	assertMetadataAccessedAfter(t, db, "npm", "demo", old)
}

// TestArchHandler_RefreshRestoresEvictedBlob covers a database row whose blob
// size eviction removed while a refresh re-created the row: the next refresh
// with the same bytes stores the blob again, so the stale fallback works.
func TestArchHandler_RefreshRestoresEvictedBlob(t *testing.T) {
	db := []byte("core-db-bytes")
	upstream := newArchUpstream(t, map[string][]byte{"/core/os/x86_64/core.db": db})
	proxy := newArchTestProxy(t, upstream)
	h := NewArchHandler(proxy, "http://proxy.example", map[string]string{"archlinux": upstream.URL})
	path := "/archlinux/core/os/x86_64/core.db"

	assertArchBody(t, serveArchRequest(h, http.MethodGet, path, nil), "seed", db)
	entry, err := proxy.DB.GetMetadataCache(archEcosystem, h.metadataCacheKey("archlinux", upstream.URL, "core/os/x86_64/core.db"))
	if err != nil || entry == nil {
		t.Fatalf("metadata entry: %v, %v", entry, err)
	}
	if err := proxy.Storage.Delete(t.Context(), entry.StoragePath); err != nil {
		t.Fatalf("deleting blob: %v", err)
	}

	ageMetadata(t, proxy, h, "archlinux", upstream.URL, "core/os/x86_64/core.db")
	assertArchBody(t, serveArchRequest(h, http.MethodGet, path, nil), "refresh", db)

	ageMetadata(t, proxy, h, "archlinux", upstream.URL, "core/os/x86_64/core.db")
	upstream.available.Store(false)
	assertArchBody(t, serveArchRequest(h, http.MethodGet, path, nil), "stale", db)
}
