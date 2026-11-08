package handler

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
)

// proxyCachedRequest runs one ProxyCached request for key "pkg".
func proxyCachedRequest(proxy *Proxy, upstreamURL string, header http.Header) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/pkg", nil)
	for k, v := range header {
		r.Header[k] = v
	}
	proxy.ProxyCached(w, r, upstreamURL+"/pkg", "test", "pkg")
	return w
}

// TestFetchedMetadataValidatorsDescribeBody checks that a document fetched
// fresh but not stored is served with upstream's validators, not those of the
// older row still in the cache.
func TestFetchedMetadataValidatorsDescribeBody(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(headerContentType, contentTypeJSON)
		if requests.Add(1) == 1 {
			w.Header().Set(headerETag, `"v1"`)
			_, _ = w.Write([]byte(`{"v":1}`))
			return
		}
		w.Header().Set(headerETag, `"v2"`)
		_, _ = w.Write([]byte(`{"v":2}`))
	}))
	t.Cleanup(upstream.Close)

	proxy, _, store, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Millisecond
	proxy.HTTPClient = upstream.Client()

	if w := proxyCachedRequest(proxy, upstream.URL, nil); w.Code != http.StatusOK {
		t.Fatalf("first request: %d", w.Code)
	}
	time.Sleep(5 * time.Millisecond)
	store.mu.Lock()
	store.storeErr = errors.New("disk full")
	store.mu.Unlock()

	w := proxyCachedRequest(proxy, upstream.URL, nil)
	if w.Code != http.StatusOK || w.Body.String() != `{"v":2}` {
		t.Fatalf("second request = %d %q, want v2", w.Code, w.Body)
	}
	if got := w.Header().Get(headerETag); got != `"v2"` {
		t.Errorf("ETag = %q, want %q", got, `"v2"`)
	}
	if got := w.Header().Get("Warning"); got != "" {
		t.Errorf("Warning = %q on a fresh document", got)
	}

	// A client holding v1 must get v2, not a 304.
	w = proxyCachedRequest(proxy, upstream.URL, http.Header{"If-None-Match": {`"v1"`}})
	if w.Code != http.StatusOK || w.Body.String() != `{"v":2}` {
		t.Errorf("conditional request = %d %q, want 200 v2", w.Code, w.Body)
	}
}

// TestStaleMetadataMarkedWithoutTTL checks that the stale fallback carries
// the Warning header with metadata_ttl 0, where every request revalidates.
func TestStaleMetadataMarkedWithoutTTL(t *testing.T) {
	upstream := upstreamThen(t, `{"v":1}`, http.StatusBadGateway)
	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = 0
	proxy.HTTPClient = upstream.Client()

	if w := proxyCachedRequest(proxy, upstream.URL, nil); w.Code != http.StatusOK {
		t.Fatalf("first request: %d", w.Code)
	}
	w := proxyCachedRequest(proxy, upstream.URL, nil)
	if w.Code != http.StatusOK || w.Body.String() != `{"v":1}` {
		t.Fatalf("fallback = %d %q", w.Code, w.Body)
	}
	if got := w.Header().Get("Warning"); got != staleWarning {
		t.Errorf("Warning = %q, want %q", got, staleWarning)
	}
}

// seedMetadataRow stores {"v":1} for key "pkg" with ETag "v1" and a row past
// its TTL.
func seedMetadataRow(t *testing.T, db *database.DB, store *mockStorage) string {
	t.Helper()
	const body, etag = `{"v":1}`, `"v1"`
	path := metadataStoragePath("test", "pkg")
	store.mu.Lock()
	store.files[path] = []byte(body)
	store.mu.Unlock()
	if err := db.UpsertMetadataCache(&database.MetadataCacheEntry{
		Ecosystem:     "test",
		Name:          "pkg",
		StoragePath:   path,
		ETag:          sql.NullString{String: etag, Valid: true},
		ContentType:   sql.NullString{String: contentTypeJSON, Valid: true},
		ContentDigest: sql.NullString{String: "sha256:" + sha256Hex(body), Valid: true},
		Size:          sql.NullInt64{Int64: int64(len(body)), Valid: true},
		FetchedAt:     sql.NullTime{Time: time.Now().Add(-time.Hour), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	return path
}

// v2Upstream answers If-None-Match "v1" with 304 after running onNotModified,
// and any other request with v2.
func v2Upstream(t *testing.T, onNotModified func()) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			onNotModified()
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set(headerContentType, contentTypeJSON)
		w.Header().Set(headerETag, `"v2"`)
		_, _ = w.Write([]byte(`{"v":2}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestNotModifiedChecksStoredBytes checks that a 304 for a row whose blob has
// meanwhile been replaced (by another instance sharing the store) does not
// serve the new bytes under the old ETag.
func TestNotModifiedChecksStoredBytes(t *testing.T) {
	proxy, db, store, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Minute
	path := seedMetadataRow(t, db, store)
	upstream := v2Upstream(t, func() {
		// The other instance has written v2 but not yet its row.
		store.mu.Lock()
		store.files[path] = []byte(`{"v":2}`)
		store.mu.Unlock()
	})
	proxy.HTTPClient = upstream.Client()

	w := proxyCachedRequest(proxy, upstream.URL, nil)
	if w.Code != http.StatusOK || w.Body.String() != `{"v":2}` {
		t.Fatalf("response = %d %q", w.Code, w.Body)
	}
	if got := w.Header().Get(headerETag); got != `"v2"` {
		t.Errorf("ETag = %q, want %q", got, `"v2"`)
	}
	row, err := db.GetMetadataCache("test", "pkg")
	if err != nil || row == nil {
		t.Fatal(row, err)
	}
	if row.ETag.String != `"v2"` || row.ContentDigest.String != "sha256:"+sha256Hex(`{"v":2}`) {
		t.Errorf("row = etag %q digest %q, want v2's", row.ETag.String, row.ContentDigest.String)
	}
}

// TestNotModifiedAfterRowReplaced checks that a 304 for a row another
// instance has replaced since it was read does not store the old document
// back over the new one.
func TestNotModifiedAfterRowReplaced(t *testing.T) {
	proxy, db, store, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Minute
	path := seedMetadataRow(t, db, store)
	upstream := v2Upstream(t, func() {
		// The other instance has recorded v2's row; its blob is not written
		// yet.
		_ = db.UpsertMetadataCache(&database.MetadataCacheEntry{
			Ecosystem:     "test",
			Name:          "pkg",
			StoragePath:   path,
			ETag:          sql.NullString{String: `"v2"`, Valid: true},
			ContentType:   sql.NullString{String: contentTypeJSON, Valid: true},
			ContentDigest: sql.NullString{String: "sha256:" + sha256Hex(`{"v":2}`), Valid: true},
			Size:          sql.NullInt64{Int64: 7, Valid: true},
			FetchedAt:     sql.NullTime{Time: time.Now(), Valid: true},
		})
	})
	proxy.HTTPClient = upstream.Client()

	res := proxy.fetchMetadataDocument(context.Background(), "test", "pkg", upstream.URL+"/pkg", contentTypeJSON, nil)
	if res.err != nil || string(res.body) != `{"v":2}` {
		t.Fatalf("fetch = %q, %v; want v2", res.body, res.err)
	}
	row, err := db.GetMetadataCache("test", "pkg")
	if err != nil || row == nil {
		t.Fatal(row, err)
	}
	if row.ETag.String != `"v2"` {
		t.Errorf("row etag = %q, want %q", row.ETag.String, `"v2"`)
	}
}

// TestNotModifiedRejectedBodyNotRefreshed checks that a 304 for a stored
// document validate rejects does not mark that document fresh.
func TestNotModifiedRejectedBodyNotRefreshed(t *testing.T) {
	proxy, db, store, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Minute
	seedMetadataRow(t, db, store)
	before, err := db.GetMetadataCache("test", "pkg")
	if err != nil || before == nil {
		t.Fatal(before, err)
	}
	upstream := v2Upstream(t, func() {})
	proxy.HTTPClient = upstream.Client()

	reject := func(body []byte) error {
		if string(body) == `{"v":1}` {
			return errors.New("damaged")
		}
		return nil
	}
	proxy.fetchMetadataDocument(context.Background(), "test", "pkg", upstream.URL+"/pkg", contentTypeJSON, reject)
	row, err := db.GetMetadataCache("test", "pkg")
	if err != nil || row == nil {
		t.Fatal(row, err)
	}
	if !row.FetchedAt.Time.Equal(before.FetchedAt.Time) {
		t.Errorf("rejected v1 row fetched_at moved from %v to %v", before.FetchedAt.Time, row.FetchedAt.Time)
	}
}

// openHookStorage runs onOpen before the first Open.
type openHookStorage struct {
	*mockStorage
	once   sync.Once
	onOpen func()
}

func (s *openHookStorage) Open(ctx context.Context, path string) (io.ReadCloser, error) {
	s.once.Do(s.onOpen)
	return s.mockStorage.Open(ctx, path)
}

// TestStaleMetadataChecksStoredBytes checks that the stale fallback does not
// serve a blob another instance replaced after the row was read under that
// row's ETag.
func TestStaleMetadataChecksStoredBytes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(upstream.Close)
	proxy, db, store, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = 0
	proxy.HTTPClient = upstream.Client()
	path := seedMetadataRow(t, db, store)
	proxy.Storage = &openHookStorage{mockStorage: store, onOpen: func() {
		// The other instance has written v2 but not yet its row.
		store.mu.Lock()
		store.files[path] = []byte(`{"v":2}`)
		store.mu.Unlock()
	}}

	w := proxyCachedRequest(proxy, upstream.URL, http.Header{"If-None-Match": {`"v1"`}})
	if w.Code == http.StatusNotModified || w.Header().Get(headerETag) == `"v1"` {
		t.Errorf("response = %d ETag %q for v2's bytes, want no v1 validators", w.Code, w.Header().Get(headerETag))
	}
}
