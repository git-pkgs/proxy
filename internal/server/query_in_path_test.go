package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/storage"
	"github.com/git-pkgs/registries/fetch"
	"github.com/go-chi/chi/v5"
)

// countingTransport records every upstream request and fails it, so a test
// can tell whether a handler reached out at all.
type countingTransport struct{ hits atomic.Int32 }

func (c *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.hits.Add(1)
	return nil, errors.New("upstream contacted")
}

// TestProtocolHandlersRejectQueryInPath sends a path with an encoded '?' or
// '#' to every mounted ecosystem. Without the guard, most of these fetch the
// file before the '?' and cache it under the name after it.
func TestProtocolHandlersRejectQueryInPath(t *testing.T) {
	db, err := database.Create(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("creating database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := storage.OpenBucket(context.Background(), "file://"+t.TempDir())
	if err != nil {
		t.Fatalf("opening storage: %v", err)
	}

	transport := &countingTransport{}
	client := &http.Client{Transport: transport}
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(client), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	proxy.HTTPClient = client

	cfg := config.Default()
	cfg.BaseURL = "http://proxy.example"
	cfg.Upstream.Generic = map[string]string{"github": "https://github.com"}
	s := &Server{cfg: cfg}
	r := chi.NewRouter()
	s.mountProtocolHandlers(r, proxy)

	for _, target := range []string{
		"/apk/alpine/v3.22/main/x86_64/busybox-1.37.0-r12.apk%3F/foo-9.9-r0.apk",
		"/apk/alpine/v3.22/main/x86_64/busybox-1.37.0-r12.apk%23/foo-9.9-r0.apk",
		"/rpm/Packages/n/nginx-1.24.0-1.fc39.x86_64.rpm%3F/foo-9.9-1.fc39.x86_64.rpm",
		"/debian/pool/main/c/curl/curl_8.5.0-2_amd64.deb%3F/foo_9.9-1_amd64.deb",
		"/generic/github/owner/repo/releases/download/v1/tool.tar.gz%3F/other/repo/releases/download/v2/tool.tar.gz",
		"/pypi/packages/ab/cd/ef/requests-2.31.0.tar.gz%3F/ab/cd/ef/foo-9.9.tar.gz",
		"/maven/com/google/guava/guava/32.1.3-jre/guava-32.1.3-jre.jar%3F/org/evil/foo/9.9/foo-9.9.jar",
		"/conda/conda-forge/linux-64/numpy-1.26.0-py312_0.conda%3F%2Ffoo-9.9-py_0.conda",
		"/cran/src/contrib/ggplot2_3.4.0.tar.gz%3F%2Ffoo_9.9.tar.gz",
		"/gem/gems/rails-7.0.0.gem%3F%2Ffoo-9.9.gem",
		"/hex/tarballs/phoenix-1.7.10.tar%3F%2Ffoo-9.9.tar",
		"/pub/packages/http/versions/0.13.0.tar.gz%3F1.2.0.tar.gz",
		"/go/golang.org/x/text/@v/v0.3.0.zip%3F/@v/v0.3.8.zip",
		"/nuget/v3-flatcontainer/newtonsoft.json/13.0.1/newtonsoft.json.13.0.1.nupkg%3F%2Ffoo.9.9.nupkg",
		"/conan/v2/files/zlib/1.3/_/_/rev/recipe/conanfile.py%3F%2Fx",
		"/composer/p2/monolog/monolog.json%3Fx",
		"/npm/lodash%3F/-/lodash-4.17.21.tgz",
		"/cargo/crates/serde%3F/1.0.0/download",
		"/julia/registry/23338594-aafe-5451-b93e-139f81909106/abc%3F",
		"/swift/apple/swift-nio/2.0.0%3F.zip",
		"/homebrew/formula%3F.json",
		"/helm/charts/nginx-1.0.0.tgz%3F",
		"/v2/library/alpine/manifests/latest%3F",
		"/gradle/cache/abc%3F",
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", target, w.Code)
		}
	}
	if n := transport.hits.Load(); n != 0 {
		t.Errorf("upstream was contacted %d times", n)
	}
}

// TestComposerFilesAllowHashInVersion keeps Composer archive downloads of git
// branches with '#' in their name working: the version is escaped in the dist
// URL and the archive is resolved from metadata, not from the request path.
func TestComposerFilesAllowHashInVersion(t *testing.T) {
	var reached atomic.Bool
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Store(true) })

	w := httptest.NewRecorder()
	rejectQueryInPath(next).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/composer/files/acme/lib/dev-fix%2F%2342/abc123.zip", nil))
	if w.Code == http.StatusBadRequest || !reached.Load() {
		t.Errorf("status = %d, reached = %v; want the request passed on", w.Code, reached.Load())
	}
}
