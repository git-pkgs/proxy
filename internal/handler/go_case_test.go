package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/registries/fetch"
)

const (
	upperLogrusPath = "/github.com/!sirupsen/logrus/@v/v1.0.6.zip"
	lowerLogrusPath = "/github.com/sirupsen/logrus/@v/v1.0.6.zip"
)

func TestGoZipFilenameKeepsCase(t *testing.T) {
	lower := goZipFilename("github.com/sirupsen/logrus", "v1.0.6")
	if lower != "logrus@v1.0.6.zip" {
		t.Errorf("lowercase module filename = %q, want the plain name", lower)
	}
	upper := goZipFilename("github.com/Sirupsen/logrus", "v1.0.6")
	if upper == lower || !strings.HasPrefix(upper, "logrus@v1.0.6~") || !strings.HasSuffix(upper, ".zip") {
		t.Errorf("uppercase module filename = %q, want logrus@v1.0.6~{hash}.zip", upper)
	}
	if other := goZipFilename("github.com/SIRUPSEN/logrus", "v1.0.6"); other == upper {
		t.Errorf("differently cased modules share filename %q", other)
	}
}

// goCaseUpstream serves different bytes for each module path and counts
// requests per path.
func goCaseUpstream(t *testing.T) (*httptest.Server, map[string]*atomic.Int32) {
	t.Helper()
	counts := map[string]*atomic.Int32{upperLogrusPath: {}, lowerLogrusPath: {}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count, ok := counts[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		count.Add(1)
		_, _ = io.WriteString(w, "zip of "+r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	return srv, counts
}

func newGoCaseHandler(t *testing.T, upstream *httptest.Server) (http.Handler, *Proxy) {
	t.Helper()
	proxy, _, store, _ := setupTestProxy(t)
	store.seekable = true
	proxy.HTTPClient = upstream.Client()
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	return NewGoHandlerWithUpstream(proxy, "http://localhost", upstream.URL).Routes(), proxy
}

func getGo(h http.Handler, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

func TestGoModulesDifferingInCaseAreCachedApart(t *testing.T) {
	upstream, counts := goCaseUpstream(t)
	h, _ := newGoCaseHandler(t, upstream)

	for range 2 {
		for _, path := range []string{upperLogrusPath, lowerLogrusPath} {
			if w := getGo(h, path); w.Code != http.StatusOK || w.Body.String() != "zip of "+path {
				t.Fatalf("GET %s: status = %d, body = %q", path, w.Code, w.Body)
			}
		}
	}
	for path, count := range counts {
		if n := count.Load(); n != 1 {
			t.Errorf("%s fetched %d times, want once and then cached", path, n)
		}
	}
}

// Before module paths kept their case, the uppercase module was cached under
// the lowercase module's plain filename. The lowercase module must not be
// served those bytes.
func TestGoZipCachedForAnotherModuleIsRefetched(t *testing.T) {
	upstream, counts := goCaseUpstream(t)
	h, proxy := newGoCaseHandler(t, upstream)
	seedGoZip(t, proxy, "https://proxy.golang.org"+upperLogrusPath, "zip of "+upperLogrusPath)

	if w := getGo(h, lowerLogrusPath); w.Code != http.StatusOK || w.Body.String() != "zip of "+lowerLogrusPath {
		t.Fatalf("status = %d, body = %q, want the lowercase module's zip", w.Code, w.Body)
	}
	if n := counts[lowerLogrusPath].Load(); n != 1 {
		t.Errorf("lowercase module fetched %d times, want 1", n)
	}
}

// A zip cached from another upstream host for the same module stays valid:
// only the module path decides whether a cached zip belongs to the request.
func TestGoZipFromAnotherUpstreamHostIsKept(t *testing.T) {
	upstream, counts := goCaseUpstream(t)
	h, proxy := newGoCaseHandler(t, upstream)
	seedGoZip(t, proxy, "https://old-proxy.example"+lowerLogrusPath, "zip of "+lowerLogrusPath)

	if w := getGo(h, lowerLogrusPath); w.Code != http.StatusOK || w.Body.String() != "zip of "+lowerLogrusPath {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body)
	}
	if n := counts[lowerLogrusPath].Load(); n != 0 {
		t.Errorf("zip fetched %d times, want it served from the cache", n)
	}
}

func TestGoLegacyCacheKeepsCaseDistinctModules(t *testing.T) {
	upstream, counts := goCaseUpstream(t)
	h, proxy := newGoCaseHandler(t, upstream)
	seedGoZip(t, proxy, upstream.URL+lowerLogrusPath, "zip of "+lowerLogrusPath)
	old := seedLegacyUpperGoZip(t, proxy, "https://old-proxy.example"+upperLogrusPath, "zip of "+upperLogrusPath)
	upstream.Close()

	for range 2 {
		for _, path := range []string{upperLogrusPath, lowerLogrusPath} {
			if w := getGo(h, path); w.Code != http.StatusOK || w.Body.String() != "zip of "+path {
				t.Fatalf("GET %s: status = %d, body = %q", path, w.Code, w.Body)
			}
		}
	}
	current, err := proxy.DB.GetArtifact("pkg:golang/github.com/Sirupsen/logrus@v1.0.6", old.Filename)
	if err != nil || current == nil {
		t.Fatalf("moved artifact = %+v, %v", current, err)
	}
	if current.ID != old.ID || current.StoragePath != old.StoragePath || current.FetchedAt != old.FetchedAt || current.HitCount != old.HitCount+2 {
		t.Errorf("artifact history changed: before=%+v, after=%+v", old, current)
	}
	legacy, err := proxy.DB.GetArtifact(old.VersionPURL, old.Filename)
	if err != nil || legacy != nil {
		t.Fatalf("legacy artifact = %+v, %v", legacy, err)
	}
	for path, count := range counts {
		if count.Load() != 0 {
			t.Errorf("unexpected upstream request: %s", path)
		}
	}
}

func TestGoLegacyCacheRequiresExactModuleOrigin(t *testing.T) {
	for _, origin := range []string{"", "https://proxy.example" + lowerLogrusPath} {
		t.Run(origin, func(t *testing.T) {
			upstream, counts := goCaseUpstream(t)
			h, proxy := newGoCaseHandler(t, upstream)
			seedLegacyUpperGoZip(t, proxy, origin, "another module")
			if w := getGo(h, upperLogrusPath); w.Code != http.StatusOK || w.Body.String() != "zip of "+upperLogrusPath {
				t.Fatalf("status = %d, body = %q", w.Code, w.Body)
			}
			if counts[upperLogrusPath].Load() != 1 {
				t.Fatal("archive with unproven identity was reused")
			}
		})
	}
}

func seedLegacyUpperGoZip(t *testing.T, proxy *Proxy, upstreamURL, content string) *database.Artifact {
	t.Helper()
	const packagePURL = "pkg:golang/github.com/sirupsen/logrus"
	const versionPURL = packagePURL + "@v1.0.6"
	const storagePath = "golang/logrus/v1.0.6/legacy/upper.zip"
	filename := goZipFilename("github.com/Sirupsen/logrus", "v1.0.6")
	if _, _, err := proxy.Storage.Store(context.Background(), storagePath, strings.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	artifact := testArtifact(content, versionPURL, filename, "application/zip")
	if err := proxy.updateCacheDB("golang", "github.com/Sirupsen/logrus", packagePURL, upstreamURL, storagePath, artifact, ""); err != nil {
		t.Fatal(err)
	}
	old, err := proxy.DB.GetArtifact(versionPURL, filename)
	if err != nil || old == nil {
		t.Fatalf("seeded artifact = %+v, %v", old, err)
	}
	return old
}

// seedGoZip caches content as github.com/sirupsen/logrus v1.0.6 under the
// plain filename, recorded as fetched from upstreamURL.
func seedGoZip(t *testing.T, proxy *Proxy, upstreamURL, content string) {
	t.Helper()
	const storagePath = "golang/logrus/v1.0.6/seed/logrus@v1.0.6.zip"
	store := proxy.Storage.(*mockStorage)
	if _, _, err := store.Store(context.Background(), storagePath, strings.NewReader(content)); err != nil {
		t.Fatalf("seeding storage: %v", err)
	}
	pkgPURL, versionPURL, err := packagePURLStrings("golang", "github.com/sirupsen/logrus", "v1.0.6")
	if err != nil {
		t.Fatalf("packagePURLStrings: %v", err)
	}
	artifact := testArtifact(content, versionPURL, "logrus@v1.0.6.zip", "application/zip")
	if err := proxy.updateCacheDB("golang", "github.com/sirupsen/logrus", pkgPURL, upstreamURL, storagePath, artifact, ""); err != nil {
		t.Fatalf("seeding cache record: %v", err)
	}
}
