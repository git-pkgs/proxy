package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/artifacts"
	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/proxy/internal/packageurl"
	"github.com/git-pkgs/registries/fetch"
)

func newStreamingProxy(t *testing.T, body string) (*Proxy, *mockStorage, *mockFetcher) {
	t.Helper()
	proxy, _, store, fetcher := setupTestProxy(t)
	proxy.StreamArtifacts = true
	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader(body)),
		Size:        int64(len(body)),
		ContentType: "application/gzip",
	}
	return proxy, store, fetcher
}

func TestStreamArtifactsFromURLStreamsWithoutStoring(t *testing.T) {
	proxy, store, fetcher := newStreamingProxy(t, "fetched content")

	result, err := proxy.GetOrFetchArtifactFromURL(context.Background(), "pypi", "newpkg", "1.0.0",
		"newpkg-1.0.0.tar.gz", "https://pypi.org/files/newpkg-1.0.0.tar.gz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = result.Reader.Close() }()

	body, err := io.ReadAll(result.Reader)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if string(body) != "fetched content" {
		t.Errorf("body = %q, want %q", body, "fetched content")
	}
	if fetcher.fetchedURL != "https://pypi.org/files/newpkg-1.0.0.tar.gz" {
		t.Errorf("fetched URL = %q", fetcher.fetchedURL)
	}
	if result.Cached {
		t.Error("streaming result reported as cached")
	}
	if result.Artifact.Size != int64(len("fetched content")) {
		t.Errorf("Size = %d, want the upstream size", result.Artifact.Size)
	}
	if result.Artifact.MediaType != "application/gzip" {
		t.Errorf("MediaType = %q", result.Artifact.MediaType)
	}
	if len(store.files) != 0 {
		t.Errorf("streaming stored %d files, want none", len(store.files))
	}
	cached, err := proxy.DB.GetCachedArtifact("pkg:pypi/newpkg", "pkg:pypi/newpkg@1.0.0", "newpkg-1.0.0.tar.gz")
	if err != nil {
		t.Fatalf("GetCachedArtifact: %v", err)
	}
	if cached != nil {
		t.Error("streaming recorded the artifact in the cache database")
	}
}

func TestStreamArtifactsGetOrFetchArtifactStreamsWithoutStoring(t *testing.T) {
	proxy, store, fetcher := newStreamingProxy(t, "tarball data")

	result, err := proxy.GetOrFetchArtifact(context.Background(), "npm", "leftpad", testVersion100, "leftpad-1.0.0.tgz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = result.Reader.Close() }()

	body, _ := io.ReadAll(result.Reader)
	if string(body) != "tarball data" {
		t.Errorf("body = %q", body)
	}
	if !fetcher.fetchCalled {
		t.Error("upstream was not fetched")
	}
	if len(store.files) != 0 {
		t.Errorf("streaming stored %d files, want none", len(store.files))
	}
}

func TestStreamArtifactsIgnoresCachedArtifacts(t *testing.T) {
	proxy, _, store, fetcher := setupTestProxy(t)
	fetcher.artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader("old bytes"))}
	url := "https://pypi.org/files/newpkg-1.0.0.tar.gz"

	// Populate the cache in normal mode first.
	result, err := proxy.GetOrFetchArtifactFromURL(context.Background(), "pypi", "newpkg", "1.0.0", "newpkg-1.0.0.tar.gz", url)
	if err != nil {
		t.Fatalf("priming cache: %v", err)
	}
	_ = result.Reader.Close()
	if len(store.files) != 1 {
		t.Fatalf("expected the cache to hold the artifact, got %d files", len(store.files))
	}

	proxy.StreamArtifacts = true
	cached, err := proxy.GetCachedArtifact(context.Background(), "pypi", "newpkg", "1.0.0", "newpkg-1.0.0.tar.gz")
	if err != nil || cached != nil {
		t.Fatalf("GetCachedArtifact = %v, %v; want nil, nil while streaming", cached, err)
	}

	fetcher.fetchCalled = false
	fetcher.artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader("new bytes"))}
	result, err = proxy.GetOrFetchArtifactFromURL(context.Background(), "pypi", "newpkg", "1.0.0", "newpkg-1.0.0.tar.gz", url)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = result.Reader.Close() }()
	body, _ := io.ReadAll(result.Reader)
	if !fetcher.fetchCalled || string(body) != "new bytes" {
		t.Errorf("streaming served %q (fetched=%v), want a fresh upstream fetch", body, fetcher.fetchCalled)
	}
}

func TestStreamArtifactsVerifiesUpstreamDigest(t *testing.T) {
	const content = "blob bytes"

	t.Run("matching digest streams the body", func(t *testing.T) {
		proxy, _, _ := newStreamingProxy(t, content)
		result, err := proxy.GetOrFetchArtifactFromURLWithDigest(context.Background(), "oci", "library/app", "v1",
			"blob", "https://registry.test/blob", "sha256:"+sha256Hex(content))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer func() { _ = result.Reader.Close() }()

		body, err := io.ReadAll(result.Reader)
		if err != nil {
			t.Fatalf("reading verified body: %v", err)
		}
		if string(body) != content {
			t.Errorf("body = %q", body)
		}
		if got := result.Artifact.Digest.Encoded(); got != sha256Hex(content) {
			t.Errorf("Digest = %q, want the upstream digest", got)
		}
		if result.Artifact.Size >= 0 {
			t.Errorf("Size = %d, want unknown so the response is chunked", result.Artifact.Size)
		}
	})

	t.Run("mismatched digest fails the read", func(t *testing.T) {
		proxy, _, _ := newStreamingProxy(t, content)
		result, err := proxy.GetOrFetchArtifactFromURLWithDigest(context.Background(), "oci", "library/app", "v1",
			"blob", "https://registry.test/blob", "sha256:"+sha256Hex("other bytes"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer func() { _ = result.Reader.Close() }()

		if _, err := io.ReadAll(result.Reader); !errors.Is(err, ErrArtifactDigestMismatch) {
			t.Errorf("read error = %v, want ErrArtifactDigestMismatch", err)
		}
	})
}

func TestServeArtifactAbortsOnStreamedDigestMismatch(t *testing.T) {
	proxy, _, _ := newStreamingProxy(t, "blob bytes")
	result, err := proxy.GetOrFetchArtifactFromURLWithDigest(context.Background(), "oci", "library/app", "v1",
		"blob", "https://registry.test/blob", "sha256:"+sha256Hex("other bytes"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rec := httptest.NewRecorder()
	defer func() {
		if r := recover(); r != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want http.ErrAbortHandler", r)
		}
		if rec.Header().Get(headerContentLength) != "" {
			t.Errorf("Content-Length = %q, want none so the client sees an unterminated response",
				rec.Header().Get(headerContentLength))
		}
	}()
	ServeArtifact(rec, result)
}

func TestNPMDownloadCooldownWhileStreaming(t *testing.T) {
	now := time.Now()
	packument := `{
		"name": "leftpad",
		"dist-tags": {"latest": "2.0.0"},
		"time": {
			"1.0.0": "` + now.Add(-30*24*time.Hour).Format(time.RFC3339) + `",
			"2.0.0": "` + now.Add(-1*time.Hour).Format(time.RFC3339) + `"
		},
		"versions": {"1.0.0": {}, "2.0.0": {}}
	}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentTypeJSON)
		_, _ = io.WriteString(w, packument)
	}))
	defer upstream.Close()

	tests := []struct {
		name       string
		version    string
		wantStatus int
	}{
		{"published before the window streams the tarball", testVersion100, http.StatusOK},
		{"published inside the window is withheld", "2.0.0", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxy, store, fetcher := newStreamingProxy(t, "tarball data")
			proxy.HTTPClient = upstream.Client()
			proxy.Cooldown = &cooldown.Config{Default: "7d"}

			srv := httptest.NewServer(NewNPMHandler(proxy, "http://proxy.test", upstream.URL).Routes())
			defer srv.Close()

			resp, err := http.Get(srv.URL + "/leftpad/-/leftpad-" + tt.version + ".tgz")
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if tt.wantStatus == http.StatusOK && string(body) != "tarball data" {
				t.Errorf("body = %q", body)
			}
			if tt.wantStatus == http.StatusNotFound && fetcher.fetchCalled {
				t.Error("fetched a version that is still inside the cooldown window")
			}
			if len(store.files) != 0 {
				t.Errorf("streaming stored %d files, want none", len(store.files))
			}
		})
	}
}

// truncatingUpstream answers every request matching match with raw, then
// closes the connection, so the response body ends early.
func truncatingUpstream(t *testing.T, match func(*http.Request) bool, raw string, fallback http.HandlerFunc) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !match(r) {
			fallback(w, r)
			return
		}
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_, _ = buf.WriteString(raw)
		_ = buf.Flush()
		_ = conn.Close()
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

// requireIncompleteResponse fails unless reading the response surfaces an error,
// i.e. the client cannot mistake the body for a complete download.
func requireIncompleteResponse(t *testing.T, url string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Fatalf("client read a complete %d response (Content-Length %q, body %q), want an incomplete one",
			resp.StatusCode, resp.Header.Get(headerContentLength), body)
	}
}

func useRealFetcher(t *testing.T, proxy *Proxy, upstream *httptest.Server) {
	t.Helper()
	proxy.HTTPClient = upstream.Client()
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
}

func TestStreamArtifactsOCIBlobShorterThanContentLengthIsIncomplete(t *testing.T) {
	digest := "sha256:" + sha256Hex(strings.Repeat("x", 100))
	upstream := truncatingUpstream(t,
		func(r *http.Request) bool { return strings.Contains(r.URL.Path, "/blobs/") },
		"HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nContent-Length: 100\r\n\r\nshort",
		http.NotFound)

	proxy, _, store, _ := setupTestProxy(t)
	proxy.StreamArtifacts = true
	useRealFetcher(t, proxy, upstream)
	h := NewContainerHandlerWithRegistry(proxy, "http://proxy.example", "", map[string]string{"ghcr": upstream.URL})
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	requireIncompleteResponse(t, srv.URL+"/upstream/ghcr/owner/demo/blobs/"+digest)
	if len(store.files) != 0 {
		t.Errorf("streaming stored %d files, want none", len(store.files))
	}
}

func TestStreamArtifactsNPMTarballWithoutFinalChunkIsIncomplete(t *testing.T) {
	upstream := truncatingUpstream(t,
		func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, ".tgz") },
		"HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nshort\r\n",
		http.NotFound)

	proxy, _, store, _ := setupTestProxy(t)
	proxy.StreamArtifacts = true
	useRealFetcher(t, proxy, upstream)
	srv := httptest.NewServer(NewNPMHandler(proxy, "http://proxy.test", upstream.URL).Routes())
	defer srv.Close()

	requireIncompleteResponse(t, srv.URL+"/leftpad/-/leftpad-1.0.0.tgz")
	if len(store.files) != 0 {
		t.Errorf("streaming stored %d files, want none", len(store.files))
	}
}

func TestServeArtifactAbortsOnShortRead(t *testing.T) {
	tests := []struct {
		name   string
		reader io.Reader
		size   int64
	}{
		{"read error", io.MultiReader(strings.NewReader("short"), iotestErrReader{io.ErrUnexpectedEOF}), -1},
		{"fewer bytes than the declared size", strings.NewReader("short"), 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := &CacheResult{Reader: io.NopCloser(tt.reader), Artifact: artifacts.Artifact{Size: tt.size}}
			defer func() {
				if r := recover(); r != http.ErrAbortHandler {
					t.Fatalf("recovered %v, want http.ErrAbortHandler", r)
				}
			}()
			ServeArtifact(httptest.NewRecorder(), result)
		})
	}
}

type iotestErrReader struct{ err error }

func (r iotestErrReader) Read([]byte) (int, error) { return 0, r.err }

func TestStreamArtifactsSwiftArchiveHeadIgnoresCachedEntry(t *testing.T) {
	archive := []byte("cached archive")
	checksum := sha256.Sum256(archive)
	var archiveRequests int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".zip") {
			archiveRequests++
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"apple.example","version":"1.2.3","resources":[{"name":"source-archive","type":"application/zip","checksum":%q}]}`, hex.EncodeToString(checksum[:]))
	}))
	defer upstream.Close()

	proxy, _, _, fetcher := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader(string(archive))),
		Size:        int64(len(archive)),
		ContentType: "application/zip",
	}
	packagePURL, versionPURL := packageurl.MakeCacheStrings("swift", "apple/example", "1.2.3")
	cached, err := proxy.getOrFetchArtifactFromURLWithCachePURLs(
		context.Background(), "swift", "apple/example", "1.2.3", "example-1.2.3.zip",
		packagePURL, versionPURL, upstream.URL+"/apple/example/1.2.3.zip", nil, hex.EncodeToString(checksum[:]), "",
	)
	if err != nil {
		t.Fatalf("seeding cache in normal mode: %v", err)
	}
	_ = cached.Reader.Close()

	proxy.StreamArtifacts = true
	fetcher.artifact = nil
	fetcher.fetchErr = fetch.ErrNotFound
	handler := NewSwiftHandler(proxy, "https://proxy.example", upstream.URL).Routes()

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodHead, "/apple/example/1.2.3.zip", nil))
	if w.Code == http.StatusOK {
		t.Fatalf("HEAD served the cached archive (Content-Length %q) while streaming artifacts", w.Header().Get(headerContentLength))
	}
	if archiveRequests == 0 {
		t.Error("HEAD did not ask the upstream archive")
	}

	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/apple/example/1.2.3.zip", nil))
	if w.Code == http.StatusOK {
		t.Fatalf("GET served the cached archive while streaming artifacts")
	}
}
