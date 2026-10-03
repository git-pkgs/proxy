package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/storage"
	"github.com/git-pkgs/registries/fetch"
)

const (
	testURLHost  = "downloads.example.org"
	testURLPath  = "/releases/tool-1.0.0.tar.gz"
	testURLRoute = "/" + testURLHost + testURLPath
	testPublic   = "http://rgw.example:7480/bucket/prefix"
)

// urlUpstream is a fake https origin serving body at testURLPath.
type urlUpstream struct {
	*httptest.Server
	body      atomic.Value // []byte
	available atomic.Bool
	requests  atomic.Int32
}

func newURLUpstream(t *testing.T, body []byte) *urlUpstream {
	t.Helper()
	u := &urlUpstream{}
	u.body.Store(body)
	u.available.Store(true)
	u.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !u.available.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path != testURLPath {
			http.NotFound(w, r)
			return
		}
		u.requests.Add(1)
		_, _ = w.Write(u.body.Load().([]byte))
	}))
	t.Cleanup(u.Close)
	return u
}

func newTestURLHandler(t *testing.T, upstream *urlUpstream, directServe bool) (*URLHandler, *Proxy, *mockStorage) {
	t.Helper()
	proxy, _, store, _ := setupTestProxy(t)
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy.Fetcher = fetcher
	h := NewURLHandler(proxy, directServe, time.Minute)
	h.upstreamBase = func(string) string { return upstream.URL }
	return h, proxy, store
}

func serveURLRequest(h *URLHandler, method, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(method, target, nil))
	return w
}

func sha256Of(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestParseURLRequest(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	tests := []struct {
		target   string
		ok       bool
		host     string
		path     string
		query    string
		digest   string
		filename string
	}{
		{"/www.boost.org/releases/boost-1.83.0.tar.gz", true, "www.boost.org", "/releases/boost-1.83.0.tar.gz", "", "", "boost-1.83.0.tar.gz"},
		{"/WWW.Boost.ORG/x.tgz", true, "www.boost.org", "/x.tgz", "", "", "x.tgz"},
		{"/api.github.com/repos/o/r/tarball/v1?a=1&b=2", true, "api.github.com", "/repos/o/r/tarball/v1", "a=1&b=2", "", "v1"},
		{"/sha256/" + strings.ToUpper(digest) + "/ftp.gnu.org/gnu/gawk/gawk-5.3.1.tar.xz", true, "ftp.gnu.org", "/gnu/gawk/gawk-5.3.1.tar.xz", "", digest, "gawk-5.3.1.tar.xz"},
		{"/example.org/dir/", true, "example.org", "/dir/", "", "", "download"},
		{"/example.org/a%20b%2Bc.tar.gz", true, "example.org", "/a%20b%2Bc.tar.gz", "", "", "a_b+c.tar.gz"},
		{"/1.2.3.4/file", true, "1.2.3.4", "/file", "", "", "file"},
		{"/sha256/abc/example.org/file", false, "", "", "", "", ""},
		{"/example.org", false, "", "", "", "", ""},
		{"/example.org/", false, "", "", "", "", ""},
		{"/localhost/file", false, "", "", "", "", ""},
		{"/example.org:8443/file", false, "", "", "", "", ""},
		{"/user@example.org/file", false, "", "", "", "", ""},
		{"/example.org/a/../../etc/passwd", false, "", "", "", "", ""},
		{"/-bad.example.org/file", false, "", "", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			req, ok := parseURLRequest(httptest.NewRequest(http.MethodGet, tt.target, nil))
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v (%+v)", ok, tt.ok, req)
			}
			if !ok {
				return
			}
			if req.host != tt.host || req.escapedPath != tt.path || req.query != tt.query ||
				req.digest != tt.digest || req.filename != tt.filename {
				t.Errorf("got %+v", req)
			}
			if len(req.version) != urlVersionHexLen {
				t.Errorf("version = %q", req.version)
			}
		})
	}
}

func TestParseURLRequest_VersionDependsOnPathAndQuery(t *testing.T) {
	parse := func(target string) string {
		req, ok := parseURLRequest(httptest.NewRequest(http.MethodGet, target, nil))
		if !ok {
			t.Fatalf("parse %s failed", target)
		}
		return req.version
	}
	a := parse("/example.org/f.tgz")
	if a != parse("/sha256/"+strings.Repeat("0", 64)+"/example.org/f.tgz") {
		t.Error("digest changed the version")
	}
	if a == parse("/example.org/f.tgz?x=1") || a == parse("/example.org/g/f.tgz") {
		t.Error("distinct URLs share a version")
	}
}

func TestURLHandler_RejectsMethodsAndBadPaths(t *testing.T) {
	h := NewURLHandler(testProxy(), false, time.Minute)
	if w := serveURLRequest(h, http.MethodPost, testURLRoute); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: status = %d", w.Code)
	}
	if w := serveURLRequest(h, http.MethodGet, "/localhost/x"); w.Code != http.StatusBadRequest {
		t.Errorf("bad host: status = %d", w.Code)
	}
}

func TestURLHandler_StreamsAndCaches(t *testing.T) {
	body := []byte("tarball bytes")
	upstream := newURLUpstream(t, body)
	h, _, _ := newTestURLHandler(t, upstream, false)

	for i := range 2 {
		w := serveURLRequest(h, http.MethodGet, testURLRoute)
		if w.Code != http.StatusOK || w.Body.String() != string(body) {
			t.Fatalf("request %d: status %d body %q", i, w.Code, w.Body.String())
		}
		if got := w.Header().Get("Location"); got != "" {
			t.Errorf("request %d: unexpected redirect %s", i, got)
		}
	}
	if got := upstream.requests.Load(); got != 1 {
		t.Errorf("upstream requests = %d, want 1", got)
	}

	// Immutable: served from cache while the origin is down.
	upstream.available.Store(false)
	if w := serveURLRequest(h, http.MethodGet, testURLRoute); w.Code != http.StatusOK {
		t.Errorf("origin down: status = %d", w.Code)
	}
}

func TestURLHandler_HeadAnswersFromRecord(t *testing.T) {
	body := []byte("head me")
	upstream := newURLUpstream(t, body)
	h, proxy, _ := newTestURLHandler(t, upstream, true)
	proxy.DirectServePublicURL = testPublic

	w := serveURLRequest(h, http.MethodHead, testURLRoute)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if w.Body.Len() != 0 || w.Header().Get("Location") != "" {
		t.Errorf("HEAD got body %d bytes, Location %q", w.Body.Len(), w.Header().Get("Location"))
	}
	if got := w.Header().Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Errorf("Content-Length = %q", got)
	}
	if got := w.Header().Get("ETag"); got != `"`+sha256Of(body)+`"` {
		t.Errorf("ETag = %q", got)
	}
}

func TestURLHandler_Digest(t *testing.T) {
	body := []byte("pinned bytes")
	upstream := newURLUpstream(t, body)
	h, proxy, _ := newTestURLHandler(t, upstream, false)

	bad := "/sha256/" + strings.Repeat("0", 64) + testURLRoute
	if w := serveURLRequest(h, http.MethodGet, bad); w.Code != http.StatusBadGateway {
		t.Fatalf("mismatch: status = %d, want 502", w.Code)
	}
	if cached, _ := proxy.GetCachedArtifact(context.Background(), urlEcosystem, testURLHost,
		mustParse(t, testURLRoute).version, "tool-1.0.0.tar.gz"); cached != nil {
		t.Fatal("mismatching download was cached")
	}

	good := "/sha256/" + sha256Of(body) + testURLRoute
	if w := serveURLRequest(h, http.MethodGet, good); w.Code != http.StatusOK || w.Body.String() != string(body) {
		t.Fatalf("match: status %d body %q", w.Code, w.Body.String())
	}

	// The origin republished; a client pinning the new digest refetches.
	newBody := []byte("republished bytes")
	upstream.body.Store(newBody)
	before := upstream.requests.Load()
	w := serveURLRequest(h, http.MethodGet, "/sha256/"+sha256Of(newBody)+testURLRoute)
	if w.Code != http.StatusOK || w.Body.String() != string(newBody) {
		t.Fatalf("new digest: status %d body %q", w.Code, w.Body.String())
	}
	if upstream.requests.Load() != before+1 {
		t.Error("new digest did not refetch")
	}
}

func mustParse(t *testing.T, target string) urlRequest {
	t.Helper()
	req, ok := parseURLRequest(httptest.NewRequest(http.MethodGet, target, nil))
	if !ok {
		t.Fatalf("parse %s failed", target)
	}
	return req
}

func TestURLHandler_RedirectsToPublicURL(t *testing.T) {
	body := []byte("redirect me")
	upstream := newURLUpstream(t, body)
	h, proxy, store := newTestURLHandler(t, upstream, true)
	proxy.DirectServePublicURL = testPublic + "/"

	req := mustParse(t, testURLRoute)
	wantPrefix := testPublic + "/url/" + testURLHost + "/" + req.version + "/"

	var first string
	for i := range 2 {
		w := serveURLRequest(h, http.MethodGet, testURLRoute)
		if w.Code != http.StatusFound {
			t.Fatalf("request %d: status = %d: %s", i, w.Code, w.Body.String())
		}
		loc := w.Header().Get("Location")
		if !strings.HasPrefix(loc, wantPrefix) || !strings.HasSuffix(loc, "/tool-1.0.0.tar.gz") {
			t.Fatalf("request %d: Location = %s", i, loc)
		}
		key := strings.TrimPrefix(loc, testPublic+"/")
		if _, ok := store.files[key]; !ok {
			t.Fatalf("request %d: no stored object at %s", i, key)
		}
		if i == 0 {
			first = loc
		} else if loc != first {
			t.Errorf("hit redirected to %s, miss to %s", loc, first)
		}
	}
	if got := upstream.requests.Load(); got != 1 {
		t.Errorf("upstream requests = %d, want 1", got)
	}
}

func TestURLHandler_RedirectsToSignedURL(t *testing.T) {
	upstream := newURLUpstream(t, []byte("signed"))
	h, _, store := newTestURLHandler(t, upstream, true)
	store.signedURL = "https://bucket.s3.amazonaws.com/obj?X-Amz-Signature=abc"

	w := serveURLRequest(h, http.MethodGet, testURLRoute)
	if w.Code != http.StatusFound || w.Header().Get("Location") != store.signedURL {
		t.Fatalf("status %d Location %q", w.Code, w.Header().Get("Location"))
	}
}

func TestURLHandler_StreamsWhenRedirectUnsupported(t *testing.T) {
	body := []byte("no signing")
	upstream := newURLUpstream(t, body)
	h, _, _ := newTestURLHandler(t, upstream, true)

	for i := range 2 {
		w := serveURLRequest(h, http.MethodGet, testURLRoute)
		if w.Code != http.StatusOK || w.Body.String() != string(body) {
			t.Fatalf("request %d: status %d body %q", i, w.Code, w.Body.String())
		}
	}
}

func TestURLHandler_RefetchesMissingObject(t *testing.T) {
	for _, directServe := range []bool{true, false} {
		t.Run("direct_serve="+strconv.FormatBool(directServe), func(t *testing.T) {
			body := []byte("lifecycle expired me")
			upstream := newURLUpstream(t, body)
			h, proxy, store := newTestURLHandler(t, upstream, directServe)
			proxy.DirectServePublicURL = testPublic

			if w := serveURLRequest(h, http.MethodGet, testURLRoute); w.Code >= 400 {
				t.Fatalf("first: status = %d", w.Code)
			}
			clear(store.files) // as a bucket lifecycle rule would

			w := serveURLRequest(h, http.MethodGet, testURLRoute)
			switch {
			case directServe && w.Code != http.StatusFound:
				t.Fatalf("after expiry: status = %d, want 302", w.Code)
			case !directServe && (w.Code != http.StatusOK || w.Body.String() != string(body)):
				t.Fatalf("after expiry: status %d body %q", w.Code, w.Body.String())
			}
			if got := upstream.requests.Load(); got != 2 {
				t.Errorf("upstream requests = %d, want 2", got)
			}
			if len(store.files) != 1 {
				t.Errorf("stored objects = %d, want 1", len(store.files))
			}
			if directServe {
				key := strings.TrimPrefix(w.Header().Get("Location"), testPublic+"/")
				if _, ok := store.files[key]; !ok {
					t.Errorf("redirected to %s, which is not stored", key)
				}
			}

			// Gone again with the origin down: one failed refetch, no loop.
			clear(store.files)
			upstream.available.Store(false)
			if w := serveURLRequest(h, http.MethodGet, testURLRoute); w.Code != http.StatusBadGateway {
				t.Errorf("expired and origin down: status = %d, want 502", w.Code)
			}
			if got := upstream.requests.Load(); got != 2 {
				t.Errorf("upstream requests = %d, want 2 (unavailable requests are not counted)", got)
			}
		})
	}
}

// zeroReader yields n zero bytes without holding them in memory.
type zeroReader struct{ n int64 }

func (z *zeroReader) Read(p []byte) (int, error) {
	if z.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > z.n {
		p = p[:z.n]
	}
	clear(p)
	z.n -= int64(len(p))
	return len(p), nil
}

func TestURLHandler_LargeFileIsNotCapped(t *testing.T) {
	if testing.Short() {
		t.Skip("streams 120MB")
	}
	const size = 120 << 20 // over the 100MB metadata buffer cap
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(size))
		_, _ = io.Copy(w, &zeroReader{n: size})
	}))
	t.Cleanup(upstream.Close)

	ctx := context.Background()
	store, err := storage.OpenBucket(ctx, "file://"+filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	proxy, _, _, _ := setupTestProxy(t)
	proxy.Storage = store
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy.Fetcher = fetcher
	h := NewURLHandler(proxy, false, time.Minute)
	h.upstreamBase = func(string) string { return upstream.URL }

	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/big.example.org/ghc.tar.xz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	hash := sha256.New()
	n, err := io.Copy(hash, resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK || n != size {
		t.Fatalf("status %d, read %d bytes, err %v", resp.StatusCode, n, err)
	}
	want := sha256.New()
	_, _ = io.Copy(want, &zeroReader{n: size})
	if hex.EncodeToString(hash.Sum(nil)) != hex.EncodeToString(want.Sum(nil)) {
		t.Error("body hash mismatch")
	}
}
