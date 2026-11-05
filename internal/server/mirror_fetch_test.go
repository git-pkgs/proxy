package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/mirror"
	"github.com/git-pkgs/proxy/internal/storage"
)

// newTarballUpstream serves body for path, compared case-insensitively since
// scoped npm names arrive with an escaped slash, and counts the requests.
func newTarballUpstream(t *testing.T, path, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.URL.EscapedPath(), path) && !strings.EqualFold(r.URL.Path, path) {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(upstream.Close)
	return upstream, &hits
}

// mirrorWithConfig mirrors purls with the proxy proxy mirror builds for cfg.
func mirrorWithConfig(t *testing.T, cfg *config.Config, purls ...string) (*mirror.Progress, *handler.Proxy) {
	t.Helper()
	return mirrorWithVersions(t, cfg, nil, purls...)
}

// mirrorWithVersions mirrors purls, listing versions and files with versions,
// or from the configured upstreams when it is nil.
func mirrorWithVersions(t *testing.T, cfg *config.Config, versions mirror.VersionLister, purls ...string) (*mirror.Progress, *handler.Proxy) {
	t.Helper()
	cfg.Upstream.AllowLoopback = true
	db, err := database.Create(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MigrateSchema(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := storage.OpenBucket(context.Background(), "file://"+t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy, _ := NewMirrorProxy(cfg, db, store, logger)
	proxy.CacheMetadata = true // as proxy mirror does
	m := mirror.New(proxy, db, store, logger, 1)
	m.Charts = handler.NewHelmHandler(proxy, cfg.BaseURL, cfg.Upstream.Helm)
	m.Versions = versions
	m.JuliaUpstream = cfg.Upstream.Julia
	m.APKRepositories = cfg.Upstream.APK
	if versions == nil {
		m.Versions = NewMirrorVersions(cfg.Upstream, proxy.HTTPClient)
	}
	progress, err := m.Run(context.Background(), &mirror.PURLSource{PURLs: purls, Versions: m.Versions})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	return progress, proxy
}

func TestMirrorFetchesFromConfiguredUpstream(t *testing.T) {
	upstream, hits := newTarballUpstream(t, "/left-pad/-/left-pad-1.3.0.tgz", "tarball")
	cfg := config.Default()
	cfg.Upstream.NPM = upstream.URL

	progress, _ := mirrorWithConfig(t, cfg, "pkg:npm/left-pad@1.3.0")

	if progress.Completed != 1 || hits.Load() != 1 {
		t.Errorf("completed = %d, upstream hits = %d, errors = %v; want the tarball from upstream.npm", progress.Completed, hits.Load(), progress.Errors)
	}
}

func TestMirrorFetchesRoutedNPMPackagesFromTheirRoute(t *testing.T) {
	const tarballPath = "/@acme/widget/-/widget-1.0.0.tgz"
	public, publicHits := newTarballUpstream(t, tarballPath, "public")
	private, privateHits := newTarballUpstream(t, tarballPath, "private")
	cfg := config.Default()
	cfg.Upstream.NPM = public.URL
	cfg.Upstream.NPMRoutes = map[string]string{"@acme/*": private.URL}

	progress, proxy := mirrorWithConfig(t, cfg, "pkg:npm/%40acme/widget@1.0.0")

	if progress.Completed != 1 || privateHits.Load() != 1 {
		t.Errorf("completed = %d, route hits = %d, errors = %v; want the tarball from the route", progress.Completed, privateHits.Load(), progress.Errors)
	}
	if publicHits.Load() != 0 {
		t.Error("a routed package was mirrored from upstream.npm")
	}

	// A client gets the mirrored tarball from the cache once the route is down.
	private.Close()
	npm := handler.NewNPMHandler(proxy, "http://proxy.test", cfg.Upstream.NPM).WithPackageRoutes(cfg.Upstream.NPMRoutes)
	rec := httptest.NewRecorder()
	npm.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tarballPath, nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "private" {
		t.Errorf("tarball from cache: status = %d, body = %q, want 200 and the mirrored tarball", rec.Code, rec.Body)
	}
}

func TestMirrorResolverBaseURLs(t *testing.T) {
	u := config.Default().Upstream
	u.NPM = "https://npm.example.com/"
	u.CargoDownload = "https://crates.example.com/crates"
	u.Gem = "https://gems.example.com"
	u.Go = "https://goproxy.example.com"
	u.Hex = "https://hex.example.com"
	u.Pub = "https://pub.example.com"
	u.Maven = "https://maven.example.com/maven2"
	u.NuGet = "https://nuget.example.com"

	resolver := newMirrorResolver(u)
	tests := []struct {
		ecosystem, name, version, want string
	}{
		{"npm", "left-pad", "1.3.0", "https://npm.example.com/left-pad/-/left-pad-1.3.0.tgz"},
		{"cargo", "serde", "1.0.0", "https://crates.example.com/crates/serde/serde-1.0.0.crate"},
		{"gem", "rake", "13.2.1", "https://gems.example.com/downloads/rake-13.2.1.gem"},
		{"golang", "golang.org/x/mod", "v0.20.0", "https://goproxy.example.com/golang.org/x/mod/@v/v0.20.0.zip"},
		{"hex", "jason", "1.4.4", "https://hex.example.com/tarballs/jason-1.4.4.tar"},
		{"pub", "http", "1.2.2", "https://pub.example.com/packages/http/versions/1.2.2.tar.gz"},
		{"maven", "org.example:lib", "1.0", "https://maven.example.com/maven2/org/example/lib/1.0/lib-1.0.jar"},
		{"nuget", "Newtonsoft.Json", "13.0.3", "https://nuget.example.com/v3-flatcontainer/newtonsoft.json/13.0.3/newtonsoft.json.13.0.3.nupkg"},
	}
	for _, tt := range tests {
		info, err := resolver.Resolve(context.Background(), tt.ecosystem, tt.name, tt.version)
		if err != nil {
			t.Errorf("%s: Resolve() error = %v", tt.ecosystem, err)
			continue
		}
		if info.URL != tt.want {
			t.Errorf("%s: URL = %q, want %q", tt.ecosystem, info.URL, tt.want)
		}
	}
}
