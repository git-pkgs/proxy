package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
		{"invalid/path", "", "", ""},
		{"pool/main/n/nginx/nginx.deb", "", "", ""},
	})
}

func TestDebianHandler_Routes(t *testing.T) {
	h := NewDebianHandler(nil, "http://localhost:8080", "", nil)
	assertRoutesBasics(t, h.Routes(), "/dists/stable/Release", "/pool/../../../etc/passwd")
}

// TestDebianHandler_LegacyCacheKeysUnchanged pins the cache identities the
// main archive used before named repositories existed. Deployments carry warm
// caches across upgrades, so a changed key here silently discards them.
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
	if got := h.artifactCacheFilename("", filename); got != "hello_2.10-3_amd64.deb" {
		t.Errorf("legacy artifact cache filename = %q, want %q", got, "hello_2.10-3_amd64.deb")
	}

	// Metadata cache: path separators replaced with underscores.
	if got := h.metadataCacheKeyFor("", h.upstreamURL, distPath); got != "dists_trixie_InRelease" {
		t.Errorf("legacy metadata cache key = %q, want %q", got, "dists_trixie_InRelease")
	}

	// A named repository must not reuse either identity.
	if got := h.artifactCacheFilename("security", filename); got == "hello_2.10-3_amd64.deb" {
		t.Error("named repository reused the legacy artifact cache filename")
	}
	if got := h.metadataCacheKeyFor("security", "https://security.test", distPath); got == "dists_trixie_InRelease" {
		t.Error("named repository reused the legacy metadata cache key")
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

func serveDebianRequest(h *DebianHandler, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	return w
}
