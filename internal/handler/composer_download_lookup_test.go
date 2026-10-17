package handler

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/registries/fetch"
)

// composerLookupUpstream serves v/p's metadata listing versions and the
// archives they point at, counts metadata and archive requests, and fails
// every request while down is set.
type composerLookupUpstream struct {
	h        http.Handler
	versions atomic.Value // []string
	down     atomic.Bool
	metadata atomic.Int32
	archives atomic.Int32
}

func setupComposerLookup(t *testing.T, cacheMetadata bool, versions ...string) *composerLookupUpstream {
	t.Helper()
	u := &composerLookupUpstream{}
	u.versions.Store(versions)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u.down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		switch {
		case r.URL.Path == "/p2/v/p.json":
			u.metadata.Add(1)
			var entries []string
			for _, v := range u.versions.Load().([]string) {
				entries = append(entries, fmt.Sprintf(`{"version":%q,"dist":{"url":%q,"type":"zip"}}`, v, srv.URL+"/archives/"+v+".zip"))
			}
			_, _ = fmt.Fprintf(w, `{"packages":{"v/p":[%s]}}`, strings.Join(entries, ","))
		case r.URL.Path == "/p2/v/p~dev.json":
			u.metadata.Add(1)
			http.NotFound(w, r)
		case strings.HasPrefix(r.URL.Path, "/archives/"):
			u.archives.Add(1)
			_, _ = io.WriteString(w, "archive "+r.URL.Path)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = cacheMetadata
	proxy.MetadataTTL = time.Hour
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(srv.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	u.h = NewComposerHandlerWithUpstreams(proxy, "http://proxy", srv.URL, srv.URL).Routes()
	return u
}

func (u *composerLookupUpstream) get(t *testing.T, target string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	u.h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	return w
}

func (u *composerLookupUpstream) download(t *testing.T, version string) {
	t.Helper()
	w := u.get(t, "/files/v/p/"+version+"/"+version+".zip")
	if want := "archive /archives/" + version + ".zip"; w.Code != http.StatusOK || w.Body.String() != want {
		t.Fatalf("download %s: status %d, body %q, want 200 %q", version, w.Code, w.Body.String(), want)
	}
}

// TestComposerDownloadResolvesFromCachedMetadata checks that downloads find
// their archive in the metadata Composer was just served, instead of fetching
// it from upstream again for every archive.
func TestComposerDownloadResolvesFromCachedMetadata(t *testing.T) {
	u := setupComposerLookup(t, true, "1.0.0")
	if w := u.get(t, "/p2/v/p.json"); w.Code != http.StatusOK {
		t.Fatalf("metadata: status %d", w.Code)
	}
	u.download(t, "1.0.0")
	u.download(t, "1.0.0")
	if n := u.metadata.Load(); n != 1 {
		t.Errorf("upstream metadata requests = %d, want 1 (the metadata request itself)", n)
	}
	if n := u.archives.Load(); n != 1 {
		t.Errorf("upstream archive requests = %d, want 1", n)
	}
}

// TestComposerCachedArchiveServedWhileUpstreamDown checks that a cached
// archive stays installable when upstream fails, rather than failing on the
// metadata lookup that only names where to fetch it from.
func TestComposerCachedArchiveServedWhileUpstreamDown(t *testing.T) {
	u := setupComposerLookup(t, true, "1.0.0")
	u.download(t, "1.0.0")
	u.down.Store(true)
	u.download(t, "1.0.0")
}

// TestComposerDownloadOfVersionNewerThanCachedMetadata checks that a version
// published after the metadata was cached, which a lock file can name before
// the cached copy expires, is still looked up upstream.
func TestComposerDownloadOfVersionNewerThanCachedMetadata(t *testing.T) {
	u := setupComposerLookup(t, true, "1.0.0")
	u.download(t, "1.0.0")
	u.versions.Store([]string{"1.0.0", "1.1.0"})
	u.download(t, "1.1.0")
}

// TestComposerDownloadWithoutMetadataCache checks that without cache_metadata
// a download asks upstream for the metadata once, as it always has.
func TestComposerDownloadWithoutMetadataCache(t *testing.T) {
	u := setupComposerLookup(t, false, "1.0.0")
	u.download(t, "1.0.0")
	if n := u.metadata.Load(); n != 1 {
		t.Errorf("upstream metadata requests = %d, want 1", n)
	}
}
