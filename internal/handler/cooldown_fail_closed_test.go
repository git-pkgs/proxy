package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	registryclient "github.com/git-pkgs/registries/client"
	"github.com/git-pkgs/registries/fetch"
)

// holdPaths returns a client that answers requests for the given paths with
// the rate limit gate's HeldError and sends the rest to upstream.
func holdPaths(paths ...string) *http.Client {
	held := &registryclient.HeldError{Host: "upstream.example", Until: time.Now().Add(30 * time.Second), RetryAfter: 30 * time.Second}
	return &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		for _, p := range paths {
			if r.URL.Path == p {
				return nil, held
			}
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
}

func serveGet(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

func wantStatus(t *testing.T, w *httptest.ResponseRecorder, status int, leaked string) {
	t.Helper()
	if w.Code != status {
		t.Errorf("status = %d, want %d: %s", w.Code, status, w.Body.String())
	}
	if status == http.StatusServiceUnavailable && w.Header().Get("Retry-After") == "" {
		t.Error("503 without Retry-After")
	}
	if leaked != "" && strings.Contains(w.Body.String(), leaked) {
		t.Errorf("response lists %q, which cooldown should have withheld", leaked)
	}
}

func TestGemCooldownFailsClosed(t *testing.T) {
	index := "---\n1.0.0 |checksum:abc\n2.0.0 |checksum:def\n"
	versionsStatus := http.StatusInternalServerError
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/versions/") {
			w.WriteHeader(versionsStatus)
			return
		}
		_, _ = io.WriteString(w, index)
	}))
	defer upstream.Close()

	t.Run("versions API fails", func(t *testing.T) {
		proxy, _, _, _ := setupTestProxy(t)
		proxy.Cooldown = &cooldown.Config{Default: "3d"}
		h := NewGemHandlerWithUpstream(proxy, "http://proxy.test", upstream.URL)
		wantStatus(t, serveGet(t, h.Routes(), "/info/testgem"), http.StatusBadGateway, "2.0.0")
	})

	t.Run("versions API held by rate limit", func(t *testing.T) {
		proxy, _, _, _ := setupTestProxy(t)
		proxy.Cooldown = &cooldown.Config{Default: "3d"}
		proxy.HTTPClient = holdPaths("/api/v1/versions/testgem.json")
		h := NewGemHandlerWithUpstream(proxy, "http://proxy.test", upstream.URL)
		wantStatus(t, serveGet(t, h.Routes(), "/info/testgem"), http.StatusServiceUnavailable, "2.0.0")
	})

	t.Run("exempt package needs no times", func(t *testing.T) {
		proxy, _, _, _ := setupTestProxy(t)
		proxy.Cooldown = &cooldown.Config{Default: "3d", Packages: map[string]string{"pkg:gem/testgem": "0"}}
		h := NewGemHandlerWithUpstream(proxy, "http://proxy.test", upstream.URL)
		w := serveGet(t, h.Routes(), "/info/testgem")
		if w.Code != http.StatusOK || w.Body.String() != index {
			t.Errorf("status = %d, body = %q; want the index unchanged", w.Code, w.Body.String())
		}
	})
}

func TestHexCooldownFailsClosed(t *testing.T) {
	now := time.Now()
	proto := buildHexSigned(buildHexPackage("testpkg", []string{testVersion100, "2.0.0"}))
	apiJSON, _ := json.Marshal(hexPackageAPI{Releases: []hexRelease{
		{Version: testVersion100, InsertedAt: now.Add(-7 * 24 * time.Hour).Format(time.RFC3339Nano)},
		{Version: "2.0.0", InsertedAt: now.Add(-time.Hour).Format(time.RFC3339Nano)},
	}})

	newUpstream := func(t *testing.T, packageBody []byte, apiStatus int) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/packages/testpkg":
				_, _ = w.Write(packageBody)
			case "/api/packages/testpkg":
				w.WriteHeader(apiStatus)
				_, _ = w.Write(apiJSON)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	handler := func(t *testing.T, srv *httptest.Server, client *http.Client) http.Handler {
		proxy, _, _, _ := setupTestProxy(t)
		proxy.Cooldown = &cooldown.Config{Default: "3d"}
		if client != nil {
			proxy.HTTPClient = client
		}
		return NewHexHandlerWithUpstreams(proxy, "http://proxy.test", srv.URL, srv.URL).Routes()
	}

	t.Run("API fails", func(t *testing.T) {
		srv := newUpstream(t, proto, http.StatusInternalServerError)
		wantStatus(t, serveGet(t, handler(t, srv, nil), "/packages/testpkg"), http.StatusBadGateway, "")
	})

	t.Run("API held by rate limit", func(t *testing.T) {
		srv := newUpstream(t, proto, http.StatusOK)
		w := serveGet(t, handler(t, srv, holdPaths("/api/packages/testpkg")), "/packages/testpkg")
		wantStatus(t, w, http.StatusServiceUnavailable, "")
	})

	t.Run("package can't be filtered", func(t *testing.T) {
		srv := newUpstream(t, []byte("not gzip 2.0.0"), http.StatusOK)
		wantStatus(t, serveGet(t, handler(t, srv, nil), "/packages/testpkg"), http.StatusBadGateway, "2.0.0")
	})

	t.Run("filtered", func(t *testing.T) {
		srv := newUpstream(t, proto, http.StatusOK)
		w := serveGet(t, handler(t, srv, nil), "/packages/testpkg")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
	})
}

func TestPyPICooldownFailsClosed(t *testing.T) {
	simple := `{"meta":{"api-version":"1.4"},"name":"ruff","files":[` +
		`{"filename":"ruff-1.0.0.tar.gz","url":"https://files.pythonhosted.org/packages/ab/ruff-1.0.0.tar.gz"},` +
		`{"filename":"ruff-2.0.0.tar.gz","url":"https://files.pythonhosted.org/packages/cd/ruff-2.0.0.tar.gz"}]}`

	tests := []struct {
		name   string
		json   func(*http.Request) (*http.Response, error)
		status int
	}{
		{"JSON API fails", func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusInternalServerError, Body: http.NoBody, Request: r}, nil
		}, http.StatusBadGateway},
		{"JSON API held by rate limit", func(*http.Request) (*http.Response, error) {
			return nil, &registryclient.HeldError{Host: "pypi.test", RetryAfter: 30 * time.Second}
		}, http.StatusServiceUnavailable},
		{"JSON has no releases", func(r *http.Request) (*http.Response, error) {
			return pypiHTTPResponse(r, "application/json", `{"info":{}}`), nil
		}, http.StatusBadGateway},
		{"JSON API doesn't know the package", func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody, Request: r}, nil
		}, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, proxy := setupPyPIHandler(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/pypi/ruff/json" {
					return tt.json(r)
				}
				return pypiHTTPResponse(r, pypiSimpleJSON, simple), nil
			})
			proxy.Cooldown = &cooldown.Config{Default: "7d"}

			req := httptest.NewRequest(http.MethodGet, "/simple/ruff/", nil)
			req.Header.Set("Accept", uvPyPIAccept)
			w := httptest.NewRecorder()
			h.Routes().ServeHTTP(w, req)
			leaked := "ruff-2.0.0"
			if tt.status == http.StatusOK {
				leaked = ""
			}
			wantStatus(t, w, tt.status, leaked)
		})
	}
}

func TestPyPIDownloadCooldownFailsClosed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	proxy, _, _, fetcher := setupTestProxy(t)
	proxy.Cooldown = &cooldown.Config{Default: "7d"}
	fetcher.artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader("sdist")), ContentType: "application/octet-stream"}
	h := &PyPIHandler{proxy: proxy, upstreamURL: upstream.URL, proxyURL: "http://localhost"}

	w := serveGet(t, h.Routes(), "/packages/packages/ab/cd/ef0123456789/newpkg-2.0.0.tar.gz")
	wantStatus(t, w, http.StatusBadGateway, "")
	if fetcher.fetchCalled {
		t.Error("fetched a file whose publish time couldn't be checked")
	}
}

func TestNPMDownloadCooldownFailsClosed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	proxy, _, _, fetcher := setupTestProxy(t)
	proxy.Cooldown = &cooldown.Config{Default: "7d"}
	fetcher.artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader("tarball")), ContentType: "application/octet-stream"}
	h := NewNPMHandler(proxy, "http://proxy.test", upstream.URL)

	w := serveGet(t, h.Routes(), "/leftpad/-/leftpad-2.0.0.tgz")
	wantStatus(t, w, http.StatusBadGateway, "")
	if fetcher.fetchCalled {
		t.Error("fetched a tarball whose publish time couldn't be checked")
	}
}

// TestCooldownRewriteFailureFailsClosed checks that a metadata document the
// cooldown rewrite can't parse is refused rather than passed on, and still
// passed on when cooldown doesn't apply.
func TestCooldownRewriteFailureFailsClosed(t *testing.T) {
	const broken = `{"name": "pkg", "versions": [ 2.0.0`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(headerContentType, contentTypeJSON)
		_, _ = io.WriteString(w, broken)
	}))
	defer upstream.Close()

	tests := []struct {
		name    string
		purl    string
		handler func(*Proxy) http.Handler
		path    string
	}{
		{"npm", "pkg:npm/pkg", func(p *Proxy) http.Handler { return NewNPMHandler(p, "http://proxy.test", upstream.URL).Routes() }, "/pkg"},
		{"composer", "pkg:composer/vendor/pkg", func(p *Proxy) http.Handler {
			return NewComposerHandlerWithUpstreams(p, "http://proxy.test", upstream.URL, upstream.URL).Routes()
		}, "/p2/vendor/pkg.json"},
		{"pub", "pkg:pub/pkg", func(p *Proxy) http.Handler {
			return NewPubHandlerWithUpstream(p, "http://proxy.test", upstream.URL).Routes()
		}, "/api/packages/pkg"},
		{"pypi json", "pkg:pypi/pkg", func(p *Proxy) http.Handler {
			return NewPyPIHandlerWithUpstreams(p, "http://proxy.test", upstream.URL, "").Routes()
		}, "/pypi/pkg/json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxy, _, _, _ := setupTestProxy(t)
			proxy.Cooldown = &cooldown.Config{Default: "7d"}
			wantStatus(t, serveGet(t, tt.handler(proxy), tt.path), http.StatusBadGateway, "2.0.0")
		})
		t.Run(tt.name+" exempt", func(t *testing.T) {
			proxy, _, _, _ := setupTestProxy(t)
			proxy.Cooldown = &cooldown.Config{Default: "7d", Packages: map[string]string{tt.purl: "0"}}
			w := serveGet(t, tt.handler(proxy), tt.path)
			if w.Code != http.StatusOK {
				t.Errorf("status = %d, want 200 for a package cooldown doesn't apply to", w.Code)
			}
		})
	}
}
