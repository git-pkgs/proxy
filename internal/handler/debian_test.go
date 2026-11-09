package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/purl"
	"github.com/git-pkgs/registries/fetch"
)

func TestDebianHandler_parsePoolPath(t *testing.T) {
	h := &DebianHandler{}

	assertPathParser(t, "parsePoolPath", h.parsePoolPath, []pathParseCase{
		{"pool/main/n/nginx/nginx_1.18.0-6_amd64.deb", "nginx", "1.18.0-6", "amd64"},
		{"pool/main/libn/libncurses/libncurses6_6.2-1_amd64.deb", "libncurses6", "6.2-1", "amd64"},
		{"pool/contrib/v/virtualbox/virtualbox_6.1.38-1_amd64.deb", "virtualbox", "6.1.38-1", "amd64"},
		{"pool/main/g/git/git_2.39.2-1_arm64.deb", "git", "2.39.2-1", "arm64"},
		{
			"pool/universe/n/nmap/nmap_7.91+dfsg1+really7.80+dfsg1-2ubuntu0.1_amd64.deb",
			"nmap", "7.91+dfsg1+really7.80+dfsg1-2ubuntu0.1", "amd64",
		},
		{"pool/main/o/openssl/openssl_3.0.2-0ubuntu1.15~build1_amd64.deb", "openssl", "3.0.2-0ubuntu1.15~build1", "amd64"},
		{"nginx_1.18.0-6_amd64.deb", "nginx", "1.18.0-6", "amd64"},
		{"invalid/path", "", "", ""},
		{"pool/main/n/nginx/nginx.deb", "", "", ""},
	})
}

func TestDebianHandler_Routes(t *testing.T) {
	h := NewDebianHandler(nil, "http://localhost:8080", "", nil)
	assertRoutesBasics(t, h.Routes(), "/dists/stable/Release", "/pool/../../../etc/passwd")
}

// TestDebianHandler_LegacyCacheKeysUnchanged pins the artifact cache identity
// the main archive used before named repositories existed. Packages are now
// keyed by path, but deployments carry warm caches across upgrades, so
// entries under this name are still read for the URL they were fetched from
// (TestDebianHandler_LegacyPackageEntryServedForItsPath). Metadata is cheap
// to refetch; its keys are covered by TestMetadataCacheKeysUnambiguous.
func TestDebianHandler_LegacyCacheKeysUnchanged(t *testing.T) {
	const (
		poolPath = "pool/main/h/hello/hello_2.10-3_amd64.deb"
		distPath = "dists/trixie/InRelease"
	)

	h := NewDebianHandler(nil, "http://proxy.example", "https://archive.test", map[string]string{
		"security": "https://security.test",
	})

	// Artifact cache: the bare filename, with no repository prefix.
	filename := poolPath[strings.LastIndex(poolPath, "/")+1:]
	if got := h.legacyArtifactCacheFilename("", filename); got != "hello_2.10-3_amd64.deb" {
		t.Errorf("legacy artifact cache filename = %q, want %q", got, "hello_2.10-3_amd64.deb")
	}

	// A named repository must not reuse either identity.
	if got := h.legacyArtifactCacheFilename("security", filename); got == "hello_2.10-3_amd64.deb" {
		t.Error("named repository reused the legacy artifact cache filename")
	}
	if h.metadataCacheKeyFor("security", "https://security.test", distPath) == h.metadataCacheKeyFor("", h.upstreamURL, distPath) {
		t.Error("named repository reused the main archive's metadata cache key")
	}
}

// TestDebianHandler_NamedRepositoryRouting checks that a named repository
// reaches its own archive while the main archive keeps serving /dists/ and
// /pool/ unchanged.
func TestDebianHandler_NamedRepositoryRouting(t *testing.T) {
	mainRelease := "main archive InRelease"
	mainArchive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/dists/trixie/InRelease" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprint(w, mainRelease)
	}))
	defer mainArchive.Close()

	securityRelease := "security archive InRelease"
	securityArchive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/dists/trixie-security/InRelease" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprint(w, securityRelease)
	}))
	defer securityArchive.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.HTTPClient = http.DefaultClient

	h := NewDebianHandler(proxy, "http://proxy.example", mainArchive.URL, map[string]string{
		"security": securityArchive.URL,
	})

	got := serveDebianRequest(h, "/security/dists/trixie-security/InRelease")
	if got.Code != http.StatusOK || got.Body.String() != securityRelease {
		t.Errorf("named repository: status = %d, body = %q, want 200 %q",
			got.Code, got.Body.String(), securityRelease)
	}

	got = serveDebianRequest(h, "/dists/trixie/InRelease")
	if got.Code != http.StatusOK || got.Body.String() != mainRelease {
		t.Errorf("main archive: status = %d, body = %q, want 200 %q",
			got.Code, got.Body.String(), mainRelease)
	}

	// An unconfigured name is not a repository, so it addresses the main
	// archive as a plain path.
	got = serveDebianRequest(h, "/unknown/dists/trixie/InRelease")
	if got.Code == http.StatusOK {
		t.Errorf("unknown repository: status = 200, want the main archive's 404")
	}
}

// TestDebianHandler_UnknownRepositoryReportsConfiguredNames covers a misspelled
// repository name. {name}/dists/ is never a main-archive path, so the handler
// answers directly rather than forwarding upstream, where the reply would be an
// opaque HTML 404 that does not mention the repository at all.
func TestDebianHandler_UnknownRepositoryReportsConfiguredNames(t *testing.T) {
	var upstreamHits int
	mainArchive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits++
		http.Error(w, "<html>404 Not Found</html>", http.StatusNotFound)
	}))
	defer mainArchive.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = http.DefaultClient

	h := NewDebianHandler(proxy, "http://proxy.example", mainArchive.URL, map[string]string{
		"security": "http://security.example",
		"ghcli":    "http://ghcli.example",
	})

	got := serveDebianRequest(h, "/secuirty/dists/trixie-security/InRelease")
	if got.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", got.Code, http.StatusNotFound)
	}
	body := got.Body.String()
	if !strings.Contains(body, `"secuirty"`) {
		t.Errorf("body = %q, want it to name the misspelled repository", body)
	}
	// Sorted, so the message is stable across map iteration order.
	if !strings.Contains(body, "ghcli, security") {
		t.Errorf("body = %q, want it to list the configured repositories", body)
	}
	if upstreamHits != 0 {
		t.Errorf("upstream hits = %d, want 0: the handler should answer without forwarding", upstreamHits)
	}

	// A root-level main-archive path still falls through, since the main
	// archive really does serve files of its own there.
	got = serveDebianRequest(h, "/project/trace/README")
	if got.Code != http.StatusNotFound || upstreamHits != 1 {
		t.Errorf("main-archive path: status = %d, upstream hits = %d, want 404 and 1",
			got.Code, upstreamHits)
	}
}

// TestDebianHandler_MetadataCacheKeysDoNotCollideAcrossRepositories guards the
// hashed metadata cache key. The main archive's key replaces '/' with '_', so
// a repository named "security" serving dists/trixie/InRelease would otherwise
// collide with the main archive's own security_dists_trixie_InRelease entry,
// serving one archive's signed metadata to clients of the other.
func TestDebianHandler_MetadataCacheKeysDoNotCollideAcrossRepositories(t *testing.T) {
	mainRelease := "main archive InRelease"
	mainArchive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, mainRelease)
	}))
	defer mainArchive.Close()

	securityRelease := "security archive InRelease"
	securityArchive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, securityRelease)
	}))
	defer securityArchive.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.HTTPClient = http.DefaultClient

	h := NewDebianHandler(proxy, "http://proxy.example", mainArchive.URL, map[string]string{
		"security": securityArchive.URL,
	})

	// Main archive path whose separator-based key is "security_dists_...".
	first := serveDebianRequest(h, "/dists/security/dists/trixie/InRelease")
	if first.Code != http.StatusOK || first.Body.String() != mainRelease {
		t.Fatalf("main archive: status = %d, body = %q, want 200 %q",
			first.Code, first.Body.String(), mainRelease)
	}

	// Served within the metadata TTL: a colliding key would return
	// mainRelease here.
	second := serveDebianRequest(h, "/security/dists/trixie/InRelease")
	if second.Code != http.StatusOK {
		t.Fatalf("named repository: status = %d, want 200: %s", second.Code, second.Body.String())
	}
	if second.Body.String() != securityRelease {
		t.Errorf("named repository served %q, want %q (cache key collision)",
			second.Body.String(), securityRelease)
	}
}

// TestDebianHandler_PackageCacheScopedPerRepository checks that the same
// package filename in two archives does not resolve to one cached artifact.
func TestDebianHandler_PackageCacheScopedPerRepository(t *testing.T) {
	const pkgPath = "/pool/main/h/hello/hello_2.10-3_amd64.deb"

	mainPkg := "main archive package bytes"
	mainArchive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, mainPkg)
	}))
	defer mainArchive.Close()

	securityPkg := "security archive package bytes"
	securityArchive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, securityPkg)
	}))
	defer securityArchive.Close()

	proxy, _, _, _ := setupTestProxy(t)
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(mainArchive.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })

	h := NewDebianHandler(proxy, "http://proxy.example", mainArchive.URL, map[string]string{
		"security": securityArchive.URL,
	})

	first := serveDebianRequest(h, pkgPath)
	if first.Code != http.StatusOK || first.Body.String() != mainPkg {
		t.Fatalf("main archive: status = %d, body = %q, want 200 %q",
			first.Code, first.Body.String(), mainPkg)
	}

	second := serveDebianRequest(h, "/security"+pkgPath)
	if second.Code != http.StatusOK {
		t.Fatalf("named repository: status = %d, want 200: %s", second.Code, second.Body.String())
	}
	if second.Body.String() != securityPkg {
		t.Errorf("named repository served %q, want %q (artifact cache collision)",
			second.Body.String(), securityPkg)
	}
}

func TestDebianHandler_FlatArchivePackageIsCached(t *testing.T) {
	const pkgBody = "docker-ce package bytes"

	var upstreamHits int

	archive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		_, _ = fmt.Fprint(w, pkgBody)
	}))
	defer archive.Close()

	proxy, _, _, _ := setupTestProxy(t)
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(archive.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })

	h := NewDebianHandler(proxy, "http://proxy.example", "https://archive.test", map[string]string{
		"docker": archive.URL,
	})

	for _, path := range []string{
		"/docker/dists/trixie/pool/stable/amd64/docker-ce_5.0.1-1~debian.13~trixie_amd64.deb",
		"/docker/foo_1.0_amd64.deb",
	} {
		for attempt := 1; attempt <= 2; attempt++ {
			w := serveDebianRequest(h, path)
			if w.Code != http.StatusOK || w.Body.String() != pkgBody {
				t.Fatalf("%s request %d: status = %d, body = %q, want 200 %q",
					path, attempt, w.Code, w.Body.String(), pkgBody)
			}
		}
	}

	if upstreamHits != 2 {
		t.Errorf("upstream fetched %d times for four requests, want 2: the package is not cached", upstreamHits)
	}
}

func serveDebianRequest(h *DebianHandler, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	return w
}

func TestDebRetentionKey(t *testing.T) {
	tests := []struct {
		key  string
		want string
		ok   bool
	}{
		{"pkg:deb/curl", "pkg:deb/curl", true},
		{"pkg:deb/debian/curl", "pkg:deb/curl", true},
		{"pkg:deb/ubuntu/Curl", "pkg:deb/curl", true},
		{"pkg:deb/debian/libstdc++6", "pkg:deb/libstdc%2B%2B6", true},
		{"pkg:deb/debian/main/curl", "", false},
		{"pkg:deb/debian/pool%2Fcurl", "", false},
		{"pkg:apk/debian/curl", "", false},
	}
	for _, tt := range tests {
		p, err := purl.Parse(tt.key)
		if err != nil {
			t.Fatalf("Parse(%s): %v", tt.key, err)
		}
		got, ok := debRetentionKey(p)
		if ok != tt.ok || (ok && got != tt.want) {
			t.Errorf("debRetentionKey(%s) = %q, %v, want %q, %v", tt.key, got, ok, tt.want, tt.ok)
		}
	}
	if _, ok := debRetentionKey(nil); ok {
		t.Error("debRetentionKey(nil) accepted")
	}
}

// debianSuiteArchive serves a different build of one package file name per
// suite, as Docker's repository nests a pool under each suite.
func debianSuiteArchive(t *testing.T, available *atomic.Bool) *httptest.Server {
	t.Helper()
	archive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !available.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		suite := strings.Split(strings.TrimPrefix(r.URL.Path, "/dists/"), "/")[0]
		_, _ = fmt.Fprint(w, suite+" build")
	}))
	t.Cleanup(archive.Close)
	return archive
}

const (
	debianBullseyePackage = "/docker/dists/bullseye/pool/stable/amd64/containerd.io_1.7.25-1_amd64.deb"
	debianBookwormPackage = "/docker/dists/bookworm/pool/stable/amd64/containerd.io_1.7.25-1_amd64.deb"
)

// TestDebianHandler_PackageCacheScopedPerPath checks that one file name in
// two directories of a repository does not resolve to one cached artifact.
func TestDebianHandler_PackageCacheScopedPerPath(t *testing.T) {
	var available atomic.Bool
	available.Store(true)
	archive := debianSuiteArchive(t, &available)
	proxy, _, _, _ := setupTestProxy(t)
	proxy.Fetcher = fetch.NewFetcher(fetch.WithHTTPClient(archive.Client()), fetch.WithMaxRetries(0))
	h := NewDebianHandler(proxy, "http://proxy.example", "https://archive.test", map[string]string{"docker": archive.URL})

	for _, tc := range []struct{ path, want string }{
		{debianBullseyePackage, "bullseye build"},
		{debianBookwormPackage, "bookworm build"},
	} {
		if w := serveDebianRequest(h, tc.path); w.Code != http.StatusOK || w.Body.String() != tc.want {
			t.Errorf("GET %s = %d %q, want %q", tc.path, w.Code, w.Body.String(), tc.want)
		}
	}
}

// TestDebianHandler_LegacyPackageEntryServedForItsPath checks that a package
// cached under the file name alone, as before packages were keyed by path, is
// still served for the path it was fetched from, and only for that path.
func TestDebianHandler_LegacyPackageEntryServedForItsPath(t *testing.T) {
	var available atomic.Bool
	available.Store(true)
	archive := debianSuiteArchive(t, &available)
	proxy, _, _, _ := setupTestProxy(t)
	proxy.Fetcher = fetch.NewFetcher(fetch.WithHTTPClient(archive.Client()), fetch.WithMaxRetries(0))
	h := NewDebianHandler(proxy, "http://proxy.example", "https://archive.test", map[string]string{"docker": archive.URL})

	// The entry an earlier version cached for the bullseye path.
	legacy, err := proxy.GetOrFetchArtifactFromURL(context.Background(), "deb", "containerd.io", "1.7.25-1",
		"docker/containerd.io_1.7.25-1_amd64.deb", archive.URL+strings.TrimPrefix(debianBullseyePackage, "/docker"))
	if err != nil {
		t.Fatalf("seed legacy entry: %v", err)
	}
	_ = legacy.Reader.Close()

	if w := serveDebianRequest(h, debianBookwormPackage); w.Body.String() != "bookworm build" {
		t.Errorf("bookworm = %d %q, want its own build, not the legacy bullseye entry", w.Code, w.Body.String())
	}
	available.Store(false)
	if w := serveDebianRequest(h, debianBullseyePackage); w.Code != http.StatusOK || w.Body.String() != "bullseye build" {
		t.Errorf("bullseye with the archive down = %d %q, want the legacy entry", w.Code, w.Body.String())
	}
}
