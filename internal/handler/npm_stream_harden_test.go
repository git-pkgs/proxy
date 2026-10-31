package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// bigMetadataFiles returns a copy of what is in storage under the metadata path
// of big, staged copies included.
func bigMetadataFiles(store *mockStorage) map[string]string {
	store.mu.Lock()
	defer store.mu.Unlock()
	files := map[string]string{}
	for path, b := range store.files {
		if strings.HasPrefix(path, metadataStoragePath("npm", "big")) {
			files[path] = string(b)
		}
	}
	return files
}

// TestNPMStreamedPackumentBlobCorrupt checks that a stored copy whose bytes
// no longer match the digest of its row, within its TTL, is fetched again
// rather than failing the request, also above metadata_max_size.
func TestNPMStreamedPackumentBlobCorrupt(t *testing.T) {
	body := bigPackument(400)
	u := newStreamUpstream(t, body)
	proxy, store, handler := streamProxy(t, u)
	proxy.MetadataMaxSize = int64(len(body) / 4)
	decodeStreamed(t, serve(handler, "/big", nil))

	path := metadataStoragePath("npm", "big")
	store.mu.Lock()
	store.files[path] = []byte(strings.Replace(body, "1.0.5", "1.0.X", 1))
	store.mu.Unlock()
	proxy.SetMetadataRewriteCacheSize(64 << 20)

	if doc := decodeStreamed(t, serve(handler, "/big", nil)); len(doc.Versions) != 400 {
		t.Errorf("served %d versions, want 400", len(doc.Versions))
	}
	if got := bigMetadataFiles(store)[path]; got != body {
		t.Error("the corrupt copy was not replaced")
	}
}

// TestNPMStreamedPackumentTruncated checks that a document upstream cuts
// off halfway neither replaces the stored copy, which is served instead,
// nor leaves anything behind, with and without cooldown, streamed and in
// memory.
func TestNPMStreamedPackumentTruncated(t *testing.T) {
	for _, tt := range []struct {
		name               string
		cooldown, inMemory bool
	}{
		{"cooldown", true, false},
		{"no cooldown", false, false},
		{"in memory with cooldown", true, true},
		{"in memory without cooldown", false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := bigPackument(400)
			u := newStreamUpstream(t, body)
			proxy, store, handler := streamProxy(t, u)
			if !tt.cooldown {
				proxy.Cooldown = nil
			}
			if tt.inMemory {
				proxy.SetMetadataRewriteCacheSize(0)
			} else {
				proxy.MetadataMaxSize = int64(len(body) / 4)
			}
			want := len(decodeStreamed(t, serve(handler, "/big", nil)).Versions)
			before := bigMetadataFiles(store)

			u.replace(bigPackument(401), `"v2"`)
			u.truncate.Store(true)
			proxy.MetadataTTL = time.Nanosecond
			if !tt.inMemory {
				proxy.SetMetadataRewriteCacheSize(64 << 20)
			}

			if doc := decodeStreamed(t, serve(handler, "/big", nil)); len(doc.Versions) != want {
				t.Errorf("served %d versions, want the stored %d", len(doc.Versions), want)
			}
			after := bigMetadataFiles(store)
			if len(after) != len(before) {
				t.Errorf("storage holds %d files, %d before", len(after), len(before))
			}
			for path, b := range before {
				if after[path] != b {
					t.Errorf("%s was changed", path)
				}
			}
		})
	}
}

// TestNPMStreamedPackumentTruncatedUncached checks that a document upstream
// cuts off halfway fails the request and is not kept, streamed and in memory.
func TestNPMStreamedPackumentTruncatedUncached(t *testing.T) {
	for _, inMemory := range []bool{false, true} {
		t.Run(map[bool]string{false: "streamed", true: "in memory"}[inMemory], func(t *testing.T) {
			body := bigPackument(400)
			u := newStreamUpstream(t, body)
			proxy, store, handler := streamProxy(t, u)
			proxy.Cooldown = nil
			if inMemory {
				proxy.SetMetadataRewriteCacheSize(0)
			} else {
				proxy.MetadataMaxSize = int64(len(body) / 4)
			}
			u.truncate.Store(true)

			if rec := serve(handler, "/big", nil); rec.Code != http.StatusBadGateway {
				t.Errorf("status %d, want 502", rec.Code)
			}
			if files := bigMetadataFiles(store); len(files) != 0 {
				t.Errorf("left %d files in storage", len(files))
			}
			if entry, _ := proxy.DB.GetMetadataCache("npm", "big"); entry != nil {
				t.Error("left a metadata cache row")
			}
		})
	}
}

// TestNPMStreamedPackumentUncachedErrors checks the answers to upstream
// failures with nothing cached.
func TestNPMStreamedPackumentUncachedErrors(t *testing.T) {
	for _, tt := range []struct {
		name string
		set  func(*streamUpstream)
		want int
	}{
		{"not found", func(u *streamUpstream) { u.gone.Store(true) }, http.StatusNotFound},
		{"upstream down", func(u *streamUpstream) { u.fail.Store(true) }, http.StatusBadGateway},
	} {
		t.Run(tt.name, func(t *testing.T) {
			u := newStreamUpstream(t, bigPackument(10))
			_, store, handler := streamProxy(t, u)
			tt.set(u)
			if rec := serve(handler, "/big", nil); rec.Code != tt.want {
				t.Errorf("status %d, want %d", rec.Code, tt.want)
			}
			if files := bigMetadataFiles(store); len(files) != 0 {
				t.Errorf("left %d files in storage", len(files))
			}
		})
	}
}

// TestNPMStreamedPackumentConcurrent checks that concurrent requests for an
// uncached packument share one upstream fetch and all get it.
func TestNPMStreamedPackumentConcurrent(t *testing.T) {
	body := bigPackument(400)
	u := newStreamUpstream(t, body)
	proxy, _, handler := streamProxy(t, u)
	proxy.MetadataMaxSize = int64(len(body) / 4)
	const n = 16
	var wg sync.WaitGroup
	recs := make([]*httptest.ResponseRecorder, n)
	for i := range n {
		wg.Go(func() { recs[i] = serve(handler, "/big", nil) })
	}
	wg.Wait()
	for _, rec := range recs {
		if doc := decodeStreamed(t, rec); len(doc.Versions) != 400 {
			t.Errorf("served %d versions, want 400", len(doc.Versions))
		}
	}
	if got := u.sent.Load(); got != 1 {
		t.Errorf("upstream sent the document %d times, want once", got)
	}
}

// TestNPMStreamedPackumentClientRevalidates checks that a client sending
// the ETag it got gets a 304, and a new ETag once the document changes.
func TestNPMStreamedPackumentClientRevalidates(t *testing.T) {
	body := bigPackument(400)
	u := newStreamUpstream(t, body)
	proxy, _, handler := streamProxy(t, u)
	proxy.MetadataMaxSize = int64(len(body) / 4)

	first := serve(handler, "/big", nil)
	etag := first.Header().Get("ETag")
	if first.Code != http.StatusOK || etag == "" {
		t.Fatalf("status %d, ETag %q", first.Code, etag)
	}
	if rec := serve(handler, "/big", http.Header{"If-None-Match": {etag}}); rec.Code != http.StatusNotModified {
		t.Errorf("same document: status %d, want 304", rec.Code)
	}

	u.replace(bigPackument(401), `"v2"`)
	proxy.MetadataTTL = time.Nanosecond
	rec := serve(handler, "/big", http.Header{"If-None-Match": {etag}})
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") == etag {
		t.Errorf("changed document: status %d, ETag %q (was %q)", rec.Code, rec.Header().Get("ETag"), etag)
	}
	if doc := decodeStreamed(t, rec); len(doc.Versions) != 401 {
		t.Errorf("served %d versions, want 401", len(doc.Versions))
	}
}
