package handler

import (
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

// TestDownloadCooldownRefetchesMetadataLackingTheVersion caches each
// ecosystem's metadata before a release, within metadata_ttl. A download of
// the new release then has no publish time in the cached copy; the metadata
// is fetched again and the download decided on what upstream lists now. If
// that fetch fails, the download is refused as when no time can be read.
func TestDownloadCooldownRefetchesMetadataLackingTheVersion(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	before := map[string]time.Time{cooldownOldVersion: now.Add(-30 * 24 * time.Hour), cooldownUntimed: {}}
	after := map[string]time.Time{
		cooldownOldVersion:    now.Add(-30 * 24 * time.Hour),
		cooldownRecentVersion: now.Add(-time.Hour),
		cooldownUntimed:       {},
	}

	for _, c := range downloadCooldownCases() {
		if c.ecosystem == "helm" {
			// Charts are downloaded by digest, which a stale index lacks
			// differently; not covered here.
			continue
		}
		// expired has the cached copy past metadata_ttl, so it is served
		// stale while the upstream is down.
		for _, variant := range []struct {
			name                   string
			upstreamFails, expired bool
		}{{"released", false, false}, {"upstream-down", true, false}, {"stale", true, true}} {
			t.Run(c.ecosystem+"/"+variant.name, func(t *testing.T) {
				checkRefetch(t, c, before, after, variant.upstreamFails, variant.expired)
			})
		}
	}
}

// checkRefetch runs one case of
// TestDownloadCooldownRefetchesMetadataLackingTheVersion.
func checkRefetch(t *testing.T, c downloadCooldownCase, before, after map[string]time.Time, upstreamFails, expired bool) {
	t.Helper()
	var released, down atomic.Bool
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch {
		case down.Load():
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		case released.Load():
			c.metadata(w, r, after)
		default:
			c.metadata(w, r, before)
		}
	}))
	t.Cleanup(upstream.Close)
	proxy, _, _, fetcher := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	if expired {
		proxy.MetadataTTL = time.Nanosecond
	}
	fetcher.fetchErr = errors.New("artifact must be served from the cache")
	proxy.Cooldown = &cooldown.Config{Packages: map[string]string{
		canonicalPackagePURL(c.ecosystem, "demo"): "7d",
	}}
	handler := c.handler(proxy, upstream.URL)
	get := func(version string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, c.path(version), nil))
		return w
	}

	// Caches the metadata, which doesn't list the release yet.
	c.seedCachedArtifact(t, proxy, cooldownOldVersion)
	if w := get(cooldownOldVersion); w.Code != http.StatusOK {
		t.Fatalf("old version: status %d, body %q", w.Code, w.Body.String())
	}
	// A version listed without a time is not fetched again.
	if !expired {
		warmed := requests.Load()
		c.seedCachedArtifact(t, proxy, cooldownUntimed)
		if w := get(cooldownUntimed); w.Code != http.StatusOK || requests.Load() != warmed {
			t.Fatalf("untimed version: status %d, %d more upstream requests; want 200 and none",
				w.Code, requests.Load()-warmed)
		}
	}

	released.Store(true)
	down.Store(upstreamFails)
	if upstreamFails {
		// Fetchable, so only the cooldown check stops it.
		fetcher.fetchErr = nil
		fetcher.artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader("artifact"))}
	} else {
		c.seedCachedArtifact(t, proxy, cooldownRecentVersion)
	}
	w := get(cooldownRecentVersion)
	switch {
	case !upstreamFails && w.Code != http.StatusNotFound:
		t.Errorf("release in cooldown: status %d, want 404 (body %q)", w.Code, w.Body.String())
	case upstreamFails && w.Code < http.StatusInternalServerError:
		t.Errorf("release with upstream down: status %d, want a 5xx (body %q)", w.Code, w.Body.String())
	}
}

// TestDownloadCooldownFetchesUnlistedVersionOnce checks a download of a
// version that metadata just fetched from upstream doesn't list makes no
// second metadata request.
func TestDownloadCooldownFetchesUnlistedVersionOnce(t *testing.T) {
	times := map[string]time.Time{cooldownOldVersion: time.Now().Add(-30 * 24 * time.Hour)}
	for _, c := range downloadCooldownCases() {
		if c.ecosystem == "helm" {
			continue
		}
		t.Run(c.ecosystem, func(t *testing.T) {
			requests := func(version string) int32 {
				var n atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					n.Add(1)
					c.metadata(w, r, times)
				}))
				defer upstream.Close()
				proxy, _, _, fetcher := setupTestProxy(t)
				proxy.HTTPClient = upstream.Client()
				proxy.CacheMetadata = true
				proxy.MetadataTTL = time.Hour
				fetcher.fetchErr = errors.New("artifact must be served from the cache")
				proxy.Cooldown = &cooldown.Config{Packages: map[string]string{
					canonicalPackagePURL(c.ecosystem, "demo"): "7d",
				}}
				c.seedCachedArtifact(t, proxy, version)
				c.handler(proxy, upstream.URL).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, c.path(version), nil))
				return n.Load()
			}
			if listed, unlisted := requests(cooldownOldVersion), requests(cooldownRecentVersion); unlisted != listed {
				t.Errorf("metadata requests for an unlisted version = %d, for a listed one %d", unlisted, listed)
			}
		})
	}
}

// TestPyPIDownloadCooldownRefetchesReleasesLackingTheVersion is
// TestDownloadCooldownRefetchesMetadataLackingTheVersion for PyPI, whose
// release times come from the JSON API.
func TestPyPIDownloadCooldownRefetchesReleasesLackingTheVersion(t *testing.T) {
	now := time.Now()
	old := `"1.0.0": [{"upload_time_iso_8601": "` + now.Add(-30*24*time.Hour).Format(time.RFC3339) + `"}]`
	recent := `"2.0.0": [{"upload_time_iso_8601": "` + now.Add(-time.Hour).Format(time.RFC3339) + `"}]`
	for _, upstreamFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("upstream-down=%t", upstreamFails), func(t *testing.T) {
			var released, down atomic.Bool
			var metadataRequests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				metadataRequests.Add(1)
				switch {
				case down.Load():
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
				case released.Load():
					_, _ = io.WriteString(w, `{"releases": {`+old+`, `+recent+`}}`)
				default:
					_, _ = io.WriteString(w, `{"releases": {`+old+`}}`)
				}
			}))
			defer upstream.Close()
			proxy, _, _, fetcher := setupTestProxy(t)
			proxy.HTTPClient = upstream.Client()
			proxy.CacheMetadata = true
			proxy.MetadataTTL = time.Hour
			proxy.Cooldown = &cooldown.Config{Default: "7d"}
			fetcher.artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader("package data"))}
			h := &PyPIHandler{proxy: proxy, upstreamURL: upstream.URL, proxyURL: "http://localhost"}
			get := func(filename string) int {
				w := httptest.NewRecorder()
				h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/packages/packages/ab/cd/ef0123456789/"+filename, nil))
				return w.Code
			}

			// An unlisted version right after a fetch makes no second one.
			if code := get("newpkg-9.0.0.tar.gz"); code != http.StatusOK || metadataRequests.Load() != 1 {
				t.Fatalf("unlisted version: status %d, %d metadata requests; want 200 and 1", code, metadataRequests.Load())
			}
			released.Store(true)
			down.Store(upstreamFails)
			code := get("newpkg-2.0.0.tar.gz")
			switch {
			case !upstreamFails && code != http.StatusNotFound:
				t.Errorf("release in cooldown: status %d, want 404", code)
			case upstreamFails && code < http.StatusInternalServerError:
				t.Errorf("release with upstream down: status %d, want a 5xx", code)
			}
		})
	}
}

// TestCondaRefetchedIndexIsReused checks that, without the metadata cache,
// a repodata index fetched again for a newly released file replaces the one
// that lacked it, so the next download of the file doesn't fetch it again.
func TestCondaRefetchedIndexIsReused(t *testing.T) {
	var c downloadCooldownCase
	for _, candidate := range downloadCooldownCases() {
		if candidate.ecosystem == "conda" {
			c = candidate
		}
	}
	now := time.Now()
	var released atomic.Bool
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		times := map[string]time.Time{cooldownOldVersion: now.Add(-30 * 24 * time.Hour)}
		if released.Load() {
			times[cooldownRecentVersion] = now.Add(-time.Hour)
		}
		c.metadata(w, r, times)
	}))
	defer upstream.Close()
	proxy, _, _, fetcher := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	fetcher.fetchErr = errors.New("artifact must be served from the cache")
	proxy.Cooldown = &cooldown.Config{Default: "7d"}
	handler := c.handler(proxy, upstream.URL)
	get := func(version string) int {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, c.path(version), nil))
		return w.Code
	}
	c.seedCachedArtifact(t, proxy, cooldownOldVersion)
	c.seedCachedArtifact(t, proxy, cooldownRecentVersion)
	if code := get(cooldownOldVersion); code != http.StatusOK {
		t.Fatalf("old version: status %d", code)
	}
	released.Store(true)
	for range 2 {
		if code := get(cooldownRecentVersion); code != http.StatusNotFound {
			t.Fatalf("release in cooldown: status %d, want 404", code)
		}
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("repodata fetched %d times, want 2", got)
	}
}

// TestCondaConcurrentRefetchesShareOneIndex checks that concurrent downloads
// of a newly released file, finding it missing from the same kept index,
// fetch and index repodata past metadata_max_size once between them.
func TestCondaConcurrentRefetchesShareOneIndex(t *testing.T) {
	var c downloadCooldownCase
	for _, candidate := range downloadCooldownCases() {
		if candidate.ecosystem == "conda" {
			c = candidate
		}
	}
	now := time.Now()
	var released atomic.Bool
	var after atomic.Int32
	gate := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		times := map[string]time.Time{cooldownOldVersion: now.Add(-30 * 24 * time.Hour)}
		if released.Load() {
			after.Add(1)
			<-gate
			times[cooldownRecentVersion] = now.Add(-time.Hour)
		}
		c.metadata(w, r, times)
	}))
	defer upstream.Close()
	proxy, _, _, fetcher := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	proxy.MetadataMaxSize = 10 // every repodata is streamed
	fetcher.fetchErr = errors.New("artifact must be served from the cache")
	proxy.Cooldown = &cooldown.Config{Default: "7d"}
	handler := c.handler(proxy, upstream.URL)
	get := func(version string) int {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, c.path(version), nil))
		return w.Code
	}
	c.seedCachedArtifact(t, proxy, cooldownOldVersion)
	c.seedCachedArtifact(t, proxy, cooldownRecentVersion)
	if code := get(cooldownOldVersion); code != http.StatusOK {
		t.Fatalf("old version: status %d", code)
	}

	released.Store(true)
	const downloads = 5
	codes := make(chan int, downloads)
	for range downloads {
		go func() { codes <- get(cooldownRecentVersion) }()
	}
	// Lets every download reach the refetch before upstream answers.
	time.Sleep(200 * time.Millisecond)
	close(gate)
	for range downloads {
		if code := <-codes; code != http.StatusNotFound {
			t.Errorf("release in cooldown: status %d, want 404", code)
		}
	}
	// The refetch tries the metadata cache's fetch first, then streams.
	if got := after.Load(); got != 2 {
		t.Errorf("repodata fetched %d times for %d downloads, want 2", got, downloads)
	}
}
