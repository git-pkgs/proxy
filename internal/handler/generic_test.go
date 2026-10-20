package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	upstreamhttp "github.com/git-pkgs/proxy/internal/httpclient"
	"github.com/git-pkgs/proxy/internal/retention"
	"github.com/git-pkgs/registries/fetch"
)

const testReleaseAssetPath = "/jqlang/jq/releases/download/jq-1.7.1/jq-linux-amd64"

func TestGenericRetentionConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Retention.Ecosystems = map[string]string{"generic": "30d"}
	cfg.Storage.Retention.Packages = map[string]string{"pkg:generic/jqlang%2Fjq": "90d"}

	rules, err := cfg.RetentionRules(retention.Default)
	if err != nil {
		t.Fatalf("RetentionRules() error = %v", err)
	}
	if got := rules.For(genericEcosystem, "pkg:generic/jqlang%2Fjq"); got != 90*24*time.Hour {
		t.Errorf("jqlang/jq retention = %v, want 90d", got)
	}
	if got := rules.For(genericEcosystem, "pkg:generic/other%2Frepo"); got != 30*24*time.Hour {
		t.Errorf("generic ecosystem retention = %v, want 30d", got)
	}
}

func TestParseGitHubReleaseAsset(t *testing.T) {
	tests := []struct {
		path string
		want githubReleaseAsset
		ok   bool
	}{
		{
			"jqlang/jq/releases/download/jq-1.7.1/jq-linux-amd64",
			githubReleaseAsset{owner: "jqlang", repo: "jq", tag: "jq-1.7.1", filename: "jq-linux-amd64"},
			true,
		},
		{
			"cli/cli/releases/download/v2.63.2/gh_2.63.2_linux_amd64.tar.gz",
			githubReleaseAsset{owner: "cli", repo: "cli", tag: "v2.63.2", filename: "gh_2.63.2_linux_amd64.tar.gz"},
			true,
		},
		// Mutable: resolves to whatever is latest today.
		{"jqlang/jq/releases/latest/download/jq-linux-amd64", githubReleaseAsset{}, false},
		// API lookups and tag listings are not assets.
		{"repos/jqlang/jq/releases/tags/jq-1.7.1", githubReleaseAsset{}, false},
		{"jqlang/jq/releases/tag/jq-1.7.1", githubReleaseAsset{}, false},
		// Source archives are a different shape.
		{"jqlang/jq/archive/refs/tags/jq-1.7.1.tar.gz", githubReleaseAsset{}, false},
		// Extra or missing segments.
		{"jqlang/jq/releases/download/jq-1.7.1", githubReleaseAsset{}, false},
		{"jqlang/jq/releases/download/jq-1.7.1/dir/asset", githubReleaseAsset{}, false},
		{"", githubReleaseAsset{}, false},
	}

	for _, tt := range tests {
		got, ok := parseGitHubReleaseAsset(tt.path)
		if ok != tt.ok || got != tt.want {
			t.Errorf("parseGitHubReleaseAsset(%q) = (%+v, %v), want (%+v, %v)", tt.path, got, ok, tt.want, tt.ok)
		}
	}
}

const testMavenPattern = `dist/(?P<name>maven)/maven-3/(?P<version>[^/]+)/binaries/(?P<file>[^/]+)`

func TestMatchArtifactPattern(t *testing.T) {
	named, err := config.CompileGenericArtifactPattern(testMavenPattern)
	if err != nil {
		t.Fatal(err)
	}
	unnamed, err := config.CompileGenericArtifactPattern(`dist/v(?P<version>[^/]+)/(?P<file>.+)`)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		pattern *regexp.Regexp
		path    string
		want    genericArtifact
		ok      bool
	}{
		{
			"name group",
			named,
			"dist/maven/maven-3/3.9.9/binaries/apache-maven-3.9.9-bin.tar.gz",
			genericArtifact{name: "maven", version: "3.9.9", filename: "apache-maven-3.9.9-bin.tar.gz"},
			true,
		},
		{
			"name defaults to upstream",
			unnamed,
			"dist/v20.15.0/node-v20.15.0-linux-x64.tar.xz",
			genericArtifact{name: "apache", version: "20.15.0", filename: "node-v20.15.0-linux-x64.tar.xz"},
			true,
		},
		// Patterns match whole paths only.
		{"prefix only", named, "dist/maven/maven-3/3.9.9/binaries/apache-maven-3.9.9-bin.tar.gz/extra", genericArtifact{}, false},
		{"suffix only", named, "mirror/dist/maven/maven-3/3.9.9/binaries/apache-maven-3.9.9-bin.tar.gz", genericArtifact{}, false},
		{"directory listing", named, "dist/maven/maven-3/3.9.9/binaries/", genericArtifact{}, false},
		// A file group spanning segments would nest storage paths.
		{"file with slash", unnamed, "dist/v20.15.0/win-x64/node.exe", genericArtifact{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := matchArtifactPattern(tt.pattern, "apache", tt.path)
			if ok != tt.ok || got != tt.want {
				t.Errorf("matchArtifactPattern(%q) = (%+v, %v), want (%+v, %v)", tt.path, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestGenericHandler_BuiltinArtifactPatterns(t *testing.T) {
	h := NewGenericHandler(testProxy(), map[string]string{
		"apache": "https://archive.apache.org/",
		"node":   "https://nodejs.org",
		"golang": "https://dl.google.com/go",
		"mirror": "https://mirror.example.com",
	}, map[string][]string{
		"apache": {`extra/(?P<version>[^/]+)/(?P<file>[^/]+)`, testMavenPattern},
	})

	tests := []struct {
		repository string
		path       string
		want       genericArtifact
		ok         bool
	}{
		// The configured testMavenPattern overlaps the built-in and takes precedence.
		{"apache", "dist/maven/maven-3/3.9.9/binaries/apache-maven-3.9.9-bin.tar.gz",
			genericArtifact{name: "maven", version: "3.9.9", filename: "apache-maven-3.9.9-bin.tar.gz"}, true},
		{"apache", "dist/maven/maven-3/3.9.9/binaries/apache-maven-3.9.9-bin.tar.gz.sha512",
			genericArtifact{name: "maven", version: "3.9.9", filename: "apache-maven-3.9.9-bin.tar.gz.sha512"}, true},
		{"apache", "dist/maven/maven-4/4.0.0/binaries/apache-maven-4.0.0-bin.tar.gz",
			genericArtifact{name: "maven/maven-4", version: "4.0.0", filename: "apache-maven-4.0.0-bin.tar.gz"}, true},
		{"apache", "dist/maven/mvnd/1.0.6/maven-mvnd-1.0.6-linux-amd64.tar.gz",
			genericArtifact{name: "maven/mvnd", version: "1.0.6", filename: "maven-mvnd-1.0.6-linux-amd64.tar.gz"}, true},
		{"apache", "dist/maven/maven-3/", genericArtifact{}, false},
		{"apache", "dist/maven/maven-3/3.9.9/source/apache-maven-3.9.9-src.tar.gz", genericArtifact{}, false},
		{"apache", "extra/1.0/file.txt", genericArtifact{name: "apache", version: "1.0", filename: "file.txt"}, true},
		{"node", "dist/v22.11.0/node-v22.11.0-linux-x64.tar.gz",
			genericArtifact{name: "node", version: "v22.11.0", filename: "node-v22.11.0-linux-x64.tar.gz"}, true},
		{"node", "dist/v22.11.0/SHASUMS256.txt",
			genericArtifact{name: "node", version: "v22.11.0", filename: "SHASUMS256.txt"}, true},
		{"node", "dist/index.json", genericArtifact{}, false},
		{"golang", "go1.23.2.linux-amd64.tar.gz",
			genericArtifact{name: "golang", version: "1.23.2", filename: "go1.23.2.linux-amd64.tar.gz"}, true},
		{"golang", "go1.23.2.darwin-arm64.tar.gz.sha256",
			genericArtifact{name: "golang", version: "1.23.2", filename: "go1.23.2.darwin-arm64.tar.gz.sha256"}, true},
		{"golang", "go1.24rc1.windows-amd64.zip",
			genericArtifact{name: "golang", version: "1.24rc1", filename: "go1.24rc1.windows-amd64.zip"}, true},
		{"mirror", "dist/maven/maven-3/3.9.9/binaries/apache-maven-3.9.9-bin.tar.gz", genericArtifact{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.repository+"/"+tt.path, func(t *testing.T) {
			got, ok := h.parseArtifact(tt.repository, tt.path)
			if ok != tt.ok || got != tt.want {
				t.Errorf("parseArtifact(%q, %q) = (%+v, %v), want (%+v, %v)", tt.repository, tt.path, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestGenericHandler_ArtifactPatternIsCachedAndOtherPathsAreNot(t *testing.T) {
	const (
		artifactPath = "/dist/maven/maven-3/3.9.9/binaries/apache-maven-3.9.9-bin.tar.gz"
		listingPath  = "/dist/maven/maven-3/3.9.9/binaries/"
	)
	tarball := []byte("maven tarball bytes")
	var available atomic.Bool
	available.Store(true)
	var artifactRequests, listingRequests atomic.Int32

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !available.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case artifactPath:
			artifactRequests.Add(1)
			_, _ = w.Write(tarball)
		case listingPath:
			listingRequests.Add(1)
			_, _ = w.Write([]byte("<html>listing</html>"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	proxy, db, store, _ := setupTestProxy(t)
	store.seekable = true
	proxy.HTTPClient = upstream.Client()
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })

	h := NewGenericHandler(proxy,
		map[string]string{"apache": upstream.URL},
		map[string][]string{"apache": {testMavenPattern}})

	w := serveGenericRequest(h, "/apache"+artifactPath)
	if w.Code != http.StatusOK || w.Body.String() != string(tarball) {
		t.Fatalf("status = %d, body = %q, want 200 %q", w.Code, w.Body.String(), tarball)
	}

	pkg, err := db.GetPackageByEcosystemName(genericEcosystem, "maven")
	if err != nil || pkg == nil {
		t.Fatalf("package generic/maven = %v, %v; want a cached package", pkg, err)
	}
	versions, err := db.GetVersionsByPackagePURL(pkg.PURL)
	if err != nil || len(versions) != 1 || !strings.HasSuffix(versions[0].PURL, "@3.9.9") {
		t.Fatalf("versions = %+v, %v; want one @3.9.9", versions, err)
	}

	available.Store(false)
	w = serveGenericRequest(h, "/apache"+artifactPath)
	if w.Code != http.StatusOK || w.Body.String() != string(tarball) {
		t.Fatalf("cached: status = %d, body = %q, want 200 %q", w.Code, w.Body.String(), tarball)
	}
	if got := artifactRequests.Load(); got != 1 {
		t.Errorf("artifact upstream requests = %d, want 1", got)
	}

	// Paths the pattern does not match keep going through the metadata route.
	available.Store(true)
	for range 2 {
		serveGenericRequest(h, "/apache"+listingPath)
	}
	if got := listingRequests.Load(); got != 2 {
		t.Errorf("listing upstream requests = %d, want 2 with metadata caching off", got)
	}
}

func TestGenericHandler_ConfiguredArtifactsKeepDistinctPathsAndQueries(t *testing.T) {
	for _, tt := range []struct {
		name    string
		targets []string
	}{
		{"platform paths", []string{
			"/releases/tool/1.0.0/linux/tool.tar.gz",
			"/releases/tool/1.0.0/darwin/tool.tar.gz",
		}},
		{"query parameters", []string{
			"/releases/tool/1.0.0/linux/tool.tar.gz?token=example&variant=a%2Bb",
			"/releases/tool/1.0.0/linux/tool.tar.gz?token=other&variant=a+b",
			"/releases/tool/1.0.0/linux/tool.tar.gz",
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				// Echo the exact target so dropped or changed queries fail too.
				_, _ = w.Write([]byte(r.URL.RequestURI()))
			}))
			defer upstream.Close()

			proxy, _, store, _ := setupTestProxy(t)
			store.seekable = true
			fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
			proxy.Fetcher = fetcher
			t.Cleanup(func() { _ = fetcher.Close() })
			h := NewGenericHandler(proxy,
				map[string]string{"downloads": upstream.URL},
				map[string][]string{"downloads": {`releases/(?P<name>tool)/(?P<version>[^/]+)/(linux|darwin)/(?P<file>[^/]+)`}})

			for pass := range 2 {
				for _, target := range tt.targets {
					w := serveGenericRequest(h, "/downloads"+target)
					if w.Code != http.StatusOK || w.Body.String() != target {
						t.Fatalf("pass %d, target %q: status = %d, body = %q; want 200 %q",
							pass, target, w.Code, w.Body.String(), target)
					}
				}
				upstream.Close()
			}
			if got := requests.Load(); got != int32(len(tt.targets)) {
				t.Errorf("upstream requests = %d, want %d", got, len(tt.targets))
			}
		})
	}
}

func TestGenericHandler_RejectsUnknownUpstreamAndBadPaths(t *testing.T) {
	h := NewGenericHandler(testProxy(), map[string]string{"github": "https://github.com"})

	tests := []struct {
		name   string
		method string
		target string
		want   int
	}{
		{"unknown upstream", http.MethodGet, "/gitlab/owner/repo/releases/download/v1/asset", http.StatusNotFound},
		{"missing path", http.MethodGet, "/github", http.StatusNotFound},
		{"missing path with slash", http.MethodGet, "/github/", http.StatusNotFound},
		{"traversal", http.MethodGet, "/github/../etc/passwd", http.StatusBadRequest},
		{"encoded traversal", http.MethodGet, "/github/%2e%2e/etc/passwd", http.StatusBadRequest},
		{"post", http.MethodPost, "/github/owner/repo/releases/download/v1/asset", http.StatusMethodNotAllowed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.Routes().ServeHTTP(w, httptest.NewRequest(tt.method, tt.target, nil))
			if w.Code != tt.want {
				t.Errorf("status = %d, want %d", w.Code, tt.want)
			}
		})
	}
}

func TestGenericHandler_ReleaseAssetIsCachedAndServedWhenUpstreamDown(t *testing.T) {
	asset := []byte("jq binary bytes")
	var available atomic.Bool
	available.Store(true)
	var upstreamRequests atomic.Int32

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !available.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path != testReleaseAssetPath {
			http.NotFound(w, r)
			return
		}
		upstreamRequests.Add(1)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(asset)
	}))
	defer upstream.Close()

	proxy, db, store, _ := setupTestProxy(t)
	store.seekable = true
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })

	h := NewGenericHandler(proxy, map[string]string{"github": upstream.URL})

	w := serveGenericRequest(h, "/github"+testReleaseAssetPath)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != string(asset) {
		t.Errorf("body = %q, want %q", got, asset)
	}
	if got := upstreamRequests.Load(); got != 1 {
		t.Fatalf("upstream requests = %d, want 1", got)
	}
	pkg, err := db.GetPackageByEcosystemName(genericEcosystem, "jqlang/jq")
	if err != nil || pkg == nil {
		t.Fatalf("release package = %v, %v; want a cached package", pkg, err)
	}
	if pkg.PURL != "pkg:generic/jqlang%2Fjq" {
		t.Errorf("release package PURL = %q, want pkg:generic/jqlang%%2Fjq", pkg.PURL)
	}

	// Second request must be served from cache, even with the upstream down.
	available.Store(false)
	rangeRequest := httptest.NewRequest(http.MethodGet, "/github"+testReleaseAssetPath, nil)
	rangeRequest.Header.Set("Range", "bytes=0-2")
	rangeResponse := httptest.NewRecorder()
	h.Routes().ServeHTTP(rangeResponse, rangeRequest)
	if rangeResponse.Code != http.StatusPartialContent {
		t.Fatalf("range: status = %d, want 206: %s", rangeResponse.Code, rangeResponse.Body.String())
	}
	if got := rangeResponse.Body.String(); got != string(asset[:3]) {
		t.Errorf("range: body = %q, want %q", got, asset[:3])
	}
	if got := rangeResponse.Header().Get("Content-Range"); got != "bytes 0-2/15" {
		t.Errorf("range: Content-Range = %q, want %q", got, "bytes 0-2/15")
	}
	if got := upstreamRequests.Load(); got != 1 {
		t.Errorf("upstream requests after range cache hit = %d, want 1", got)
	}

	w = serveGenericRequest(h, "/github"+testReleaseAssetPath)
	if w.Code != http.StatusOK {
		t.Fatalf("cached: status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != string(asset) {
		t.Errorf("cached: body = %q, want %q", got, asset)
	}
	if got := upstreamRequests.Load(); got != 1 {
		t.Errorf("upstream requests after cache hit = %d, want 1", got)
	}

	// HEAD is answered from the same cache entry without a body.
	w = httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodHead, "/github"+testReleaseAssetPath, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("HEAD: status = %d, want 200", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD: body length = %d, want 0", w.Body.Len())
	}
}

func TestGenericHandler_ServesReleaseAssetCachedUnderUnhashedKey(t *testing.T) {
	asset := []byte("jq binary bytes")
	var available atomic.Bool
	available.Store(true)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !available.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(asset)
	}))
	defer upstream.Close()

	proxy, _, store, _ := setupTestProxy(t)
	store.seekable = true
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })

	// Cache the asset the way releases before hashed keys did.
	result, err := proxy.GetOrFetchArtifactFromURL(context.Background(), genericEcosystem,
		"jqlang/jq", "jq-1.7.1", "github/jq-linux-amd64", upstream.URL+testReleaseAssetPath)
	if err != nil {
		t.Fatalf("priming legacy entry: %v", err)
	}
	if result.Reader != nil {
		_ = result.Reader.Close()
	}
	available.Store(false)

	h := NewGenericHandler(proxy, map[string]string{"github": upstream.URL})
	w := serveGenericRequest(h, "/github"+testReleaseAssetPath)
	if w.Code != http.StatusOK || w.Body.String() != string(asset) {
		t.Fatalf("legacy entry: status = %d, body = %q; want 200 %q", w.Code, w.Body.String(), asset)
	}

	w = serveGenericRequest(h, "/github"+testReleaseAssetPath+"?x=1")
	if w.Code != http.StatusBadGateway {
		t.Errorf("with query: status = %d, want 502", w.Code)
	}

	repointed := NewGenericHandler(proxy, map[string]string{"github": upstream.URL + "/mirror"})
	w = serveGenericRequest(repointed, "/github"+testReleaseAssetPath)
	if w.Code != http.StatusBadGateway {
		t.Errorf("repointed upstream: status = %d, want 502", w.Code)
	}
}

func TestGenericHandler_ReleaseAssetNotFoundIsNotCached(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })

	h := NewGenericHandler(proxy, map[string]string{"github": upstream.URL})
	w := serveGenericRequest(h, "/github"+testReleaseAssetPath)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
}

func TestGenericHandler_MetadataForwardsQueryAndServesStaleOnThrottle(t *testing.T) {
	const apiPath = "/repos/jqlang/jq/releases/tags/jq-1.7.1"
	body := `{"tag_name":"jq-1.7.1"}`
	var throttled atomic.Bool
	var gotQuery string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != apiPath {
			http.NotFound(w, r)
			return
		}
		gotQuery = r.URL.RawQuery
		if throttled.Load() {
			w.Header().Set("Retry-After", "60")
			http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.github+json")
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte(body))
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	proxy.CacheMetadata = true
	// A tiny TTL so the second request is past freshness and has to consult
	// the upstream, and the served copy is marked stale.
	proxy.MetadataTTL = time.Millisecond

	h := NewGenericHandler(proxy, map[string]string{"github-api": upstream.URL})

	req := httptest.NewRequest(http.MethodGet, "/github-api"+apiPath+"?per_page=1", nil)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != body {
		t.Errorf("body = %q, want %q", got, body)
	}
	if gotQuery != "per_page=1" {
		t.Errorf("upstream query = %q, want %q", gotQuery, "per_page=1")
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/vnd.github+json" {
		t.Errorf("Content-Type = %q, want upstream's", ct)
	}

	// The upstream now throttles us: the cached body must be served stale
	// rather than the 429 being passed through.
	throttled.Store(true)
	time.Sleep(5 * time.Millisecond)
	w = httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("throttled: status = %d, want 200 stale: %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != body {
		t.Errorf("throttled: body = %q, want cached %q", got, body)
	}
	if warning := w.Header().Get("Warning"); !strings.Contains(warning, "110") {
		t.Errorf("throttled: Warning = %q, want a 110 stale warning", warning)
	}
}

func TestGenericHandler_DistinctUpstreamsDoNotShareCache(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("from first"))
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("from second"))
	}))
	defer second.Close()

	proxy, _, _, _ := setupTestProxy(t)
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(first.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })

	h := NewGenericHandler(proxy, map[string]string{"one": first.URL, "two": second.URL})

	w := serveGenericRequest(h, "/one"+testReleaseAssetPath)
	if got := w.Body.String(); got != "from first" {
		t.Fatalf("one: body = %q, want %q", got, "from first")
	}
	w = serveGenericRequest(h, "/two"+testReleaseAssetPath)
	if got := w.Body.String(); got != "from second" {
		t.Fatalf("two: body = %q, want %q (must not reuse the first upstream's cache entry)", got, "from second")
	}
}

func TestGenericHandler_UpstreamAuthIsScopedToTheConfiguredHost(t *testing.T) {
	asset := []byte("private asset")
	var storageAuth atomic.Value
	storageAuth.Store("unset")

	// The object store the release host redirects to must never see the
	// token configured for the release host.
	objectStore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		storageAuth.Store(r.Header.Get("Authorization"))
		_, _ = w.Write(asset)
	}))
	defer objectStore.Close()

	var releaseAuth string
	releaseHost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		releaseAuth = r.Header.Get("Authorization")
		if releaseAuth != "Bearer github-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, objectStore.URL+"/signed"+r.URL.Path, http.StatusFound)
	}))
	defer releaseHost.Close()

	proxy, _, _, _ := setupTestProxy(t)
	authClient := &http.Client{Transport: upstreamhttp.NewTransport(http.DefaultTransport,
		upstreamhttp.AuthFunc(func(url string) (string, string) {
			if strings.HasPrefix(url, releaseHost.URL) {
				return "Authorization", "Bearer github-token"
			}
			return "", ""
		}))}
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(authClient), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })

	h := NewGenericHandler(proxy, map[string]string{"github": releaseHost.URL})
	w := serveGenericRequest(h, "/github"+testReleaseAssetPath)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != string(asset) {
		t.Errorf("body = %q, want %q", got, asset)
	}
	if releaseAuth != "Bearer github-token" {
		t.Errorf("release host Authorization = %q, want the configured token", releaseAuth)
	}
	if got := storageAuth.Load(); got != "" {
		t.Errorf("object store Authorization = %q, want none after the cross-host redirect", got)
	}
}

func serveGenericRequest(h *GenericHandler, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	return w
}
