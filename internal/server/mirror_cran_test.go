package server

import (
	"bytes"
	"compress/gzip"
	"crypto/md5" //nolint:gosec // CRAN publishes MD5 sums
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/handler"
)

const (
	cranCurrentPath  = "/src/contrib/example_1.1.tar.gz"
	cranArchivedPath = "/src/contrib/Archive/example/example_1.0.tar.gz"
	cranIndexPath    = "/src/contrib/PACKAGES.gz"
)

// cranUpstream serves a CRAN repository with example 1.1 current, listed
// with its MD5 in PACKAGES, and 0.9 and 1.0 archived. It counts requests by
// path.
type cranUpstream struct {
	*httptest.Server
	mu    sync.Mutex
	files map[string]string
	hits  map[string]int
}

// newCRANUpstream serves the files of example with bodies replaced by
// tampered, which keep the MD5 sums of the originals.
func newCRANUpstream(t *testing.T, tampered map[string]string) *cranUpstream {
	t.Helper()
	current := "example 1.1 source"
	sum := md5.Sum([]byte(current)) //nolint:gosec // see import
	var index bytes.Buffer
	zw := gzip.NewWriter(&index)
	_, _ = io.WriteString(zw, "Package: aaa\nVersion: 2.0\nMD5sum: 00000000000000000000000000000000\n\n"+
		"Package: example\nVersion: 1.1\nDepends: R (>= 4.0)\nMD5sum: "+hex.EncodeToString(sum[:])+"\nNeedsCompilation: no\n")
	_ = zw.Close()

	u := &cranUpstream{
		files: map[string]string{
			cranIndexPath:    index.String(),
			cranCurrentPath:  current,
			cranArchivedPath: "example 1.0 source",
			"/src/contrib/Archive/example/example_0.9.tar.gz": "example 0.9 source",
			"/web/packages/example/DESCRIPTION":               "Package: example\nVersion: 1.1\n",
			"/src/contrib/Archive/example/": `<a href="example_0.9.tar.gz">0.9</a>
<a href="example_1.0.tar.gz">1.0</a>`,
		},
		hits: map[string]int{},
	}
	for path, body := range tampered {
		u.files[path] = body
	}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		body, ok := u.files[r.URL.Path]
		u.hits[r.URL.Path]++
		u.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *cranUpstream) hitsFor(path string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits[path]
}

func cranMirrorConfig(upstream *cranUpstream) *config.Config {
	cfg := config.Default()
	cfg.Upstream.CRAN = upstream.URL
	return cfg
}

// getCRAN requests path from a CRAN handler for upstream.
func getCRAN(proxy *handler.Proxy, upstream *cranUpstream, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	cran := handler.NewCRANHandlerWithUpstream(proxy, "http://proxy.test", upstream.URL)
	cran.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestMirrorCRANSourcePackagesAreServedFromCache(t *testing.T) {
	upstream := newCRANUpstream(t, nil)

	progress, proxy := mirrorWithConfig(t, cranMirrorConfig(upstream), "pkg:cran/example@1.1", "pkg:cran/example@1.0")

	if progress.Completed != 2 || progress.Failed != 0 {
		t.Fatalf("completed = %d, failed = %d, errors = %v", progress.Completed, progress.Failed, progress.Errors)
	}
	for _, path := range []string{cranCurrentPath, cranArchivedPath} {
		if got := upstream.hitsFor(path); got != 1 {
			t.Errorf("%s fetched %d times, want 1", path, got)
		}
	}

	// R gets both from the cache once the upstream is down, the current
	// version also from Archive/, where CRAN moves it once a newer one is
	// published.
	upstream.Close()
	for path, want := range map[string]string{
		cranCurrentPath: "example 1.1 source",
		"/src/contrib/Archive/example/example_1.1.tar.gz": "example 1.1 source",
		cranArchivedPath: "example 1.0 source",
	} {
		if rec := getCRAN(proxy, upstream, path); rec.Code != http.StatusOK || rec.Body.String() != want {
			t.Errorf("%s from cache: status = %d, body = %q, want 200 and %q", path, rec.Code, rec.Body, want)
		}
	}
}

func TestMirrorCRANRefusesMD5Mismatch(t *testing.T) {
	upstream := newCRANUpstream(t, map[string]string{cranCurrentPath: "tampered 1.1"})

	progress, proxy := mirrorWithConfig(t, cranMirrorConfig(upstream), "pkg:cran/example@1.1")

	if progress.Failed != 1 || progress.Completed != 0 {
		t.Fatalf("completed = %d, failed = %d, errors = %v; want it refused", progress.Completed, progress.Failed, progress.Errors)
	}
	upstream.Close()
	if rec := getCRAN(proxy, upstream, cranCurrentPath); rec.Code == http.StatusOK {
		t.Errorf("%s was cached although its MD5 doesn't match", cranCurrentPath)
	}
}

func TestMirrorCRANAllVersions(t *testing.T) {
	upstream := newCRANUpstream(t, nil)

	progress, _ := mirrorWithConfig(t, cranMirrorConfig(upstream), "pkg:cran/example")

	if progress.Total != 3 || progress.Completed != 3 || progress.Failed != 0 {
		t.Fatalf("total = %d, completed = %d, failed = %d, errors = %v; want 0.9, 1.0 and 1.1 once each", progress.Total, progress.Completed, progress.Failed, progress.Errors)
	}
	if got := upstream.hitsFor(cranIndexPath); got != 1 {
		t.Errorf("PACKAGES.gz fetched %d times, want once", got)
	}
}
