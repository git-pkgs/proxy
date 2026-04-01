package handler

import (
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/storage"
	"github.com/git-pkgs/purl"
	"github.com/git-pkgs/registries/fetch"
)

// seedPackageWithPURL seeds a package using purl.MakePURLString for PURL generation,
// matching how the handlers construct PURLs internally.
func seedPackageWithPURL(t *testing.T, db *database.DB, store *mockStorage, ecosystem, name, version, filename, content string) {
	t.Helper()

	pkgPURL := purl.MakePURLString(ecosystem, name, "")
	versionPURL := purl.MakePURLString(ecosystem, name, version)

	pkg := &database.Package{
		PURL:      pkgPURL,
		Ecosystem: ecosystem,
		Name:      name,
	}
	if err := db.UpsertPackage(pkg); err != nil {
		t.Fatalf("failed to upsert package: %v", err)
	}

	ver := &database.Version{
		PURL:        versionPURL,
		PackagePURL: pkgPURL,
	}
	if err := db.UpsertVersion(ver); err != nil {
		t.Fatalf("failed to upsert version: %v", err)
	}

	storagePath := storage.ArtifactPath(ecosystem, "", name, version, filename)
	store.files[storagePath] = []byte(content)

	art := &database.Artifact{
		VersionPURL: versionPURL,
		Filename:    filename,
		UpstreamURL: "https://example.com/" + filename,
		StoragePath: sql.NullString{String: storagePath, Valid: true},
		ContentHash: sql.NullString{String: "abc123", Valid: true},
		Size:        sql.NullInt64{Int64: int64(len(content)), Valid: true},
		ContentType: sql.NullString{String: "application/octet-stream", Valid: true},
		FetchedAt:   sql.NullTime{Time: time.Now(), Valid: true},
	}
	if err := db.UpsertArtifact(art); err != nil {
		t.Fatalf("failed to upsert artifact: %v", err)
	}
}

// assertUpstreamProxied verifies that a handler proxies a request to the upstream
// server and returns the expected response body. The makeHandler function receives
// a configured Proxy and the upstream URL, and returns the handler to test.
func assertUpstreamProxied(t *testing.T, wantBody, path string, makeHandler func(*Proxy, string) http.Handler) {
	t.Helper()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, wantBody)
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()

	srv := httptest.NewServer(makeHandler(proxy, upstream.URL))
	defer srv.Close()

	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != wantBody {
		t.Errorf("body = %q, want %q", body, wantBody)
	}
}

func TestGemHandler_DownloadCacheHit(t *testing.T) {
	proxy, db, store, _ := setupTestProxy(t)
	seedPackage(t, db, store, "gem", "rails", "7.1.0", "rails-7.1.0.gem", "gem binary data")

	h := NewGemHandler(proxy, "http://localhost")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/gems/rails-7.1.0.gem")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "gem binary data" {
		t.Errorf("body = %q, want %q", body, "gem binary data")
	}
}

func TestGemHandler_DownloadCacheHitMultiHyphen(t *testing.T) {
	proxy, db, store, _ := setupTestProxy(t)
	seedPackage(t, db, store, "gem", "aws-sdk-s3", "1.142.0", "aws-sdk-s3-1.142.0.gem", "aws gem")

	h := NewGemHandler(proxy, "http://localhost")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/gems/aws-sdk-s3-1.142.0.gem")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "aws gem" {
		t.Errorf("body = %q, want %q", body, "aws gem")
	}
}

func TestGemHandler_InvalidFilename(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	h := NewGemHandler(proxy, "http://localhost")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	tests := []struct {
		path string
		code int
	}{
		{"/gems/notagem.tar.gz", http.StatusBadRequest},
		{"/gems/noversion.gem", http.StatusBadRequest},
		{"/gems/.gem", http.StatusBadRequest},
	}

	for _, tt := range tests {
		resp, err := http.Get(srv.URL + tt.path)
		if err != nil {
			t.Fatalf("request to %s failed: %v", tt.path, err)
		}
		_ = resp.Body.Close()

		if resp.StatusCode != tt.code {
			t.Errorf("GET %s: status = %d, want %d", tt.path, resp.StatusCode, tt.code)
		}
	}
}

func TestGemHandler_UpstreamProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test", "upstream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "upstream specs data")
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	h := &GemHandler{
		proxy:       proxy,
		upstreamURL: upstream.URL,
		proxyURL:    "http://localhost",
	}
	proxy.HTTPClient = upstream.Client()

	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/versions")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "upstream specs data" {
		t.Errorf("body = %q, want %q", body, "upstream specs data")
	}
	if resp.Header.Get("X-Test") != "upstream" {
		t.Errorf("missing upstream header")
	}
}

func TestGemHandler_CacheMiss(t *testing.T) {
	proxy, _, _, fetcher := setupTestProxy(t)
	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("fetched gem")),
		ContentType: "application/octet-stream",
	}

	h := NewGemHandler(proxy, "http://localhost")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/gems/sinatra-3.0.0.gem")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if !fetcher.fetchCalled {
		t.Error("expected fetcher to be called on cache miss")
	}
}

func TestGoHandler_DownloadCacheHit(t *testing.T) {
	proxy, db, store, _ := setupTestProxy(t)
	seedPackage(t, db, store, "golang", "golang.org/x/text", "v0.14.0", "text@v0.14.0.zip", "go module zip")

	h := NewGoHandler(proxy, "http://localhost")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/golang.org/x/text/@v/v0.14.0.zip")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "go module zip" {
		t.Errorf("body = %q, want %q", body, "go module zip")
	}
}

func TestGoHandler_MethodNotAllowed(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	h := NewGoHandler(proxy, "http://localhost")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/golang.org/x/text/@v/v0.14.0.zip", "", nil)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
}

func TestGoHandler_NotFound(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	h := NewGoHandler(proxy, "http://localhost")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/some/unknown/path")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestGoHandler_UnknownAtVSuffix(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	h := NewGoHandler(proxy, "http://localhost")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/golang.org/x/text/@v/v0.14.0.unknown")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestGoHandler_UpstreamProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "v0.14.0\nv0.13.0\n")
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	h := &GoHandler{
		proxy:       proxy,
		upstreamURL: upstream.URL,
		proxyURL:    "http://localhost",
	}
	proxy.HTTPClient = upstream.Client()

	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	tests := []string{
		"/golang.org/x/text/@v/list",
		"/golang.org/x/text/@v/v0.14.0.info",
		"/golang.org/x/text/@v/v0.14.0.mod",
		"/golang.org/x/text/@latest",
		"/sumdb/sum.golang.org/lookup/golang.org/x/text@v0.14.0",
	}

	for _, path := range tests {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s failed: %v", path, err)
		}
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status = %d, want %d", path, resp.StatusCode, http.StatusOK)
		}
	}
}

func TestGoHandler_CacheMiss(t *testing.T) {
	proxy, _, _, fetcher := setupTestProxy(t)
	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("module zip data")),
		ContentType: "application/zip",
	}

	h := NewGoHandler(proxy, "http://localhost")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/example.com/mod/@v/v1.0.0.zip")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if !fetcher.fetchCalled {
		t.Error("expected fetcher to be called on cache miss")
	}
}

func TestHexHandler_DownloadCacheHit(t *testing.T) {
	proxy, db, store, _ := setupTestProxy(t)
	seedPackage(t, db, store, "hex", "phoenix", "1.7.10", "phoenix-1.7.10.tar", "hex tarball")

	h := NewHexHandler(proxy, "http://localhost")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/tarballs/phoenix-1.7.10.tar")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hex tarball" {
		t.Errorf("body = %q, want %q", body, "hex tarball")
	}
}

func TestHexHandler_InvalidFilename(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	h := NewHexHandler(proxy, "http://localhost")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	tests := []struct {
		path string
		code int
	}{
		{"/tarballs/notatar.zip", http.StatusBadRequest},
		{"/tarballs/noversion.tar", http.StatusBadRequest},
	}

	for _, tt := range tests {
		resp, err := http.Get(srv.URL + tt.path)
		if err != nil {
			t.Fatalf("request to %s failed: %v", tt.path, err)
		}
		_ = resp.Body.Close()

		if resp.StatusCode != tt.code {
			t.Errorf("GET %s: status = %d, want %d", tt.path, resp.StatusCode, tt.code)
		}
	}
}

func TestHexHandler_UpstreamProxy(t *testing.T) {
	assertUpstreamProxied(t, "hex registry data", "/packages/phoenix",
		func(proxy *Proxy, upstreamURL string) http.Handler {
			h := &HexHandler{proxy: proxy, upstreamURL: upstreamURL, proxyURL: "http://localhost"}
			return h.Routes()
		},
	)
}

func TestHexHandler_CacheMiss(t *testing.T) {
	proxy, _, _, fetcher := setupTestProxy(t)
	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("fetched hex")),
		ContentType: "application/x-tar",
	}

	h := NewHexHandler(proxy, "http://localhost")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/tarballs/plug-1.15.0.tar")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if !fetcher.fetchCalled {
		t.Error("expected fetcher to be called on cache miss")
	}
}

func TestCondaHandler_DownloadCacheHit(t *testing.T) {
	proxy, db, store, _ := setupTestProxy(t)
	seedPackageWithPURL(t, db, store, "conda", "main/numpy", "1.24.0", "numpy-1.24.0-py311h64a7726_0.conda", "conda pkg")

	h := NewCondaHandler(proxy, "http://localhost")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/main/linux-64/numpy-1.24.0-py311h64a7726_0.conda")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "conda pkg" {
		t.Errorf("body = %q, want %q", body, "conda pkg")
	}
}

func TestCondaHandler_DownloadTarBz2CacheHit(t *testing.T) {
	proxy, db, store, _ := setupTestProxy(t)
	seedPackageWithPURL(t, db, store, "conda", "main/scipy", "1.11.0", "scipy-1.11.0-py311hb2e3ea1_0.tar.bz2", "tar bz2 data")

	h := NewCondaHandler(proxy, "http://localhost")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/main/linux-64/scipy-1.11.0-py311hb2e3ea1_0.tar.bz2")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "tar bz2 data" {
		t.Errorf("body = %q, want %q", body, "tar bz2 data")
	}
}

func TestCondaHandler_NonPackageFileProxied(t *testing.T) {
	assertUpstreamProxied(t, "repodata json", "/main/linux-64/repodata.json",
		func(proxy *Proxy, upstreamURL string) http.Handler {
			h := &CondaHandler{proxy: proxy, upstreamURL: upstreamURL, proxyURL: "http://localhost"}
			return h.Routes()
		},
	)
}

func TestCondaHandler_CacheMiss(t *testing.T) {
	proxy, _, _, fetcher := setupTestProxy(t)
	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("fetched conda")),
		ContentType: "application/octet-stream",
	}

	h := NewCondaHandler(proxy, "http://localhost")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("should not hit upstream for .conda files when fetcher is set")
	}))
	defer upstream.Close()
	h.upstreamURL = upstream.URL
	proxy.HTTPClient = upstream.Client()

	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/conda-forge/linux-64/pandas-2.0.0-py311h320fe9a_0.conda")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if !fetcher.fetchCalled {
		t.Error("expected fetcher to be called on cache miss")
	}

	want := upstream.URL + "/conda-forge/linux-64/pandas-2.0.0-py311h320fe9a_0.conda"
	if fetcher.fetchedURL != want {
		t.Errorf("upstream URL = %q, want %q", fetcher.fetchedURL, want)
	}
}

func TestCRANHandler_SourceDownloadCacheHit(t *testing.T) {
	proxy, db, store, _ := setupTestProxy(t)
	seedPackageWithPURL(t, db, store, "cran", "ggplot2", "3.4.0", "ggplot2_3.4.0.tar.gz", "cran source")

	h := NewCRANHandler(proxy, "http://localhost")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/src/contrib/ggplot2_3.4.0.tar.gz")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "cran source" {
		t.Errorf("body = %q, want %q", body, "cran source")
	}
}

func TestCRANHandler_BinaryDownloadCacheHit(t *testing.T) {
	proxy, db, store, _ := setupTestProxy(t)
	seedPackageWithPURL(t, db, store, "cran", "dplyr", "1.1.0_windows_4.3", "dplyr_1.1.0.zip", "cran binary")

	h := NewCRANHandler(proxy, "http://localhost")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/bin/windows/contrib/4.3/dplyr_1.1.0.zip")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "cran binary" {
		t.Errorf("body = %q, want %q", body, "cran binary")
	}
}

func TestCRANHandler_NonPackageFileProxied(t *testing.T) {
	assertUpstreamProxied(t, "PACKAGES index", "/src/contrib/PACKAGES",
		func(proxy *Proxy, upstreamURL string) http.Handler {
			h := &CRANHandler{proxy: proxy, upstreamURL: upstreamURL, proxyURL: "http://localhost"}
			return h.Routes()
		},
	)
}

func TestCRANHandler_SourceNonTarGzProxied(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "some other file")
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	h := &CRANHandler{
		proxy:       proxy,
		upstreamURL: upstream.URL,
		proxyURL:    "http://localhost",
	}
	proxy.HTTPClient = upstream.Client()

	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/src/contrib/somefile.txt")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestCRANHandler_CacheMiss(t *testing.T) {
	proxy, _, _, fetcher := setupTestProxy(t)
	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("fetched cran")),
		ContentType: "application/x-gzip",
	}

	h := NewCRANHandler(proxy, "http://localhost")
	h.upstreamURL = "https://cran.r-project.org"

	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/src/contrib/tidyr_1.3.0.tar.gz")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if !fetcher.fetchCalled {
		t.Error("expected fetcher to be called on cache miss")
	}

	want := "https://cran.r-project.org/src/contrib/tidyr_1.3.0.tar.gz"
	if fetcher.fetchedURL != want {
		t.Errorf("upstream URL = %q, want %q", fetcher.fetchedURL, want)
	}
}

func TestCRANHandler_BinaryDownloadCacheMiss(t *testing.T) {
	proxy, _, _, fetcher := setupTestProxy(t)
	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("fetched binary")),
		ContentType: "application/zip",
	}

	h := NewCRANHandler(proxy, "http://localhost")
	h.upstreamURL = "https://cran.r-project.org"

	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/bin/windows/contrib/4.3/dplyr_1.1.0.zip")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if !fetcher.fetchCalled {
		t.Error("expected fetcher to be called on cache miss")
	}

	want := "https://cran.r-project.org/bin/windows/contrib/4.3/dplyr_1.1.0.zip"
	if fetcher.fetchedURL != want {
		t.Errorf("upstream URL = %q, want %q", fetcher.fetchedURL, want)
	}
}

func TestMavenHandler_DownloadCacheHit(t *testing.T) {
	proxy, db, store, _ := setupTestProxy(t)
	seedPackageWithPURL(t, db, store, "maven", "com.google.guava:guava", "32.1.3-jre", "guava-32.1.3-jre.jar", "jar content")

	h := NewMavenHandler(proxy, "http://localhost")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/com/google/guava/guava/32.1.3-jre/guava-32.1.3-jre.jar")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "jar content" {
		t.Errorf("body = %q, want %q", body, "jar content")
	}
}

func TestMavenHandler_MetadataProxied(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "<metadata/>")
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	h := &MavenHandler{
		proxy:       proxy,
		upstreamURL: upstream.URL,
		proxyURL:    "http://localhost",
	}
	proxy.HTTPClient = upstream.Client()

	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	paths := []string{
		"/com/google/guava/guava/maven-metadata.xml",
		"/com/google/guava/guava/32.1.3-jre/guava-32.1.3-jre.jar.sha1",
		"/com/google/guava/guava/32.1.3-jre/guava-32.1.3-jre.jar.md5",
	}

	for _, path := range paths {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s failed: %v", path, err)
		}
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status = %d, want %d", path, resp.StatusCode, http.StatusOK)
		}
	}
}

func TestMavenHandler_EmptyPathNotFound(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	h := NewMavenHandler(proxy, "http://localhost")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestMavenHandler_ArtifactExtensions(t *testing.T) {
	proxy, _, _, fetcher := setupTestProxy(t)

	extensions := []string{".jar", ".war", ".ear", ".pom", ".aar", ".klib"}
	for _, ext := range extensions {
		fetcher.artifact = &fetch.Artifact{
			Body:        io.NopCloser(strings.NewReader("artifact")),
			ContentType: "application/java-archive",
		}
		fetcher.fetchCalled = false

		h := NewMavenHandler(proxy, "http://localhost")

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("should not proxy artifact file %s to upstream", ext)
		}))
		h.upstreamURL = upstream.URL
		proxy.HTTPClient = upstream.Client()

		srv := httptest.NewServer(h.Routes())

		path := fmt.Sprintf("/com/example/lib/1.0/lib-1.0%s", ext)
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s failed: %v", path, err)
		}
		_ = resp.Body.Close()

		if !fetcher.fetchCalled {
			t.Errorf("fetcher not called for %s", ext)
		}

		srv.Close()
		upstream.Close()
	}
}

func TestMavenHandler_CacheMiss(t *testing.T) {
	proxy, _, _, fetcher := setupTestProxy(t)
	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("fetched jar")),
		ContentType: "application/java-archive",
	}

	h := NewMavenHandler(proxy, "http://localhost")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/org/apache/commons/commons-lang3/3.14.0/commons-lang3-3.14.0.jar")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if !fetcher.fetchCalled {
		t.Error("expected fetcher to be called on cache miss")
	}

	want := "https://repo1.maven.org/maven2/org/apache/commons/commons-lang3/3.14.0/commons-lang3-3.14.0.jar"
	if fetcher.fetchedURL != want {
		t.Errorf("upstream URL = %q, want %q", fetcher.fetchedURL, want)
	}
}

func TestNuGetHandler_DownloadCacheMiss(t *testing.T) {
	proxy, _, _, fetcher := setupTestProxy(t)
	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("fetched nupkg")),
		ContentType: "application/octet-stream",
	}

	h := NewNuGetHandler(proxy, "http://localhost")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v3-flatcontainer/newtonsoft.json/13.0.3/newtonsoft.json.13.0.3.nupkg")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if !fetcher.fetchCalled {
		t.Error("expected fetcher to be called on cache miss")
	}

	want := "https://api.nuget.org/v3-flatcontainer/newtonsoft.json/13.0.3/newtonsoft.json.13.0.3.nupkg"
	if fetcher.fetchedURL != want {
		t.Errorf("upstream URL = %q, want %q", fetcher.fetchedURL, want)
	}
}

func TestConanHandler_RecipeFileCacheMiss(t *testing.T) {
	proxy, _, _, fetcher := setupTestProxy(t)
	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("conan export")),
		ContentType: "application/octet-stream",
	}

	h := NewConanHandler(proxy, "http://localhost")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/files/zlib/1.3/_/_/abc123/recipe/conan_export.tgz")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if !fetcher.fetchCalled {
		t.Error("expected fetcher to be called on cache miss")
	}

	want := "https://center.conan.io/v2/files/zlib/1.3/_/_/abc123/recipe/conan_export.tgz"
	if fetcher.fetchedURL != want {
		t.Errorf("upstream URL = %q, want %q", fetcher.fetchedURL, want)
	}
}

func TestConanHandler_PackageFileCacheMiss(t *testing.T) {
	proxy, _, _, fetcher := setupTestProxy(t)
	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("conan package")),
		ContentType: "application/octet-stream",
	}

	h := NewConanHandler(proxy, "http://localhost")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/files/zlib/1.3/_/_/abc123/package/def456/ghi789/conan_package.tgz")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if !fetcher.fetchCalled {
		t.Error("expected fetcher to be called on cache miss")
	}

	want := "https://center.conan.io/v2/files/zlib/1.3/_/_/abc123/package/def456/ghi789/conan_package.tgz"
	if fetcher.fetchedURL != want {
		t.Errorf("upstream URL = %q, want %q", fetcher.fetchedURL, want)
	}
}

func TestDebianHandler_DownloadCacheMiss(t *testing.T) {
	proxy, _, _, fetcher := setupTestProxy(t)
	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("fetched deb")),
		ContentType: "application/vnd.debian.binary-package",
	}

	h := NewDebianHandler(proxy, "http://localhost")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/pool/main/n/nginx/nginx_1.18.0-6_amd64.deb")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if !fetcher.fetchCalled {
		t.Error("expected fetcher to be called on cache miss")
	}

	want := "http://deb.debian.org/debian/pool/main/n/nginx/nginx_1.18.0-6_amd64.deb"
	if fetcher.fetchedURL != want {
		t.Errorf("upstream URL = %q, want %q", fetcher.fetchedURL, want)
	}
}

func TestRPMHandler_DownloadCacheMiss(t *testing.T) {
	proxy, _, _, fetcher := setupTestProxy(t)
	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("fetched rpm")),
		ContentType: "application/x-rpm",
	}

	h := NewRPMHandler(proxy, "http://localhost")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/releases/39/Everything/x86_64/os/Packages/n/nginx-1.24.0-1.fc39.x86_64.rpm")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if !fetcher.fetchCalled {
		t.Error("expected fetcher to be called on cache miss")
	}

	want := "https://dl.fedoraproject.org/pub/fedora/linux/releases/39/Everything/x86_64/os/Packages/n/nginx-1.24.0-1.fc39.x86_64.rpm"
	if fetcher.fetchedURL != want {
		t.Errorf("upstream URL = %q, want %q", fetcher.fetchedURL, want)
	}
}
