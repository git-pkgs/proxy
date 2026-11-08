package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/registries/fetch"
)

func TestNPMContentAddressedTarball(t *testing.T) {
	for _, tc := range []struct {
		cacheMetadata, stream bool
		metadata, tarballs    int64
	}{
		{false, false, 2, 1},
		{false, true, 3, 2},
		{true, false, 1, 1},
		{true, true, 1, 2},
	} {
		t.Run(fmt.Sprintf("metadata=%t/stream=%t", tc.cacheMetadata, tc.stream), func(t *testing.T) {
			var metadataCalls, tarballCalls atomic.Int64
			upstream := npmContentAddressedRegistry(t, &metadataCalls, &tarballCalls)
			p, _, _, _ := setupTestProxy(t)
			p.CacheMetadata = tc.cacheMetadata
			p.MetadataTTL = time.Hour
			p.StreamArtifacts = tc.stream
			p.Cooldown = &cooldown.Config{Default: "7d"}
			p.HTTPClient = upstream.Client()
			fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
			p.Fetcher = fetcher
			t.Cleanup(func() { _ = fetcher.Close() })
			h := NewNPMHandler(p, "http://proxy.test", upstream.URL+"/registry")
			routes := http.StripPrefix("/npm", h.Routes())
			metadata := httptest.NewRecorder()
			routes.ServeHTTP(metadata, httptest.NewRequest(http.MethodGet, "/npm/@example/widget", nil))
			if metadata.Code != http.StatusOK {
				t.Fatalf("metadata status = %d: %s", metadata.Code, metadata.Body.String())
			}
			tarball := npmVersionTarball(metadata.Body.Bytes(), testVersion100)
			if want := "http://proxy.test/npm/@example%2Fwidget/-/widget-1.0.0.tgz"; tarball != want {
				t.Fatalf("rewritten tarball = %q, want %q", tarball, want)
			}
			for i := range 2 {
				response := httptest.NewRecorder()
				routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, tarball, nil))
				if response.Code != http.StatusOK || response.Body.String() != "tarball bytes" {
					t.Fatalf("download %d = %d %q", i, response.Code, response.Body.String())
				}
				if !tc.stream {
					upstream.Close()
				}
			}
			if metadataCalls.Load() != tc.metadata || tarballCalls.Load() != tc.tarballs {
				t.Errorf("upstream requests: metadata=%d tarballs=%d, want %d %d", metadataCalls.Load(), tarballCalls.Load(), tc.metadata, tc.tarballs)
			}
		})
	}
}

func npmContentAddressedRegistry(t *testing.T, metadataCalls, tarballCalls *atomic.Int64) *httptest.Server {
	t.Helper()
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/registry/@example/widget":
			metadataCalls.Add(1)
			w.Header().Set("Content-Type", contentTypeJSON)
			_, _ = fmt.Fprintf(w, `{"name":"@example/widget","time":{"1.0.0":%q},"versions":{"1.0.0":{"dist":{"tarball":%q}}}}`,
				time.Now().Add(-30*24*time.Hour).Format(time.RFC3339), upstream.URL+"/registry/download/@example/widget/1.0.0/abc123?token=example")
		case "/registry/download/@example/widget/1.0.0/abc123":
			if r.URL.RawQuery != "token=example" {
				t.Errorf("tarball query = %q", r.URL.RawQuery)
			}
			tarballCalls.Add(1)
			_, _ = io.WriteString(w, "tarball bytes")
		default:
			t.Errorf("unexpected upstream request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func TestNPMTarballMetadataFallback(t *testing.T) {
	for _, body := range []string{`{"versions":{}}`, `{"versions":{"1.0.0":{"dist":{}}}}`} {
		t.Run(body, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, body)
			}))
			t.Cleanup(upstream.Close)
			p, _, _, fetcher := setupTestProxy(t)
			p.CacheMetadata = true
			p.MetadataTTL = time.Hour
			fetcher.artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader("package"))}
			h := NewNPMHandler(p, "http://proxy.test", upstream.URL)
			if _, _, err := p.FetchOrCacheMetadata(t.Context(), "npm", "@example/widget", upstream.URL); err != nil {
				t.Fatal(err)
			}
			upstream.Close()
			response := httptest.NewRecorder()
			h.Routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/@example/widget/-/widget-1.0.0.tgz", nil))
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", response.Code, response.Body.String())
			}
			if want := upstream.URL + "/@example/widget/-/widget-1.0.0.tgz"; fetcher.fetchedURL != want {
				t.Errorf("fetched URL = %q, want %q", fetcher.fetchedURL, want)
			}
		})
	}
}

// TestNPMTarballWithoutMetadataIsNotFetched checks that a tarball whose
// packument can't be read, so its integrity is unknown, is neither fetched
// nor cached.
func TestNPMTarballWithoutMetadataIsNotFetched(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		limit  int64
	}{
		{name: "unavailable", status: http.StatusServiceUnavailable, body: "unavailable"},
		{name: "over metadata_max_size", status: http.StatusOK, body: `{"versions":{"1.0.0":{"dist":{"integrity":"` + sha512SRI("package") + `"}}}}`, limit: 16},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(upstream.Close)
			p, _, _, fetcher := setupTestProxy(t)
			if tc.limit > 0 {
				p.MetadataMaxSize = tc.limit
			}
			fetcher.artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader("tampered"))}
			h := NewNPMHandler(p, "http://proxy.test", upstream.URL)

			response := httptest.NewRecorder()
			h.Routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/@example/widget/-/widget-1.0.0.tgz", nil))
			if response.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502: %s", response.Code, response.Body.String())
			}
			if fetcher.fetchedURL != "" {
				t.Errorf("fetched %q without the metadata to verify it", fetcher.fetchedURL)
			}
			cached, err := p.GetCachedArtifact(t.Context(), "npm", "@example/widget", "1.0.0", "widget-1.0.0.tgz")
			if err != nil {
				t.Fatal(err)
			}
			if cached != nil {
				_ = cached.Reader.Close()
				t.Error("tarball was cached without verification")
			}
		})
	}
}

// serveEmptyPackuments answers every metadata request with a packument that
// lists no versions, for tests about the tarball download itself.
func serveEmptyPackuments(p *Proxy) {
	p.HTTPClient = &http.Client{Transport: pypiRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return pypiHTTPResponse(r, "application/json", `{"versions":{}}`), nil
	})}
}

func TestNPMTarballRejectsUnsafeMetadataURL(t *testing.T) {
	for _, target := range []string{
		"https://outside.invalid/package.tgz", "//outside.invalid/package.tgz", "/registry/package.tgz",
		"UPSTREAM/outside/package.tgz", "UPSTREAM/registry-other/package.tgz",
		"UPSTREAM/registry/../outside/package.tgz", "UPSTREAM/registry/%2e%2e/outside/package.tgz",
		"UPSTREAM/registry/%252e%252e/outside/package.tgz", "UPSTREAM/registry/..%2foutside/package.tgz",
		"UPSTREAM/registry/..%5coutside/package.tgz", "UPSTREAM/registry/package.tgz#fragment",
		"USERINFO/registry/package.tgz", "SCHEME/registry/package.tgz",
	} {
		t.Run(target, func(t *testing.T) {
			var upstream *httptest.Server
			upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				address, _ := url.Parse(upstream.URL)
				address.User = url.UserPassword("user", "secret")
				tarball := strings.NewReplacer("UPSTREAM", upstream.URL, "USERINFO", address.String(), "SCHEME", strings.Replace(upstream.URL, "http:", "https:", 1)).Replace(target)
				_ = json.NewEncoder(w).Encode(map[string]any{"versions": map[string]any{testVersion100: map[string]any{"dist": map[string]string{"tarball": tarball}}}})
			}))
			t.Cleanup(upstream.Close)
			p, _, _, fetcher := setupTestProxy(t)
			h := NewNPMHandler(p, "http://proxy.test", upstream.URL+"/registry")
			response := httptest.NewRecorder()
			h.Routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/widget/-/widget-1.0.0.tgz", nil))
			if response.Code != http.StatusBadGateway || fetcher.fetchCalled {
				t.Errorf("status = %d, fetched = %t; body: %s", response.Code, fetcher.fetchCalled, response.Body.String())
			}
		})
	}
}

// TestNPMTarballCooldownRefetchesStalePackument checks that a tarball for a
// version the cached packument does not list yet is decided on the packument
// upstream serves now, not let through for want of a publish time.
func TestNPMTarballCooldownRefetchesStalePackument(t *testing.T) {
	for _, tc := range []struct {
		name      string
		published time.Time
		down      bool
		want      int
	}{
		{"recent", time.Now().Add(-time.Hour), false, http.StatusNotFound},
		// Without upstream the version can't be checked, so it isn't served.
		{"upstream down", time.Now().Add(-time.Hour), true, http.StatusBadGateway},
		// The refetched entry's integrity is checked, so a tarball that
		// doesn't match it is refused rather than served unverified.
		{"old", time.Now().Add(-30 * 24 * time.Hour), false, http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var released atomic.Bool
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				old := time.Now().Add(-60 * 24 * time.Hour).Format(time.RFC3339)
				if released.Load() && tc.down {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				if !released.Load() {
					_, _ = fmt.Fprintf(w, `{"name":"widget","time":{"1.0.0":%q},"versions":{"1.0.0":{"dist":{}}}}`, old)
					return
				}
				_, _ = fmt.Fprintf(w, `{"name":"widget","time":{"1.0.0":%q,"2.0.0":%q},"versions":{"1.0.0":{"dist":{}},"2.0.0":{"dist":{"integrity":%q}}}}`,
					old, tc.published.Format(time.RFC3339), sha512SRI("package"))
			}))
			t.Cleanup(upstream.Close)
			p, _, _, fetcher := setupTestProxy(t)
			p.CacheMetadata = true
			p.MetadataTTL = time.Hour
			p.Cooldown = &cooldown.Config{Default: "7d"}
			p.HTTPClient = upstream.Client()
			fetcher.artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader("tampered"))}
			h := NewNPMHandler(p, "http://proxy.test", upstream.URL)

			metadata := httptest.NewRecorder()
			h.Routes().ServeHTTP(metadata, httptest.NewRequest(http.MethodGet, "/widget", nil))
			if metadata.Code != http.StatusOK {
				t.Fatalf("metadata status = %d: %s", metadata.Code, metadata.Body.String())
			}
			released.Store(true)

			response := httptest.NewRecorder()
			h.Routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/widget/-/widget-2.0.0.tgz", nil))
			if response.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", response.Code, tc.want, response.Body.String())
			}
		})
	}
}
