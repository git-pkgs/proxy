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

	"github.com/git-pkgs/cooldown"
	registryclient "github.com/git-pkgs/registries/client"
	"github.com/git-pkgs/registries/fetch"
)

// composerLookupUpstream serves v/p's metadata listing versions and the
// archives they point at, counts metadata and archive requests, and fails
// every request while down is set, with downStatus or 503. A version listed in
// published carries that publish time.
type composerLookupUpstream struct {
	h          http.Handler
	handler    *ComposerHandler
	proxy      *Proxy
	versions   atomic.Value // []string
	published  map[string]time.Time
	down       atomic.Bool
	downStatus atomic.Int32
	metadata   atomic.Int32
	archives   atomic.Int32
	routeURL   string
}

func setupComposerLookup(t *testing.T, cacheMetadata bool, versions ...string) *composerLookupUpstream {
	t.Helper()
	u := &composerLookupUpstream{published: map[string]time.Time{}}
	u.versions.Store(versions)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u.down.Load() {
			status := int(u.downStatus.Load())
			if status == 0 {
				status = http.StatusServiceUnavailable
			}
			w.Header().Set("Retry-After", "30")
			http.Error(w, "down", status)
			return
		}
		switch {
		case r.URL.Path == "/p2/v/p.json":
			u.metadata.Add(1)
			var entries []string
			for _, v := range u.versions.Load().([]string) {
				var published string
				if at, ok := u.published[v]; ok {
					published = fmt.Sprintf(`"time":%q,`, at.Format(time.RFC3339))
				}
				entries = append(entries, fmt.Sprintf(`{"version":%q,%s"dist":{"url":%q,"type":"zip"}}`, v, published, srv.URL+"/archives/"+v+".zip"))
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
	u.proxy = proxy
	u.handler = NewComposerHandlerWithUpstreams(proxy, "http://proxy", srv.URL, srv.URL)
	u.h = u.handler.Routes()
	u.routeURL = srv.URL
	return u
}

// route sends v/p to a custom repository, which is the same server under
// another name.
func (u *composerLookupUpstream) route() {
	u.h = u.handler.WithPackageRoutes(map[string]string{"v/*": u.routeURL}).Routes()
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

// TestComposerDownloadLookupUpstreamError checks that a download whose
// metadata lookup fails upstream answers 502, or 503 while upstream holds
// requests, and not 404: Composer treats 404 as the version being gone.
func TestComposerDownloadLookupUpstreamError(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			u := setupComposerLookup(t, false, "1.0.0")
			u.downStatus.Store(int32(status))
			u.down.Store(true)
			if w := u.get(t, "/files/v/p/1.0.0/1.0.0.zip"); w.Code != http.StatusBadGateway {
				t.Errorf("status = %d, want 502", w.Code)
			}
		})
	}

	t.Run("rate limited", func(t *testing.T) {
		u := setupComposerLookup(t, false, "1.0.0")
		gate := registryclient.NewRateLimitGate(http.DefaultTransport)
		u.proxy.HTTPClient = &http.Client{Transport: gate}
		u.proxy.RateLimits = gate
		u.downStatus.Store(http.StatusTooManyRequests)
		u.down.Store(true)
		w := u.get(t, "/files/v/p/1.0.0/1.0.0.zip")
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
			t.Errorf("status = %d, Retry-After %q; want 503 with Retry-After", w.Code, w.Header().Get("Retry-After"))
		}
	})

	t.Run("not listed", func(t *testing.T) {
		u := setupComposerLookup(t, false, "1.0.0")
		if w := u.get(t, "/files/v/p/2.0.0/2.0.0.zip"); w.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", w.Code)
		}
	})
}

// storedPublishTime returns the publish time recorded for v/p at version.
func (u *composerLookupUpstream) storedPublishTime(t *testing.T, version string) (time.Time, bool) {
	t.Helper()
	ver, err := u.proxy.DB.GetVersionByPURL(canonicalVersionPURL("composer", "v/p", version))
	if err != nil {
		t.Fatalf("reading versions row: %v", err)
	}
	if ver == nil || !ver.PublishedAt.Valid {
		return time.Time{}, false
	}
	return ver.PublishedAt.Time, true
}

func (u *composerLookupUpstream) wantMetadataRequests(t *testing.T, want int32) {
	t.Helper()
	if n := u.metadata.Load(); n != want {
		t.Errorf("upstream metadata requests = %d, want %d", n, want)
	}
}

// TestComposerCachedReleaseSkipsMetadata checks that a tagged release whose
// archive is cached is served without asking upstream for the metadata
// again, with and without cache_metadata, and with cooldown, which takes the
// publish time recorded when the archive was first resolved.
func TestComposerCachedReleaseSkipsMetadata(t *testing.T) {
	for _, tt := range []struct {
		name          string
		cacheMetadata bool
		cooldown      bool
	}{
		{"metadata not cached", false, false},
		{"metadata cached", true, false},
		{"cooldown", false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			u := setupComposerLookup(t, tt.cacheMetadata, "1.0.0")
			u.published["1.0.0"] = time.Now().Add(-30 * 24 * time.Hour)
			if tt.cooldown {
				u.proxy.Cooldown = &cooldown.Config{Default: "3d"}
			}
			u.download(t, "1.0.0")
			// With cache_metadata the cached copy expires; the archive must
			// not need it.
			u.proxy.MetadataTTL = 0
			u.download(t, "1.0.0")
			u.download(t, "1.0.0")
			u.wantMetadataRequests(t, 1)
			if n := u.archives.Load(); n != 1 {
				t.Errorf("upstream archive requests = %d, want 1", n)
			}
		})
	}
}

// TestComposerDownloadStoresPublishTime checks that resolving a tagged
// release records its publish time, so later downloads can check cooldown
// without the metadata.
func TestComposerDownloadStoresPublishTime(t *testing.T) {
	u := setupComposerLookup(t, false, "1.0.0")
	published := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	u.published["1.0.0"] = published
	u.download(t, "1.0.0")
	got, ok := u.storedPublishTime(t, "1.0.0")
	if !ok || !got.Equal(published) {
		t.Errorf("stored publish time = %v, %v, want %v", got, ok, published)
	}
}

// TestComposerCachedReleaseCooldownFromStoredTime checks that a cached
// archive whose stored publish time is still inside the cooldown window is
// withheld without the metadata, including when cooldown was enabled or
// raised after the archive was cached.
func TestComposerCachedReleaseCooldownFromStoredTime(t *testing.T) {
	u := setupComposerLookup(t, false, "1.0.0", "2.0.0")
	u.published["1.0.0"] = time.Now().Add(-5 * 24 * time.Hour)
	u.published["2.0.0"] = time.Now().Add(-1 * time.Hour)
	u.download(t, "2.0.0")
	u.proxy.Cooldown = &cooldown.Config{Default: "3d"}
	u.download(t, "1.0.0")

	if w := u.get(t, "/files/v/p/2.0.0/2.0.0.zip"); w.Code != http.StatusNotFound {
		t.Errorf("2.0.0 after enabling cooldown: status %d, want 404", w.Code)
	}
	u.proxy.Cooldown = &cooldown.Config{Default: "7d"}
	if w := u.get(t, "/files/v/p/1.0.0/1.0.0.zip"); w.Code != http.StatusNotFound {
		t.Errorf("1.0.0 after raising cooldown: status %d, want 404", w.Code)
	}
	u.wantMetadataRequests(t, 2)
}

// TestComposerCachedReleaseWithoutStoredTime checks that with cooldown a
// cached archive the proxy recorded no publish time for, such as one cached
// by an older version, is checked against the metadata, which then records
// the time.
func TestComposerCachedReleaseWithoutStoredTime(t *testing.T) {
	u := setupComposerLookup(t, false, "1.0.0")
	u.published["1.0.0"] = time.Now().Add(-1 * time.Hour)
	u.download(t, "1.0.0")
	if _, err := u.proxy.DB.Exec(`UPDATE versions SET published_at = NULL`); err != nil {
		t.Fatal(err)
	}

	u.proxy.Cooldown = &cooldown.Config{Default: "3d"}
	if w := u.get(t, "/files/v/p/1.0.0/1.0.0.zip"); w.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404", w.Code)
	}
	u.wantMetadataRequests(t, 2)
	if _, ok := u.storedPublishTime(t, "1.0.0"); !ok {
		t.Error("publish time not recorded from the metadata")
	}
}

// TestComposerCachedDevVersionUsesMetadata checks that a branch, whose
// archive and publish time change with every push, is still resolved from
// the metadata on each download and records no publish time.
func TestComposerCachedDevVersionUsesMetadata(t *testing.T) {
	u := setupComposerLookup(t, false, "dev-main")
	u.published["dev-main"] = time.Now().Add(-30 * 24 * time.Hour)
	u.download(t, "dev-main")
	before := u.metadata.Load()
	u.download(t, "dev-main")
	if u.metadata.Load() == before {
		t.Error("cached branch archive served without consulting the metadata")
	}
	if _, ok := u.storedPublishTime(t, "dev-main"); ok {
		t.Error("publish time recorded for a branch")
	}
}

// TestComposerCachedRoutedReleaseUsesMetadata checks that a routed package
// keeps resolving every download from its route's metadata: the versions
// table is keyed by a PURL that does not say which repository the version
// came from, so it neither stores nor trusts a publish time for it.
func TestComposerCachedRoutedReleaseUsesMetadata(t *testing.T) {
	u := setupComposerLookup(t, false, "1.0.0")
	u.published["1.0.0"] = time.Now().Add(-1 * time.Hour)
	u.route()
	u.download(t, "1.0.0")
	u.download(t, "1.0.0")
	u.wantMetadataRequests(t, 2)
	if _, ok := u.storedPublishTime(t, "1.0.0"); ok {
		t.Error("publish time recorded for a routed package")
	}

	// A time stored for the public package of the same name is not used.
	if err := u.proxy.DB.SetVersionPublishedAt(canonicalVersionPURL("composer", "v/p", "1.0.0"),
		canonicalPackagePURL("composer", "v/p"), time.Now().Add(-30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	u.proxy.Cooldown = &cooldown.Config{Default: "3d"}
	if w := u.get(t, "/files/v/p/1.0.0/1.0.0.zip"); w.Code != http.StatusNotFound {
		t.Errorf("routed release in cooldown: status %d, want 404", w.Code)
	}
}

// TestComposerCachedReleaseDenied checks that the denylist still blocks a
// cached release served without the metadata.
func TestComposerCachedReleaseDenied(t *testing.T) {
	u := setupComposerLookup(t, false, "1.0.0")
	u.download(t, "1.0.0")
	setTestDenylist(t, u.proxy, "pkg:composer/v/p@1.0.0")
	if w := u.get(t, "/files/v/p/1.0.0/1.0.0.zip"); w.Code != http.StatusForbidden {
		t.Errorf("status %d, want 403", w.Code)
	}
}
