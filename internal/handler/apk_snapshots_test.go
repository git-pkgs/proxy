package handler

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/registries/fetch"
)

const apkSnapshotIndexPath = "/v3.22/main/x86_64/APKINDEX.tar.gz"

// apkSnapshotUpstream serves the index held in index, initially "index v1",
// and counts package downloads.
type apkSnapshotUpstream struct {
	index     atomic.Value
	downloads atomic.Int32
	server    *httptest.Server
}

func newAPKSnapshotUpstream(t *testing.T) *apkSnapshotUpstream {
	t.Helper()
	u := &apkSnapshotUpstream{}
	u.index.Store("index v1")
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case apkSnapshotIndexPath:
			_, _ = w.Write([]byte(u.index.Load().(string)))
		case "/v3.22/main/x86_64/curl-8.5.0-r0.apk":
			u.downloads.Add(1)
			_, _ = w.Write([]byte("curl 8.5.0"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(u.server.Close)
	return u
}

// newSnapshotAPKHandler returns an APK handler with snapshots enabled whose
// metadata TTL is 0, so every index request asks upstream.
func newSnapshotAPKHandler(t *testing.T, upstreamURL string) (*APKHandler, *database.DB) {
	t.Helper()
	proxy, db, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = 0
	proxy.HTTPClient = http.DefaultClient
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(http.DefaultClient), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	h := NewAPKHandler(proxy, "http://proxy.example", map[string]string{"alpine": upstreamURL}).
		WithSnapshots(30 * 24 * time.Hour)
	return h, db
}

func snapshotCount(t *testing.T, db *database.DB) int {
	t.Helper()
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM metadata_snapshots`); err != nil {
		t.Fatal(err)
	}
	return n
}

func expectAPKResponse(t *testing.T, h *APKHandler, target string, status int, body string) {
	t.Helper()
	w := serveAPKRequest(h, target)
	if w.Code != status {
		t.Fatalf("%s: status = %d, want %d: %s", target, w.Code, status, w.Body.String())
	}
	if status == http.StatusOK && w.Body.String() != body {
		t.Errorf("%s: body = %q, want %q", target, w.Body.String(), body)
	}
}

// TestAPKSnapshots_ServeIndexCurrentAtDate records two versions of an index
// through the plain path and checks that each dated path serves the version
// that was current at the end of that day.
func TestAPKSnapshots_ServeIndexCurrentAtDate(t *testing.T) {
	upstream := newAPKSnapshotUpstream(t)
	h, db := newSnapshotAPKHandler(t, upstream.server.URL)

	expectAPKResponse(t, h, "/alpine"+apkSnapshotIndexPath, http.StatusOK, "index v1")
	// Pretend v1 was first stored on 2026-01-10.
	recorded := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`UPDATE metadata_snapshots SET valid_from_ms = ?`, recorded.UnixMilli()); err != nil {
		t.Fatal(err)
	}

	upstream.index.Store("index v2")
	expectAPKResponse(t, h, "/alpine"+apkSnapshotIndexPath, http.StatusOK, "index v2")
	// An unchanged index adds no snapshot.
	expectAPKResponse(t, h, "/alpine"+apkSnapshotIndexPath, http.StatusOK, "index v2")
	if got := snapshotCount(t, db); got != 2 {
		t.Fatalf("snapshots = %d, want 2", got)
	}

	today := time.Now().UTC().Format(apkSnapshotDateLayout)
	future := time.Now().UTC().AddDate(0, 0, 2).Format(apkSnapshotDateLayout)
	expectAPKResponse(t, h, "/alpine@2026-01-09"+apkSnapshotIndexPath, http.StatusNotFound, "")
	expectAPKResponse(t, h, "/alpine@2026-01-10"+apkSnapshotIndexPath, http.StatusOK, "index v1")
	expectAPKResponse(t, h, "/alpine@"+today+apkSnapshotIndexPath, http.StatusOK, "index v2")
	expectAPKResponse(t, h, "/alpine@"+future+apkSnapshotIndexPath, http.StatusNotFound, "")
	expectAPKResponse(t, h, "/alpine@2026-13-01"+apkSnapshotIndexPath, http.StatusNotFound, "")
	expectAPKResponse(t, h, "/other@2026-01-10"+apkSnapshotIndexPath, http.StatusNotFound, "")
	expectAPKResponse(t, h, "/alpine@2026-01-10/v3.22/community/x86_64/APKINDEX.tar.gz", http.StatusNotFound, "")
}

func TestAPKSnapshots_ConditionalRequest(t *testing.T) {
	upstream := newAPKSnapshotUpstream(t)
	h, _ := newSnapshotAPKHandler(t, upstream.server.URL)
	expectAPKResponse(t, h, "/alpine"+apkSnapshotIndexPath, http.StatusOK, "index v1")

	target := "/alpine@" + time.Now().UTC().Format(apkSnapshotDateLayout) + apkSnapshotIndexPath
	first := serveAPKRequest(h, target)
	etag := first.Header().Get("ETag")
	if first.Code != http.StatusOK || etag == "" {
		t.Fatalf("status = %d, ETag = %q, want 200 with an ETag", first.Code, etag)
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("If-None-Match", etag)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusNotModified {
		t.Errorf("conditional request: status = %d, want 304", w.Code)
	}
}

// TestAPKSnapshots_PackagesShareTheCache checks that a package requested
// under a snapshot path is the cached artifact of the plain path.
func TestAPKSnapshots_PackagesShareTheCache(t *testing.T) {
	upstream := newAPKSnapshotUpstream(t)
	h, _ := newSnapshotAPKHandler(t, upstream.server.URL)

	pkg := "/v3.22/main/x86_64/curl-8.5.0-r0.apk"
	expectAPKResponse(t, h, "/alpine"+pkg, http.StatusOK, "curl 8.5.0")
	expectAPKResponse(t, h, "/alpine@2026-01-10"+pkg, http.StatusOK, "curl 8.5.0")
	if got := upstream.downloads.Load(); got != 1 {
		t.Errorf("upstream downloads = %d, want 1", got)
	}
	expectAPKResponse(t, h, "/alpine@2026-01-10/v3.22/main/x86_64/gone-1.0-r0.apk", http.StatusNotFound, "")
}

func TestAPKSnapshots_DisabledByDefault(t *testing.T) {
	upstream := newAPKSnapshotUpstream(t)
	h, db := newSnapshotAPKHandler(t, upstream.server.URL)
	h.WithSnapshots(0)

	expectAPKResponse(t, h, "/alpine"+apkSnapshotIndexPath, http.StatusOK, "index v1")
	if got := snapshotCount(t, db); got != 0 {
		t.Errorf("snapshots = %d, want 0", got)
	}
	today := time.Now().UTC().Format(apkSnapshotDateLayout)
	expectAPKResponse(t, h, "/alpine@"+today+apkSnapshotIndexPath, http.StatusNotFound, "")
}

// TestAPKSnapshots_ConfiguredNameWins checks that a repository whose
// configured name contains '@' is served as configured, not as a snapshot.
func TestAPKSnapshots_ConfiguredNameWins(t *testing.T) {
	upstream := newAPKSnapshotUpstream(t)
	h, _ := newSnapshotAPKHandler(t, upstream.server.URL)
	h.repositories["alpine@2026-01-10"] = upstream.server.URL

	expectAPKResponse(t, h, "/alpine@2026-01-10"+apkSnapshotIndexPath, http.StatusOK, "index v1")
}

// TestAPKSnapshots_SameVersionRecordedTwice checks that two requests
// recording the same version in the same millisecond, as coalesced fetches
// after an upstream update can, leave the recorded snapshot readable: the
// request whose row is not added must not delete the other's blob.
func TestAPKSnapshots_SameVersionRecordedTwice(t *testing.T) {
	u := newAPKSnapshotUpstream(t)
	h, db := newSnapshotAPKHandler(t, u.server.URL)
	expectAPKResponse(t, h, "/alpine"+apkSnapshotIndexPath, http.StatusOK, "index v1")

	key := h.metadataCacheKey("alpine", u.server.URL, apkSnapshotIndexPath[1:])
	entry, err := db.GetMetadataCache(apkEcosystem, key)
	if err != nil || entry == nil {
		t.Fatalf("cached index row = %v, %v", entry, err)
	}
	if _, err := db.Exec(`DELETE FROM metadata_snapshots`); err != nil {
		t.Fatal(err)
	}
	validFrom := time.Now()
	h.storeSnapshot(t.Context(), key, entry, validFrom)
	h.storeSnapshot(t.Context(), key, entry, validFrom)

	if n := snapshotCount(t, db); n != 1 {
		t.Fatalf("%d snapshots recorded, want 1", n)
	}
	snap, err := db.LatestMetadataSnapshot(apkEcosystem, key)
	if err != nil || snap == nil {
		t.Fatalf("latest snapshot = %v, %v", snap, err)
	}
	body, err := h.proxy.Storage.Open(t.Context(), snap.StoragePath)
	if err != nil {
		t.Fatalf("recorded snapshot blob unreadable: %v", err)
	}
	_ = body.Close()
	day := validFrom.UTC().Format(apkSnapshotDateLayout)
	expectAPKResponse(t, h, "/alpine@"+day+apkSnapshotIndexPath, http.StatusOK, "index v1")
}
