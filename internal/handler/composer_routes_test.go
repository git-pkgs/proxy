package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/git-pkgs/registries/fetch"
)

// composerUpstreamRecorder serves canned Composer metadata and records which
// paths were requested.
type composerUpstreamRecorder struct {
	*httptest.Server
	mu       sync.Mutex
	requests []string
}

func newComposerUpstream(t *testing.T, documents map[string]string, status int) *composerUpstreamRecorder {
	t.Helper()
	u := &composerUpstreamRecorder{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.requests = append(u.requests, r.URL.Path)
		u.mu.Unlock()
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		body, ok := documents[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *composerUpstreamRecorder) requested() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.requests...)
}

const routedComposerMetadata = `{
	"packages": {
		"example/library": [
			{"name": "example/library", "version": "1.0.0", "dist": {"url": "https://private.example.com/archives/library-1.0.0.zip", "type": "zip"}}
		]
	}
}`

const publicComposerMetadata = `{
	"packages": {
		"example/library": [
			{"name": "example/library", "version": "9.9.9", "dist": {"url": "https://public.example.com/library-9.9.9.zip", "type": "zip"}}
		]
	}
}`

func newRoutedComposerHandler(t *testing.T, proxy *Proxy, public, private *composerUpstreamRecorder) *ComposerHandler {
	t.Helper()
	return NewComposerHandlerWithUpstreams(proxy, "http://proxy.test", public.URL, public.URL).
		WithPackageRoutes(map[string]string{"example/*": private.URL + "/group/packages/composer"})
}

func serveComposer(h *ComposerHandler, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	return w
}

func TestComposerRoutedMetadataComesFromRouteOnly(t *testing.T) {
	public := newComposerUpstream(t, map[string]string{"/p2/example/library.json": publicComposerMetadata}, http.StatusOK)
	private := newComposerUpstream(t, map[string]string{"/group/packages/composer/p2/example/library.json": routedComposerMetadata}, http.StatusOK)
	proxy, _, _, _ := setupTestProxy(t)
	h := newRoutedComposerHandler(t, proxy, public, private)

	w := serveComposer(h, "/p2/example/library.json")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if got := public.requested(); len(got) != 0 {
		t.Errorf("default repository was queried for a routed package: %v", got)
	}

	var metadata map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &metadata); err != nil {
		t.Fatalf("parsing response: %v", err)
	}
	versions := metadata["packages"].(map[string]any)["example/library"].([]any)
	version := versions[0].(map[string]any)
	if version["version"] != "1.0.0" {
		t.Errorf("version = %v, want the routed upstream's 1.0.0", version["version"])
	}
	wantDist := "http://proxy.test/composer/files/example/library/1.0.0/library-1.0.0.zip"
	if dist := version["dist"].(map[string]any)["url"]; dist != wantDist {
		t.Errorf("dist url = %v, want %q", dist, wantDist)
	}
	if notification, ok := version["notification-url"]; !ok || notification != "" {
		t.Errorf("notification-url = %v (present %t), want empty so Composer does not report routed installs to notify-batch", notification, ok)
	}
}

func TestComposerRoutedMetadataDoesNotFallBack(t *testing.T) {
	tests := []struct {
		name          string
		privateStatus int
		wantStatus    int
	}{
		{"package missing on route", http.StatusOK, http.StatusNotFound},
		{"route rejects credentials", http.StatusUnauthorized, http.StatusBadGateway},
		{"route unavailable", http.StatusInternalServerError, http.StatusBadGateway},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			public := newComposerUpstream(t, map[string]string{"/p2/example/library.json": publicComposerMetadata}, http.StatusOK)
			private := newComposerUpstream(t, map[string]string{}, tt.privateStatus)
			proxy, _, _, _ := setupTestProxy(t)
			h := newRoutedComposerHandler(t, proxy, public, private)

			w := serveComposer(h, "/p2/example/library.json")

			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tt.wantStatus)
			}
			if got := public.requested(); len(got) != 0 {
				t.Errorf("default repository was queried for a routed package: %v", got)
			}
		})
	}
}

func TestComposerRoutedMetadataIgnoresDefaultRepositoryCache(t *testing.T) {
	public := newComposerUpstream(t, map[string]string{"/p2/example/library.json": publicComposerMetadata}, http.StatusOK)
	private := newComposerUpstream(t, map[string]string{"/group/packages/composer/p2/example/library.json": routedComposerMetadata}, http.StatusOK)
	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour

	unrouted := NewComposerHandlerWithUpstreams(proxy, "http://proxy.test", public.URL, public.URL)
	if w := serveComposer(unrouted, "/p2/example/library.json"); w.Code != http.StatusOK {
		t.Fatalf("priming the default repository cache: status %d", w.Code)
	}

	w := serveComposer(newRoutedComposerHandler(t, proxy, public, private), "/p2/example/library.json")

	if strings.Contains(w.Body.String(), "9.9.9") {
		t.Errorf("routed package served metadata cached from the default repository: %s", w.Body.String())
	}
	if len(private.requested()) == 0 {
		t.Error("routed upstream was not queried")
	}
}

func TestComposerUnroutedMetadataUsesDefaultRepository(t *testing.T) {
	public := newComposerUpstream(t, map[string]string{"/p2/other/library.json": strings.ReplaceAll(publicComposerMetadata, "example/", "other/")}, http.StatusOK)
	private := newComposerUpstream(t, map[string]string{}, http.StatusOK)
	proxy, _, _, _ := setupTestProxy(t)
	h := newRoutedComposerHandler(t, proxy, public, private)

	w := serveComposer(h, "/p2/other/library.json")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if got := private.requested(); len(got) != 0 {
		t.Errorf("route was queried for an unrouted package: %v", got)
	}
	if strings.Contains(w.Body.String(), "notification-url") {
		t.Errorf("unrouted package metadata gained a notification-url: %s", w.Body.String())
	}
}

func TestComposerRoutedDownloadResolvesFromRoute(t *testing.T) {
	public := newComposerUpstream(t, map[string]string{"/p2/example/library.json": publicComposerMetadata}, http.StatusOK)
	private := newComposerUpstream(t, map[string]string{"/group/packages/composer/p2/example/library.json": routedComposerMetadata}, http.StatusOK)
	proxy, _, _, artifactFetcher := setupTestProxy(t)
	proxy.HTTPClient = http.DefaultClient
	artifactFetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("archive")),
		ContentType: "application/zip",
	}
	h := newRoutedComposerHandler(t, proxy, public, private)

	w := serveComposer(h, "/files/example/library/1.0.0/library-1.0.0.zip")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if want := "https://private.example.com/archives/library-1.0.0.zip"; artifactFetcher.fetchedURL != want {
		t.Errorf("fetched URL = %q, want %q", artifactFetcher.fetchedURL, want)
	}
	if got := public.requested(); len(got) != 0 {
		t.Errorf("default repository was queried for a routed download: %v", got)
	}
}

func TestComposerRoutedDownloadIgnoresDefaultRepositoryArtifact(t *testing.T) {
	public := newComposerUpstream(t, map[string]string{"/p2/example/library.json": strings.ReplaceAll(publicComposerMetadata, "9.9.9", "1.0.0")}, http.StatusOK)
	private := newComposerUpstream(t, map[string]string{"/group/packages/composer/p2/example/library.json": routedComposerMetadata}, http.StatusOK)
	proxy, _, _, artifactFetcher := setupTestProxy(t)
	proxy.HTTPClient = http.DefaultClient
	artifactFetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("public archive")),
		ContentType: "application/zip",
	}

	unrouted := NewComposerHandlerWithUpstreams(proxy, "http://proxy.test", public.URL, public.URL)
	if w := serveComposer(unrouted, "/files/example/library/1.0.0/library-1.0.0.zip"); w.Code != http.StatusOK {
		t.Fatalf("priming the artifact cache: status %d; body: %s", w.Code, w.Body.String())
	}

	artifactFetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("private archive")),
		ContentType: "application/zip",
	}
	w := serveComposer(newRoutedComposerHandler(t, proxy, public, private), "/files/example/library/1.0.0/library-1.0.0.zip")

	if w.Body.String() != "private archive" {
		t.Errorf("routed download served %q, want the routed upstream's archive", w.Body.String())
	}
}

// TestComposerRoutedMinifiedMetadataDisablesNotification checks that every
// version of a routed package, as Composer expands minified metadata, has an
// empty notification-url: versions that inherit their fields, versions after a
// ~dev reset, and versions after one that set its own URL and then unset it.
func TestComposerRoutedMinifiedMetadataDisablesNotification(t *testing.T) {
	minified := `{"minified":"composer/2.0","packages":{"example/library":[` +
		`{"name":"example/library","version":"1.0.0","dist":{"url":"https://private.example.com/a/1.0.0.zip","type":"zip"}},` +
		`{"version":"1.1.0","dist":{"url":"https://private.example.com/a/1.1.0.zip","type":"zip"}},` +
		`{"version":"1.2.0","notification-url":"https://private.example.com/notify","dist":{"url":"https://private.example.com/a/1.2.0.zip","type":"zip"}},` +
		`{"version":"1.3.0","notification-url":"__unset","dist":{"url":"https://private.example.com/a/1.3.0.zip","type":"zip"}},` +
		`"~dev",` +
		`{"name":"example/library","version":"dev-main","dist":{"url":"https://private.example.com/a/main.zip","type":"zip"}}]}}`
	public := newComposerUpstream(t, map[string]string{}, http.StatusOK)
	private := newComposerUpstream(t, map[string]string{"/group/packages/composer/p2/example/library.json": minified}, http.StatusOK)
	proxy, _, _, _ := setupTestProxy(t)
	h := newRoutedComposerHandler(t, proxy, public, private)

	w := serveComposer(h, "/p2/example/library.json")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", w.Code, w.Body.String())
	}
	want := map[string]string{
		"1.0.0":    "",
		"1.1.0":    "",
		"1.2.0":    "https://private.example.com/notify", // the route's own URL is kept
		"1.3.0":    "",
		"dev-main": "",
	}
	versions := composerVersions(t, w.Body.Bytes(), "example/library")
	if len(versions) != len(want) {
		t.Fatalf("versions = %v, want %d", versionNames(versions), len(want))
	}
	for _, v := range versions {
		name, _ := v["version"].(string)
		if got, ok := v["notification-url"]; !ok || got != want[name] {
			t.Errorf("%s: notification-url = %v (present %t), want %q", name, got, ok, want[name])
		}
	}
}
