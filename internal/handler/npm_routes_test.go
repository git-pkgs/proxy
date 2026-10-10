package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/registries/fetch"
)

// npmUpstreamRecorder serves canned packuments and records which paths were
// requested.
type npmUpstreamRecorder struct {
	*httptest.Server
	mu       sync.Mutex
	requests []string
}

func newNPMUpstream(t *testing.T, packuments map[string]func(baseURL string) string, status int) *npmUpstreamRecorder {
	t.Helper()
	u := &npmUpstreamRecorder{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.requests = append(u.requests, r.URL.Path)
		u.mu.Unlock()
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		packument, ok := packuments[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, packument(u.URL))
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *npmUpstreamRecorder) requested() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.requests...)
}

func routedTestPackument(version, tarballBase string) func(string) string {
	return func(baseURL string) string {
		return `{"name":"@example/widgets","dist-tags":{"latest":"` + version + `"},"versions":{"` + version +
			`":{"version":"` + version + `","dist":{"tarball":"` + baseURL + tarballBase + `/@example/widgets/-/widgets-` + version + `.tgz"}}}}`
	}
}

const routedNPMPath = "/registry/@example/widgets"

func newRoutedNPMHandler(proxy *Proxy, public, private *npmUpstreamRecorder) *NPMHandler {
	return NewNPMHandler(proxy, "http://proxy.test", public.URL).
		WithPackageRoutes(map[string]string{"@example/*": private.URL + "/registry"})
}

func TestNPMRoutedMetadataComesFromRouteOnly(t *testing.T) {
	public := newNPMUpstream(t, map[string]func(string) string{"/@example/widgets": routedTestPackument("9.9.9", "")}, http.StatusOK)
	private := newNPMUpstream(t, map[string]func(string) string{routedNPMPath: routedTestPackument("1.0.0", "/registry")}, http.StatusOK)
	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = http.DefaultClient
	h := newRoutedNPMHandler(proxy, public, private)

	w := serveNPM(t, h, http.MethodGet, "/@example%2fwidgets", nil, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if got := public.requested(); len(got) != 0 {
		t.Errorf("default registry was queried for a routed package: %v", got)
	}
	if strings.Contains(w.Body.String(), "9.9.9") {
		t.Errorf("routed package served the default registry's metadata: %s", w.Body.String())
	}
	wantTarball := "http://proxy.test/npm/@example%2Fwidgets/-/widgets-1.0.0.tgz"
	if !strings.Contains(w.Body.String(), wantTarball) {
		t.Errorf("tarball not rewritten to %q: %s", wantTarball, w.Body.String())
	}
}

func TestNPMRoutedMetadataDoesNotFallBack(t *testing.T) {
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
			public := newNPMUpstream(t, map[string]func(string) string{"/@example/widgets": routedTestPackument("9.9.9", "")}, http.StatusOK)
			private := newNPMUpstream(t, map[string]func(string) string{}, tt.privateStatus)
			proxy, _, _, _ := setupTestProxy(t)
			proxy.HTTPClient = http.DefaultClient
			h := newRoutedNPMHandler(proxy, public, private)

			w := serveNPM(t, h, http.MethodGet, "/@example%2fwidgets", nil, nil)

			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tt.wantStatus)
			}
			if got := public.requested(); len(got) != 0 {
				t.Errorf("default registry was queried for a routed package: %v", got)
			}
		})
	}
}

func TestNPMRoutedMetadataIgnoresDefaultRegistryCache(t *testing.T) {
	public := newNPMUpstream(t, map[string]func(string) string{"/@example/widgets": routedTestPackument("9.9.9", "")}, http.StatusOK)
	private := newNPMUpstream(t, map[string]func(string) string{routedNPMPath: routedTestPackument("1.0.0", "/registry")}, http.StatusOK)
	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = http.DefaultClient
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour

	if w := serveNPM(t, NewNPMHandler(proxy, "http://proxy.test", public.URL), http.MethodGet, "/@example%2fwidgets", nil, nil); w.Code != http.StatusOK {
		t.Fatalf("priming the default registry cache: status %d", w.Code)
	}

	w := serveNPM(t, newRoutedNPMHandler(proxy, public, private), http.MethodGet, "/@example%2fwidgets", nil, nil)

	if strings.Contains(w.Body.String(), "9.9.9") {
		t.Errorf("routed package served metadata cached from the default registry: %s", w.Body.String())
	}
}

func TestNPMUnroutedMetadataUsesDefaultRegistry(t *testing.T) {
	public := newNPMUpstream(t, map[string]func(string) string{"/lodash": routedTestPackument("4.17.21", "")}, http.StatusOK)
	private := newNPMUpstream(t, map[string]func(string) string{}, http.StatusOK)
	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = http.DefaultClient
	h := newRoutedNPMHandler(proxy, public, private)

	w := serveNPM(t, h, http.MethodGet, "/lodash", nil, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if got := private.requested(); len(got) != 0 {
		t.Errorf("route was queried for an unrouted package: %v", got)
	}
}

func TestNPMRoutedDownloadResolvesFromRoute(t *testing.T) {
	public := newNPMUpstream(t, map[string]func(string) string{"/@example/widgets": routedTestPackument("1.0.0", "")}, http.StatusOK)
	private := newNPMUpstream(t, map[string]func(string) string{routedNPMPath: routedTestPackument("1.0.0", "/registry")}, http.StatusOK)
	proxy, _, _, artifactFetcher := setupTestProxy(t)
	proxy.HTTPClient = http.DefaultClient
	artifactFetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("package")),
		ContentType: "application/gzip",
	}
	h := newRoutedNPMHandler(proxy, public, private)

	w := serveNPM(t, h, http.MethodGet, "/@example/widgets/-/widgets-1.0.0.tgz", nil, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if want := private.URL + "/registry/@example/widgets/-/widgets-1.0.0.tgz"; artifactFetcher.fetchedURL != want {
		t.Errorf("fetched URL = %q, want %q", artifactFetcher.fetchedURL, want)
	}
	if got := public.requested(); len(got) != 0 {
		t.Errorf("default registry was queried for a routed download: %v", got)
	}
}

func TestNPMRoutedDownloadWithoutMetadataStaysOnRoute(t *testing.T) {
	public := newNPMUpstream(t, map[string]func(string) string{}, http.StatusOK)
	private := newNPMUpstream(t, map[string]func(string) string{}, http.StatusInternalServerError)
	proxy, _, _, artifactFetcher := setupTestProxy(t)
	proxy.HTTPClient = http.DefaultClient
	artifactFetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("package")),
		ContentType: "application/gzip",
	}
	h := newRoutedNPMHandler(proxy, public, private)

	serveNPM(t, h, http.MethodGet, "/@example/widgets/-/widgets-1.0.0.tgz", nil, nil)

	if !strings.HasPrefix(artifactFetcher.fetchedURL, private.URL+"/registry/") {
		t.Errorf("fetched URL = %q, want one on the route %q", artifactFetcher.fetchedURL, private.URL+"/registry/")
	}
}

func TestNPMRoutedDownloadIgnoresDefaultRegistryArtifact(t *testing.T) {
	public := newNPMUpstream(t, map[string]func(string) string{"/@example/widgets": routedTestPackument("1.0.0", "")}, http.StatusOK)
	private := newNPMUpstream(t, map[string]func(string) string{routedNPMPath: routedTestPackument("1.0.0", "/registry")}, http.StatusOK)
	proxy, _, _, artifactFetcher := setupTestProxy(t)
	proxy.HTTPClient = http.DefaultClient
	artifactFetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("public package")),
		ContentType: "application/gzip",
	}

	if w := serveNPM(t, NewNPMHandler(proxy, "http://proxy.test", public.URL), http.MethodGet, "/@example/widgets/-/widgets-1.0.0.tgz", nil, nil); w.Code != http.StatusOK {
		t.Fatalf("priming the artifact cache: status %d; body: %s", w.Code, w.Body.String())
	}

	artifactFetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("private package")),
		ContentType: "application/gzip",
	}
	w := serveNPM(t, newRoutedNPMHandler(proxy, public, private), http.MethodGet, "/@example/widgets/-/widgets-1.0.0.tgz", nil, nil)

	if w.Body.String() != "private package" {
		t.Errorf("routed download served %q, want the routed registry's package", w.Body.String())
	}
}

// timedTestPackument is routedTestPackument with a publish time for the
// version, published age ago.
func timedTestPackument(version, tarballBase string, age time.Duration) func(string) string {
	published := time.Now().Add(-age).UTC().Format(time.RFC3339)
	return func(baseURL string) string {
		return `{"name":"@example/widgets","dist-tags":{"latest":"` + version + `"},"versions":{"` + version +
			`":{"version":"` + version + `","dist":{"tarball":"` + baseURL + tarballBase + `/@example/widgets/-/widgets-` + version + `.tgz"}}},` +
			`"time":{"` + version + `":"` + published + `"}}`
	}
}

// TestNPMRoutedCooldownUsesRoutePublishTimes is a regression test: the
// download cooldown check stored publish times under the package's plain
// version PURL, so a time read from one registry was trusted for the same
// version from another. A route's version published an hour ago must stay in
// a seven-day cooldown even when the default registry's version of the same
// name is a month old, whichever was downloaded first, and changing a route's
// upstream must not carry over the old one's times.
func TestNPMRoutedCooldownUsesRoutePublishTimes(t *testing.T) {
	const tarball = "/@example/widgets/-/widgets-1.0.0.tgz"
	month, hour := 30*24*time.Hour, time.Hour

	type step struct {
		handler    string // "public", "route" or "other route"
		wantStatus int
	}
	tests := []struct {
		name  string
		steps []step
	}{
		{"route added after a public download", []step{
			{"public", http.StatusOK},
			{"route", http.StatusNotFound},
		}},
		{"public download after a routed one", []step{
			{"route", http.StatusNotFound},
			{"public", http.StatusOK},
		}},
		{"route moved to another upstream", []step{
			{"other route", http.StatusOK},
			{"route", http.StatusNotFound},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			public := newNPMUpstream(t, map[string]func(string) string{"/@example/widgets": timedTestPackument("1.0.0", "", month)}, http.StatusOK)
			private := newNPMUpstream(t, map[string]func(string) string{routedNPMPath: timedTestPackument("1.0.0", "/registry", hour)}, http.StatusOK)
			old := newNPMUpstream(t, map[string]func(string) string{routedNPMPath: timedTestPackument("1.0.0", "/registry", month)}, http.StatusOK)
			proxy, _, _, artifactFetcher := setupTestProxy(t)
			proxy.HTTPClient = http.DefaultClient
			proxy.CacheMetadata = true
			proxy.MetadataTTL = time.Hour
			proxy.Cooldown = &cooldown.Config{Default: "7d"}
			handlers := map[string]*NPMHandler{
				"public":      NewNPMHandler(proxy, "http://proxy.test", public.URL),
				"route":       newRoutedNPMHandler(proxy, public, private),
				"other route": newRoutedNPMHandler(proxy, public, old),
			}

			for _, s := range tt.steps {
				artifactFetcher.artifact = &fetch.Artifact{
					Body:        io.NopCloser(strings.NewReader("package")),
					ContentType: "application/gzip",
				}
				w := serveNPM(t, handlers[s.handler], http.MethodGet, tarball, nil, nil)
				if w.Code != s.wantStatus {
					t.Errorf("%s download: status %d, want %d; body: %s", s.handler, w.Code, s.wantStatus, w.Body.String())
				}
			}
		})
	}
}
