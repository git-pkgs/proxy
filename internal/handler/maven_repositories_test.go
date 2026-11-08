package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/registries/fetch"
)

// mavenRepoUpstream serves label-specific bytes for every path it knows and
// counts its requests.
func mavenRepoUpstream(t *testing.T, label string, known ...string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		for _, p := range known {
			if r.URL.Path == p {
				_, _ = io.WriteString(w, label+" "+r.URL.Path)
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

func newMavenRepositoriesHandler(t *testing.T, central, portal, grails *httptest.Server) http.Handler {
	t.Helper()
	proxy, _, store, _ := setupTestProxy(t)
	store.seekable = true
	proxy.HTTPClient = central.Client()
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(central.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	return NewMavenHandler(proxy, "http://localhost", central.URL, portal.URL).
		WithRepositories(map[string]string{"grails": grails.URL + "/restricted/"}).Routes()
}

func getMaven(h http.Handler, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

const (
	mavenJarPath      = "/org/grails/grails-core/6.0.0/grails-core-6.0.0.jar"
	mavenMetadataPath = "/org/grails/grails-core/maven-metadata.xml"
)

func TestMavenRepositoriesAreServedFromTheirOwnUpstream(t *testing.T) {
	central, _ := mavenRepoUpstream(t, "central", mavenJarPath, mavenMetadataPath)
	portal, _ := mavenRepoUpstream(t, "portal")
	grails, _ := mavenRepoUpstream(t, "grails", "/restricted"+mavenJarPath, "/restricted"+mavenMetadataPath)
	h := newMavenRepositoriesHandler(t, central, portal, grails)

	// The same coordinates in both repositories are cached apart.
	for range 2 {
		if w := getMaven(h, "/grails"+mavenJarPath); w.Code != http.StatusOK || w.Body.String() != "grails /restricted"+mavenJarPath {
			t.Fatalf("named jar: status = %d, body = %q", w.Code, w.Body)
		}
		if w := getMaven(h, mavenJarPath); w.Code != http.StatusOK || w.Body.String() != "central "+mavenJarPath {
			t.Fatalf("default jar: status = %d, body = %q", w.Code, w.Body)
		}
	}

	if w := getMaven(h, "/grails"+mavenMetadataPath); w.Code != http.StatusOK || !strings.HasPrefix(w.Body.String(), "grails ") {
		t.Errorf("named metadata: status = %d, body = %q", w.Code, w.Body)
	}
	if w := getMaven(h, mavenMetadataPath); w.Code != http.StatusOK || !strings.HasPrefix(w.Body.String(), "central ") {
		t.Errorf("default metadata: status = %d, body = %q", w.Code, w.Body)
	}
}

func TestMavenRepositoriesDoNotFallBackToThePluginPortal(t *testing.T) {
	const pluginPath = "/org/example/plugin/1.0/plugin-1.0.jar"
	central, _ := mavenRepoUpstream(t, "central")
	portal, portalRequests := mavenRepoUpstream(t, "portal", pluginPath, "/org/example/plugin/maven-metadata.xml")
	grails, _ := mavenRepoUpstream(t, "grails")
	h := newMavenRepositoriesHandler(t, central, portal, grails)

	for _, path := range []string{"/grails" + pluginPath, "/grails/org/example/plugin/maven-metadata.xml"} {
		if w := getMaven(h, path); w.Code != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404", path, w.Code)
		}
	}
	if n := portalRequests.Load(); n != 0 {
		t.Errorf("named repository asked the plugin portal %d times", n)
	}
	if w := getMaven(h, pluginPath); w.Code != http.StatusOK || !strings.HasPrefix(w.Body.String(), "portal ") {
		t.Errorf("default repository: status = %d, body = %q, want the plugin portal's copy", w.Code, w.Body)
	}
}

func TestMavenRepositoryRootIsNotFound(t *testing.T) {
	central, centralRequests := mavenRepoUpstream(t, "central")
	portal, _ := mavenRepoUpstream(t, "portal")
	grails, _ := mavenRepoUpstream(t, "grails")
	h := newMavenRepositoriesHandler(t, central, portal, grails)

	for _, path := range []string{"/grails", "/grails/"} {
		if w := getMaven(h, path); w.Code != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404", path, w.Code)
		}
	}
	if n := centralRequests.Load(); n != 0 {
		t.Errorf("repository root went to the default upstream %d times", n)
	}
}

func TestMavenRepositoryRepointedServesTheNewUpstream(t *testing.T) {
	central, _ := mavenRepoUpstream(t, "central")
	portal, _ := mavenRepoUpstream(t, "portal")
	staging, _ := mavenRepoUpstream(t, "staging", mavenJarPath)
	production, _ := mavenRepoUpstream(t, "production", mavenJarPath)

	proxy, _, store, _ := setupTestProxy(t)
	store.seekable = true
	proxy.HTTPClient = central.Client()
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(central.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })

	for _, upstream := range []struct {
		srv   *httptest.Server
		label string
	}{{staging, "staging"}, {production, "production"}} {
		h := NewMavenHandler(proxy, "http://localhost", central.URL, portal.URL).
			WithRepositories(map[string]string{"grails": upstream.srv.URL}).Routes()
		if w := getMaven(h, "/grails"+mavenJarPath); w.Code != http.StatusOK || w.Body.String() != upstream.label+" "+mavenJarPath {
			t.Errorf("%s: status = %d, body = %q", upstream.label, w.Code, w.Body)
		}
	}
}

// TestMavenDottedGroupDirectoryNotCachedAsGroup checks that a path whose
// group directories contain dots is not cached under the group the dots
// spell, where it would share an entry with the standard layout's path.
func TestMavenDottedGroupDirectoryNotCachedAsGroup(t *testing.T) {
	const standard = "/org/foo/bar/1.0/bar-1.0.jar"
	const dotted = "/org.foo/bar/1.0/bar-1.0.jar"
	central, _ := mavenRepoUpstream(t, "central", standard, dotted)
	portal, _ := mavenRepoUpstream(t, "portal")
	grails, _ := mavenRepoUpstream(t, "grails")
	h := newMavenRepositoriesHandler(t, central, portal, grails)

	for _, path := range []string{standard, dotted, standard} {
		w := getMaven(h, path)
		if want := "central " + path; w.Code != http.StatusOK || w.Body.String() != want {
			t.Errorf("GET %s = %d %q, want 200 %q", path, w.Code, w.Body.String(), want)
		}
	}
}

// TestMavenUncachedPathFallsBackToPluginPortal checks that a path proxied
// without caching, such as one with a dotted group directory, still tries
// the Gradle Plugin Portal when the primary upstream lacks it.
func TestMavenUncachedPathFallsBackToPluginPortal(t *testing.T) {
	const dotted = "/org.foo/bar/1.0/bar-1.0.jar"
	central, _ := mavenRepoUpstream(t, "central")
	portal, _ := mavenRepoUpstream(t, "portal", dotted)
	grails, _ := mavenRepoUpstream(t, "grails")
	h := newMavenRepositoriesHandler(t, central, portal, grails)

	if w := getMaven(h, dotted); w.Code != http.StatusOK || w.Body.String() != "portal "+dotted {
		t.Errorf("GET %s = %d %q, want the Plugin Portal's copy", dotted, w.Code, w.Body.String())
	}
}

func TestMavenUncachedPathKeepsByteRanges(t *testing.T) {
	const dotted = "/org.foo/bar/1.0/bar-1.0.jar"
	ranged := func() *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.ServeContent(w, r, "bar.jar", time.Time{}, strings.NewReader("0123456789"))
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	central, grails := ranged(), ranged()
	portal, _ := mavenRepoUpstream(t, "portal")
	h := newMavenRepositoriesHandler(t, central, portal, grails)

	for _, p := range []string{dotted, "/grails" + dotted} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		req.Header.Set("Range", "bytes=2-4")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusPartialContent || w.Body.String() != "234" {
			t.Errorf("GET %s with Range = %d %q, want 206 %q", p, w.Code, w.Body.String(), "234")
		}
	}
}
