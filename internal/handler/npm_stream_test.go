package handler

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/registries/fetch"
)

// bigPackument returns a packument laid out as registry.npmjs.org writes
// them, dist-tags before versions and time after, with n old versions padded
// so the document is large, and a latest published an hour ago.
func bigPackument(n int) string {
	return bigPackumentAt("https://registry.npmjs.org", n)
}

// bigPackumentAt is bigPackument with tarballs under base.
func bigPackumentAt(base string, n int) string {
	var b strings.Builder
	b.WriteString(`{"_id":"big","name":"big","dist-tags":{"latest":"9.0.0","old":"1.0.0"},"versions":{`)
	times := []string{`"created":"2015-01-01T00:00:00Z"`}
	for i := range n {
		v := fmt.Sprintf("1.0.%d", i)
		if i == 0 {
			v = "1.0.0"
		}
		fmt.Fprintf(&b, `%q:{"name":"big","version":%q,"readme":%q,"dist":{"tarball":"%s/big/-/big-%s.tgz"}},`,
			v, v, strings.Repeat("x", 512), base, v)
		times = append(times, fmt.Sprintf(`%q:"2016-01-01T00:%02d:%02dZ"`, v, i/60%60, i%60))
	}
	recent := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	b.WriteString(`"9.0.0":{"name":"big","version":"9.0.0","dist":{"tarball":"` + base + `/big/-/big-9.0.0.tgz"}}},`)
	times = append(times, `"9.0.0":"`+recent+`"`)
	b.WriteString(`"time":{` + strings.Join(times, ",") + `},"readme":"<b>big</b>"}`)
	return b.String()
}

type streamUpstream struct {
	*httptest.Server
	body atomic.Value // string
	etag atomic.Value // string
	fail atomic.Bool
	gone atomic.Bool
	// truncate makes upstream close the connection halfway through the
	// document it announced.
	truncate atomic.Bool
	requests atomic.Int32
	// sent counts the responses that carried the document.
	sent atomic.Int32
}

// replace makes upstream send body under a new ETag, so a revalidation
// gets it in a 200.
func (u *streamUpstream) replace(body, etag string) {
	u.body.Store(body)
	u.etag.Store(etag)
}

func newStreamUpstream(t *testing.T, body string) *streamUpstream {
	t.Helper()
	u := &streamUpstream{}
	u.replace(body, `"v1"`)
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.requests.Add(1)
		switch {
		case u.fail.Load():
			http.Error(w, "down", http.StatusBadGateway)
		case u.gone.Load():
			http.NotFound(w, r)
		case r.URL.Path == "/big" && r.Header.Get("If-None-Match") == u.etag.Load().(string):
			w.WriteHeader(http.StatusNotModified)
		case r.URL.Path == "/big":
			u.sent.Add(1)
			body := u.body.Load().(string)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("ETag", u.etag.Load().(string))
			if u.truncate.Load() {
				w.Header().Set("Content-Length", fmt.Sprint(len(body)))
				_, _ = io.WriteString(w, body[:len(body)/2])
				return
			}
			_, _ = io.WriteString(w, body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(u.Close)
	return u
}

func streamProxy(t *testing.T, u *streamUpstream) (*Proxy, *mockStorage, http.Handler) {
	t.Helper()
	proxy, _, store, _ := setupTestProxy(t)
	proxy.HTTPClient = u.Client()
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.SetMetadataRewriteCacheSize(64 << 20)
	// As the server does: a cooldown.Config parses itself on first use,
	// which concurrent requests would race on.
	policy, err := cooldown.NewPolicy(&cooldown.Config{Default: "3d"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	proxy.Cooldown = policy
	return proxy, store, NewNPMHandler(proxy, "http://proxy.example", u.URL).Routes()
}

type packumentDoc struct {
	DistTags map[string]string          `json:"dist-tags"`
	Versions map[string]json.RawMessage `json:"versions"`
	Time     map[string]string          `json:"time"`
}

func decodeStreamed(t *testing.T, rec *httptest.ResponseRecorder) packumentDoc {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.Bytes()
	if rec.Header().Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if body, err = io.ReadAll(zr); err != nil {
			t.Fatal(err)
		}
	}
	var doc packumentDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	return doc
}

// TestNPMStreamedPackument requests a packument larger than
// metadata_max_size with cooldown on, its time map after its versions and
// its latest inside cooldown. The in-memory rewrite can't read it at all.
func TestNPMStreamedPackument(t *testing.T) {
	const versions = 400
	body := bigPackument(versions)
	u := newStreamUpstream(t, body)
	proxy, _, handler := streamProxy(t, u)
	proxy.MetadataMaxSize = int64(len(body) / 4)

	for _, header := range []http.Header{nil, {"Accept-Encoding": {"gzip"}}} {
		doc := decodeStreamed(t, serve(handler, "/big", header))
		if len(doc.Versions) != versions {
			t.Errorf("%d versions, want %d without 9.0.0", len(doc.Versions), versions)
		}
		if _, ok := doc.Versions["9.0.0"]; ok {
			t.Error("9.0.0 is listed although it is inside cooldown")
		}
		if _, ok := doc.Time["9.0.0"]; ok {
			t.Error("time still lists 9.0.0")
		}
		if doc.DistTags["latest"] != fmt.Sprintf("1.0.%d", versions-1) || doc.DistTags["old"] != "1.0.0" {
			t.Errorf("dist-tags = %v, want latest moved to the newest version left", doc.DistTags)
		}
		if !strings.Contains(string(doc.Versions["1.0.0"]), `"tarball":"http://proxy.example/npm/big/-/big-1.0.0.tgz"`) {
			t.Errorf("tarball not rewritten: %s", doc.Versions["1.0.0"])
		}
	}
	if n := u.requests.Load(); n != 1 {
		t.Errorf("upstream asked %d times, want once", n)
	}

	// Without the rewrite cache the in-memory path runs, which the size
	// limit stops.
	proxy.SetMetadataRewriteCacheSize(0)
	if rec := serve(handler, "/big", nil); rec.Code == http.StatusOK {
		t.Error("in-memory rewrite served a packument over metadata_max_size; the test no longer covers the stream")
	}
}

// TestNPMStreamedPackumentMatchesInMemory checks that both paths serve the
// same document and ETag.
func TestNPMStreamedPackumentMatchesInMemory(t *testing.T) {
	body := bigPackument(50)
	u := newStreamUpstream(t, body)
	_, _, streamed := streamProxy(t, u)
	inMemory, _, handler := streamProxy(t, u)
	inMemory.CacheMetadata = false

	a, b := serve(streamed, "/big", nil), serve(handler, "/big", nil)
	if a.Code != http.StatusOK || b.Code != http.StatusOK {
		t.Fatalf("status %d and %d", a.Code, b.Code)
	}
	var ca, cb bytes.Buffer
	if err := json.Compact(&ca, a.Body.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := json.Compact(&cb, b.Body.Bytes()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ca.Bytes(), cb.Bytes()) {
		t.Error("streamed and in-memory rewrites differ")
	}
}

// TestNPMStreamedPackumentStale serves the stored copy when upstream fails
// after the TTL, and 404 for a package upstream doesn't have.
func TestNPMStreamedPackumentStale(t *testing.T) {
	u := newStreamUpstream(t, bigPackument(10))
	proxy, _, handler := streamProxy(t, u)
	decodeStreamed(t, serve(handler, "/big", nil))

	proxy.MetadataTTL = time.Nanosecond
	proxy.SetMetadataRewriteCacheSize(64 << 20) // drop the cached rewrite
	u.fail.Store(true)
	if doc := decodeStreamed(t, serve(handler, "/big", nil)); len(doc.Versions) != 10 {
		t.Errorf("stale copy has %d versions, want 10", len(doc.Versions))
	}

	u.fail.Store(false)
	if rec := serve(handler, "/missing", nil); rec.Code != http.StatusNotFound {
		t.Errorf("missing package: status %d, want 404", rec.Code)
	}
}

// TestNPMStreamedPackumentStaleOn404 checks that a stored copy past its TTL
// is served when upstream answers 404, as the in-memory path does.
func TestNPMStreamedPackumentStaleOn404(t *testing.T) {
	u := newStreamUpstream(t, bigPackument(10))
	proxy, _, handler := streamProxy(t, u)
	decodeStreamed(t, serve(handler, "/big", nil))

	proxy.MetadataTTL = time.Nanosecond
	u.gone.Store(true)
	for _, size := range []int64{64 << 20, 0} { // streamed, then in memory
		proxy.SetMetadataRewriteCacheSize(size)
		if doc := decodeStreamed(t, serve(handler, "/big", nil)); len(doc.Versions) != 10 {
			t.Errorf("rewrite cache %d: stale copy has %d versions, want 10", size, len(doc.Versions))
		}
	}
}

// TestNPMStreamedPackumentBlobMissing checks that a row whose document is
// gone from storage, within its TTL or revalidated by a 304 after it, fetches
// the document again, also when it is larger than metadata_max_size.
func TestNPMStreamedPackumentBlobMissing(t *testing.T) {
	for _, tt := range []struct {
		name string
		ttl  time.Duration
	}{
		{"fresh row", time.Hour},
		{"expired row", time.Nanosecond},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := bigPackument(400)
			u := newStreamUpstream(t, body)
			proxy, store, handler := streamProxy(t, u)
			proxy.MetadataMaxSize = int64(len(body) / 4)
			decodeStreamed(t, serve(handler, "/big", nil))

			path := metadataStoragePath("npm", "big")
			store.mu.Lock()
			delete(store.files, path)
			store.mu.Unlock()
			proxy.MetadataTTL = tt.ttl
			proxy.SetMetadataRewriteCacheSize(64 << 20)

			if doc := decodeStreamed(t, serve(handler, "/big", nil)); len(doc.Versions) != 400 {
				t.Errorf("served %d versions, want 400", len(doc.Versions))
			}
			if n := u.sent.Load(); n != 2 {
				t.Errorf("upstream sent the document %d times, want 2", n)
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			if string(store.files[path]) != body {
				t.Error("the document was not stored again")
			}
		})
	}
}

// TestNPMStreamedVersionBlobMissing checks that a version request and a
// tarball download fetch a packument again whose stored copy is gone while
// its row is within the TTL.
func TestNPMStreamedVersionBlobMissing(t *testing.T) {
	u := newStreamUpstream(t, "")
	u.body.Store(bigPackumentAt(u.URL, 100))
	proxy, store, handler := streamProxy(t, u)
	proxy.MetadataMaxSize = 1024
	fetcher := proxy.Fetcher.(*mockFetcher)
	fetcher.artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader("package"))}
	path := metadataStoragePath("npm", "big")

	for _, req := range []string{"/big/1.0.5", "/big/-/big-1.0.5.tgz"} {
		decodeStreamed(t, serve(handler, "/big", nil))
		store.mu.Lock()
		delete(store.files, path)
		store.mu.Unlock()
		proxy.SetMetadataRewriteCacheSize(64 << 20)

		if rec := serve(handler, req, nil); rec.Code != http.StatusOK {
			t.Errorf("%s: status %d: %.200s", req, rec.Code, rec.Body.String())
		}
		store.mu.Lock()
		_, ok := store.files[path]
		store.mu.Unlock()
		if !ok {
			t.Errorf("%s: the packument was not stored again", req)
		}
	}
}

// TestNPMStreamedPackumentChangedInStorage checks that a document replaced
// in storage between the two passes isn't rewritten from mixed bytes.
func TestNPMStreamedPackumentChangedInStorage(t *testing.T) {
	u := newStreamUpstream(t, bigPackument(10))
	proxy, store, _ := streamProxy(t, u)
	entry, err := proxy.storedMetadataDocument(context.Background(), "npm", "big", u.URL+"/big", contentTypeJSON, nil)
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.files[entry.StoragePath] = []byte(bigPackument(11))
	store.mu.Unlock()

	r, err := proxy.openStoredMetadata(context.Background(), entry)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if _, err := io.ReadAll(r); !errors.Is(err, errStoredMetadataChanged) {
		t.Errorf("read error %v, want errStoredMetadataChanged", err)
	}
}

// TestNPMStreamedPackumentRevalidates checks that a 304 after the TTL keeps
// the stored document and its cached rewrite, without storing it again.
func TestNPMStreamedPackumentRevalidates(t *testing.T) {
	u := newStreamUpstream(t, bigPackument(10))
	proxy, store, handler := streamProxy(t, u)
	first := serve(handler, "/big", nil)
	decodeStreamed(t, first)
	store.mu.Lock()
	stores := store.stores
	store.mu.Unlock()

	proxy.MetadataTTL = time.Nanosecond
	second := serve(handler, "/big", nil)
	decodeStreamed(t, second)
	if second.Header().Get("ETag") != first.Header().Get("ETag") {
		t.Errorf("ETag changed from %s to %s", first.Header().Get("ETag"), second.Header().Get("ETag"))
	}
	if n := u.requests.Load(); n != 2 {
		t.Errorf("upstream asked %d times, want 2", n)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.stores != stores {
		t.Errorf("the 304 stored the document again (%d stores, %d before)", store.stores, stores)
	}
}

// TestNPMStreamedPackumentKeepsFullCopy checks that an abbreviated packument
// from upstream, where cooldown needs the full one, doesn't replace the full
// copy in storage, which is served instead as when upstream fails.
func TestNPMStreamedPackumentKeepsFullCopy(t *testing.T) {
	full := bigPackument(10)
	u := newStreamUpstream(t, full)
	proxy, store, handler := streamProxy(t, u)
	decodeStreamed(t, serve(handler, "/big", nil))

	proxy.MetadataTTL = time.Nanosecond
	proxy.SetMetadataRewriteCacheSize(64 << 20)
	u.replace(`{"name":"big","modified":"2026-01-01T00:00:00Z","dist-tags":{"latest":"1.0.0"},"versions":{"1.0.0":{}}}`, `"v2"`)
	if doc := decodeStreamed(t, serve(handler, "/big", nil)); len(doc.Versions) != 10 {
		t.Errorf("served %d versions, want the 10 of the full copy", len(doc.Versions))
	}
	if n := u.sent.Load(); n != 2 {
		t.Fatalf("upstream sent a document %d times, want 2: the abbreviated one was not fetched", n)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if got := string(store.files[metadataStoragePath("npm", "big")]); got != full {
		t.Errorf("stored packument was replaced: %.80s", got)
	}
	for path := range store.files {
		if strings.Contains(path, ".staged-") {
			t.Errorf("staged copy %s left in storage", path)
		}
	}
}

// TestNPMStreamedPackumentStorageFails checks that a packument is still
// served, read into memory, when storing it fails.
func TestNPMStreamedPackumentStorageFails(t *testing.T) {
	u := newStreamUpstream(t, bigPackument(10))
	_, store, handler := streamProxy(t, u)
	store.storeErr = errors.New("no space left on device")
	doc := decodeStreamed(t, serve(handler, "/big", nil))
	if len(doc.Versions) != 10 || doc.DistTags["latest"] != "1.0.9" {
		t.Errorf("served %d versions, latest %q; want 10 and 1.0.9", len(doc.Versions), doc.DistTags["latest"])
	}
}

// TestNPMStreamedVersionMetadata resolves versions and dist-tags of a
// packument larger than metadata_max_size.
func TestNPMStreamedVersionMetadata(t *testing.T) {
	body := bigPackument(100)
	u := newStreamUpstream(t, body)
	proxy, _, handler := streamProxy(t, u)
	proxy.MetadataMaxSize = int64(len(body) / 4)

	for path, want := range map[string]string{
		"/big/1.0.5":  "1.0.5",
		"/big/latest": "1.0.99", // 9.0.0 is inside cooldown
		"/big/old":    "1.0.0",
	} {
		rec := serve(handler, path, nil)
		var v struct {
			Version string `json:"version"`
			Dist    struct {
				Tarball string `json:"tarball"`
			} `json:"dist"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &v) != nil {
			t.Errorf("%s: status %d: %.200s", path, rec.Code, rec.Body.String())
			continue
		}
		if v.Version != want || v.Dist.Tarball != "http://proxy.example/npm/big/-/big-"+want+".tgz" {
			t.Errorf("%s = %s with tarball %s, want %s", path, v.Version, v.Dist.Tarball, want)
		}
	}
	if rec := serve(handler, "/big/9.0.0", nil); rec.Code != http.StatusNotFound {
		t.Errorf("version inside cooldown: status %d, want 404", rec.Code)
	}
	if n := u.requests.Load(); n != 1 {
		t.Errorf("upstream asked %d times, want once", n)
	}
}

// TestNPMStreamedTarballCooldown checks the tarball download against a
// packument larger than metadata_max_size: the version's publish time and
// tarball URL are read from storage.
func TestNPMStreamedTarballCooldown(t *testing.T) {
	u := newStreamUpstream(t, "")
	u.body.Store(bigPackumentAt(u.URL, 100))
	proxy, _, handler := streamProxy(t, u)
	proxy.MetadataMaxSize = 1024
	fetcher := proxy.Fetcher.(*mockFetcher)
	fetcher.artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader("package"))}

	if rec := serve(handler, "/big/-/big-9.0.0.tgz", nil); rec.Code != http.StatusNotFound {
		t.Errorf("tarball inside cooldown: status %d, want 404", rec.Code)
	}
	if rec := serve(handler, "/big/-/big-1.0.5.tgz", nil); rec.Code != http.StatusOK || rec.Body.String() != "package" {
		t.Errorf("tarball: status %d, body %q", rec.Code, rec.Body.String())
	}
	if want := u.URL + "/big/-/big-1.0.5.tgz"; fetcher.fetchedURL != want {
		t.Errorf("fetched %s, want %s from the packument", fetcher.fetchedURL, want)
	}
}
