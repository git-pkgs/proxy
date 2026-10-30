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
)

// bigPackument returns a packument laid out as registry.npmjs.org writes
// them, dist-tags before versions and time after, with n old versions padded
// so the document is large, and a latest published an hour ago.
func bigPackument(n int) string {
	var b strings.Builder
	b.WriteString(`{"_id":"big","name":"big","dist-tags":{"latest":"9.0.0","old":"1.0.0"},"versions":{`)
	times := []string{`"created":"2015-01-01T00:00:00Z"`}
	for i := range n {
		v := fmt.Sprintf("1.0.%d", i)
		if i == 0 {
			v = "1.0.0"
		}
		fmt.Fprintf(&b, `%q:{"name":"big","version":%q,"readme":%q,"dist":{"tarball":"https://registry.npmjs.org/big/-/big-%s.tgz"}},`,
			v, v, strings.Repeat("x", 512), v)
		times = append(times, fmt.Sprintf(`%q:"2016-01-01T00:%02d:%02dZ"`, v, i/60%60, i%60))
	}
	recent := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	b.WriteString(`"9.0.0":{"name":"big","version":"9.0.0","dist":{"tarball":"https://registry.npmjs.org/big/-/big-9.0.0.tgz"}}},`)
	times = append(times, `"9.0.0":"`+recent+`"`)
	b.WriteString(`"time":{` + strings.Join(times, ",") + `},"readme":"<b>big</b>"}`)
	return b.String()
}

type streamUpstream struct {
	*httptest.Server
	body     atomic.Value // string
	fail     atomic.Bool
	requests atomic.Int32
}

func newStreamUpstream(t *testing.T, body string) *streamUpstream {
	t.Helper()
	u := &streamUpstream{}
	u.body.Store(body)
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.requests.Add(1)
		switch {
		case u.fail.Load():
			http.Error(w, "down", http.StatusBadGateway)
		case r.URL.Path == "/big" && r.Header.Get("If-None-Match") == `"v1"`:
			w.WriteHeader(http.StatusNotModified)
		case r.URL.Path == "/big":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("ETag", `"v1"`)
			_, _ = io.WriteString(w, u.body.Load().(string))
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
	proxy.Cooldown = &cooldown.Config{Default: "3d"}
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

// TestNPMStreamedPackumentChangedInStorage checks that a document replaced
// in storage between the two passes isn't rewritten from mixed bytes.
func TestNPMStreamedPackumentChangedInStorage(t *testing.T) {
	u := newStreamUpstream(t, bigPackument(10))
	proxy, store, _ := streamProxy(t, u)
	entry, err := proxy.storedMetadataDocument(context.Background(), "npm", "big", u.URL+"/big", contentTypeJSON)
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
	if store.stores != 1 {
		t.Errorf("document stored %d times, want once", store.stores)
	}
}
