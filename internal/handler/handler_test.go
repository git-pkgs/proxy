package handler

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/storage"
	"github.com/git-pkgs/registries/fetch"
)

// mockStorage implements storage.Storage for testing.
type mockStorage struct {
	files    map[string][]byte
	storeErr error
	openErr  error
}

func newMockStorage() *mockStorage {
	return &mockStorage{files: make(map[string][]byte)}
}

func (s *mockStorage) Store(_ context.Context, path string, r io.Reader) (int64, string, error) {
	if s.storeErr != nil {
		return 0, "", s.storeErr
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return 0, "", err
	}
	s.files[path] = data
	return int64(len(data)), "fakehash123", nil
}

func (s *mockStorage) Open(_ context.Context, path string) (io.ReadCloser, error) {
	if s.openErr != nil {
		return nil, s.openErr
	}
	data, ok := s.files[path]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (s *mockStorage) Exists(_ context.Context, path string) (bool, error) {
	_, ok := s.files[path]
	return ok, nil
}

func (s *mockStorage) Delete(_ context.Context, path string) error {
	delete(s.files, path)
	return nil
}

func (s *mockStorage) Size(_ context.Context, path string) (int64, error) {
	data, ok := s.files[path]
	if !ok {
		return 0, storage.ErrNotFound
	}
	return int64(len(data)), nil
}

func (s *mockStorage) UsedSpace(_ context.Context) (int64, error) {
	var total int64
	for _, data := range s.files {
		total += int64(len(data))
	}
	return total, nil
}

func (s *mockStorage) URL() string { return "mem://" }

func (s *mockStorage) Close() error { return nil }

// mockFetcher implements fetch.FetcherInterface for testing.
type mockFetcher struct {
	artifact    *fetch.Artifact
	fetchErr    error
	fetchCalled bool
	fetchedURL  string
}

func (f *mockFetcher) Fetch(ctx context.Context, url string) (*fetch.Artifact, error) {
	return f.FetchWithHeaders(ctx, url, nil)
}

func (f *mockFetcher) FetchWithHeaders(_ context.Context, url string, _ http.Header) (*fetch.Artifact, error) {
	f.fetchCalled = true
	f.fetchedURL = url
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	return f.artifact, nil
}

func (f *mockFetcher) Head(_ context.Context, _ string) (int64, string, error) {
	return 0, "", nil
}

// setupTestProxy creates a Proxy with a real DB (SQLite in temp dir) and mock storage/fetcher.
func setupTestProxy(t *testing.T) (*Proxy, *database.DB, *mockStorage, *mockFetcher) {
	t.Helper()

	dir := t.TempDir()
	db, err := database.Create(dir + "/test.db")
	if err != nil {
		t.Fatalf("failed to create test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store := newMockStorage()
	fetcher := &mockFetcher{}
	resolver := fetch.NewResolver()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	proxy := NewProxy(db, store, fetcher, resolver, logger)
	return proxy, db, store, fetcher
}

// seedPackage creates a package, version, and cached artifact in the test DB and storage.
func seedPackage(t *testing.T, db *database.DB, store *mockStorage, ecosystem, name, version, filename, content string) {
	t.Helper()

	pkg := &database.Package{
		PURL:      fmt.Sprintf("pkg:%s/%s", ecosystem, name),
		Ecosystem: ecosystem,
		Name:      name,
	}
	if err := db.UpsertPackage(pkg); err != nil {
		t.Fatalf("failed to upsert package: %v", err)
	}

	versionPURL := fmt.Sprintf("pkg:%s/%s@%s", ecosystem, name, version)
	ver := &database.Version{
		PURL:        versionPURL,
		PackagePURL: pkg.PURL,
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

// pathParseCase holds a single test case for path parsing functions that return
// (name, version, arch).
type pathParseCase struct {
	path        string
	wantName    string
	wantVersion string
	wantArch    string
}

// assertPathParser runs table-driven tests for a path parser function that returns
// three strings (name, version, arch).
func assertPathParser(t *testing.T, funcName string, parse func(string) (string, string, string), cases []pathParseCase) {
	t.Helper()
	for _, tt := range cases {
		t.Run(tt.path, func(t *testing.T) {
			name, version, arch := parse(tt.path)
			if name != tt.wantName {
				t.Errorf("%s() name = %q, want %q", funcName, name, tt.wantName)
			}
			if version != tt.wantVersion {
				t.Errorf("%s() version = %q, want %q", funcName, version, tt.wantVersion)
			}
			if arch != tt.wantArch {
				t.Errorf("%s() arch = %q, want %q", funcName, arch, tt.wantArch)
			}
		})
	}
}

// assertRoutesBasics checks that a handler's Routes() returns a non-nil handler,
// rejects POST requests with 405, and rejects path traversal with 400.
func assertRoutesBasics(t *testing.T, handler http.Handler, postPath, traversalPath string) {
	t.Helper()

	if handler == nil {
		t.Fatal("Routes() returned nil")
	}

	req := httptest.NewRequest(http.MethodPost, postPath, nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST request: got status %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}

	req = httptest.NewRequest(http.MethodGet, traversalPath, nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("path traversal: got status %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestGetOrFetchArtifact_CacheHit(t *testing.T) {
	proxy, db, store, fetcher := setupTestProxy(t)
	seedPackage(t, db, store, "npm", "lodash", "4.17.21", "lodash-4.17.21.tgz", "cached content")

	result, err := proxy.GetOrFetchArtifact(context.Background(), "npm", "lodash", "4.17.21", "lodash-4.17.21.tgz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = result.Reader.Close() }()

	if !result.Cached {
		t.Error("expected result to be cached")
	}
	if fetcher.fetchCalled {
		t.Error("fetcher should not be called on cache hit")
	}

	body, _ := io.ReadAll(result.Reader)
	if string(body) != "cached content" {
		t.Errorf("got body %q, want %q", body, "cached content")
	}
	if result.ContentType != "application/octet-stream" {
		t.Errorf("got content type %q, want %q", result.ContentType, "application/octet-stream")
	}
	if result.Hash != "abc123" {
		t.Errorf("got hash %q, want %q", result.Hash, "abc123")
	}
}

func TestGetOrFetchArtifact_CacheMiss_NoPackage(t *testing.T) {
	proxy, _, _, fetcher := setupTestProxy(t)

	// The resolver will fail because "nonexistent" isn't a real package,
	// but we're testing that it tries to fetch (doesn't return from cache).
	fetcher.fetchErr = errors.New("upstream unavailable")

	_, err := proxy.GetOrFetchArtifact(context.Background(), "npm", "nonexistent", "1.0.0", "nonexistent-1.0.0.tgz")
	if err == nil {
		t.Fatal("expected error for uncached package")
	}
}

func TestGetOrFetchArtifactFromURL_CacheMiss_StorageMissing(t *testing.T) {
	proxy, db, store, fetcher := setupTestProxy(t)

	// Seed DB but don't put the file in storage
	pkg := &database.Package{PURL: "pkg:npm/missing", Ecosystem: "npm", Name: "missing"}
	_ = db.UpsertPackage(pkg)
	ver := &database.Version{PURL: "pkg:npm/missing@1.0.0", PackagePURL: pkg.PURL}
	_ = db.UpsertVersion(ver)
	art := &database.Artifact{
		VersionPURL: ver.PURL,
		Filename:    "missing-1.0.0.tgz",
		UpstreamURL: "https://example.com/missing.tgz",
		StoragePath: sql.NullString{String: "nonexistent/path.tgz", Valid: true},
		ContentHash: sql.NullString{String: "hash", Valid: true},
		Size:        sql.NullInt64{Int64: 100, Valid: true},
		ContentType: sql.NullString{String: "application/octet-stream", Valid: true},
		FetchedAt:   sql.NullTime{Time: time.Now(), Valid: true},
	}
	_ = db.UpsertArtifact(art)

	// Storage doesn't have the file, so checkCache should return nil and trigger a refetch.
	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("refetched content")),
		ContentType: "application/gzip",
	}

	result, err := proxy.GetOrFetchArtifactFromURL(context.Background(), "npm", "missing", "1.0.0", "missing-1.0.0.tgz", "https://example.com/missing.tgz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = result.Reader.Close() }()

	if result.Cached {
		t.Error("expected cache miss (storage file was missing)")
	}
	if !fetcher.fetchCalled {
		t.Error("fetcher should be called when storage file is missing")
	}

	// Verify the new content was stored
	storagePath := storage.ArtifactPath("npm", "", "missing", "1.0.0", "missing-1.0.0.tgz")
	if _, ok := store.files[storagePath]; !ok {
		t.Error("refetched artifact should be stored")
	}
}

func TestGetOrFetchArtifactFromURL_CacheHit(t *testing.T) {
	proxy, db, store, fetcher := setupTestProxy(t)
	seedPackage(t, db, store, "pypi", "requests", "2.28.0", "requests-2.28.0.tar.gz", "pypi content")

	result, err := proxy.GetOrFetchArtifactFromURL(context.Background(), "pypi", "requests", "2.28.0", "requests-2.28.0.tar.gz", "https://pypi.org/files/requests-2.28.0.tar.gz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = result.Reader.Close() }()

	if !result.Cached {
		t.Error("expected cache hit")
	}
	if fetcher.fetchCalled {
		t.Error("fetcher should not be called on cache hit")
	}
}

func TestGetOrFetchArtifactFromURL_CacheMiss(t *testing.T) {
	proxy, _, store, fetcher := setupTestProxy(t)

	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("fetched content")),
		ContentType: "application/gzip",
	}

	result, err := proxy.GetOrFetchArtifactFromURL(context.Background(), "pypi", "newpkg", "1.0.0", "newpkg-1.0.0.tar.gz", "https://pypi.org/files/newpkg-1.0.0.tar.gz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = result.Reader.Close() }()

	if result.Cached {
		t.Error("expected cache miss")
	}
	if !fetcher.fetchCalled {
		t.Error("fetcher should be called on cache miss")
	}
	if fetcher.fetchedURL != "https://pypi.org/files/newpkg-1.0.0.tar.gz" {
		t.Errorf("fetcher called with wrong URL: %s", fetcher.fetchedURL)
	}

	body, _ := io.ReadAll(result.Reader)
	if string(body) != "fetched content" {
		t.Errorf("got body %q, want %q", body, "fetched content")
	}

	// Verify it was stored
	storagePath := storage.ArtifactPath("pypi", "", "newpkg", "1.0.0", "newpkg-1.0.0.tar.gz")
	if _, ok := store.files[storagePath]; !ok {
		t.Error("artifact was not stored in storage")
	}
}

func TestGetOrFetchArtifactFromURL_FetchError(t *testing.T) {
	proxy, _, _, fetcher := setupTestProxy(t)
	fetcher.fetchErr = errors.New("connection refused")

	_, err := proxy.GetOrFetchArtifactFromURL(context.Background(), "pypi", "fail", "1.0.0", "fail-1.0.0.tar.gz", "https://pypi.org/files/fail-1.0.0.tar.gz")
	if err == nil {
		t.Fatal("expected error on fetch failure")
	}
	if !strings.Contains(err.Error(), "fetching from upstream") {
		t.Errorf("expected upstream error, got: %v", err)
	}
}

func TestGetOrFetchArtifactFromURL_StoreError(t *testing.T) {
	proxy, _, store, fetcher := setupTestProxy(t)
	store.storeErr = errors.New("disk full")
	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("data")),
		ContentType: "application/gzip",
	}

	_, err := proxy.GetOrFetchArtifactFromURL(context.Background(), "pypi", "fail", "1.0.0", "fail-1.0.0.tar.gz", "https://pypi.org/files/fail.tar.gz")
	if err == nil {
		t.Fatal("expected error on store failure")
	}
	if !strings.Contains(err.Error(), "storing artifact") {
		t.Errorf("expected storage error, got: %v", err)
	}
}

func TestServeArtifact(t *testing.T) {
	result := &CacheResult{
		Reader:      io.NopCloser(strings.NewReader("file contents")),
		Size:        13,
		ContentType: "application/gzip",
		Hash:        "sha256abc",
		Cached:      true,
	}

	w := httptest.NewRecorder()
	ServeArtifact(w, result)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if w.Header().Get("Content-Type") != "application/gzip" {
		t.Errorf("Content-Type = %q, want %q", w.Header().Get("Content-Type"), "application/gzip")
	}
	if w.Header().Get("Content-Length") != "13" {
		t.Errorf("Content-Length = %q, want %q", w.Header().Get("Content-Length"), "13")
	}
	if w.Header().Get("ETag") != `"sha256abc"` {
		t.Errorf("ETag = %q, want %q", w.Header().Get("ETag"), `"sha256abc"`)
	}
	if w.Body.String() != "file contents" {
		t.Errorf("body = %q, want %q", w.Body.String(), "file contents")
	}
}

func TestServeArtifact_EmptyFields(t *testing.T) {
	result := &CacheResult{
		Reader: io.NopCloser(strings.NewReader("data")),
	}

	w := httptest.NewRecorder()
	ServeArtifact(w, result)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if w.Header().Get("Content-Type") != "" {
		t.Errorf("Content-Type should be empty, got %q", w.Header().Get("Content-Type"))
	}
	if w.Header().Get("Content-Length") != "" {
		t.Errorf("Content-Length should be empty, got %q", w.Header().Get("Content-Length"))
	}
	if w.Header().Get("ETag") != "" {
		t.Errorf("ETag should be empty, got %q", w.Header().Get("ETag"))
	}
}

func TestJSONError(t *testing.T) {
	tests := []struct {
		status  int
		message string
	}{
		{http.StatusBadRequest, "bad request"},
		{http.StatusNotFound, "not found"},
		{http.StatusInternalServerError, "internal error"},
	}

	for _, tt := range tests {
		w := httptest.NewRecorder()
		JSONError(w, tt.status, tt.message)

		if w.Code != tt.status {
			t.Errorf("status = %d, want %d", w.Code, tt.status)
		}
		if w.Header().Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q, want %q", w.Header().Get("Content-Type"), "application/json")
		}
		body := w.Body.String()
		if !strings.Contains(body, tt.message) {
			t.Errorf("body %q should contain %q", body, tt.message)
		}
	}
}

func TestNewProxy_NilLogger(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Create(dir + "/test.db")
	if err != nil {
		t.Fatalf("failed to create test database: %v", err)
	}
	defer func() { _ = db.Close() }()

	proxy := NewProxy(db, newMockStorage(), &mockFetcher{}, fetch.NewResolver(), nil)
	if proxy.Logger == nil {
		t.Error("Logger should be set to default when nil is passed")
	}
}

const testLastModified = "Wed, 01 Jan 2025 12:00:00 GMT"

// setupCachedProxy creates a Proxy with CacheMetadata enabled and an upstream
// test server that returns JSON with ETag and Last-Modified headers.
func setupCachedProxy(t *testing.T, upstreamETag, upstreamLastModified string) (*Proxy, *httptest.Server) {
	t.Helper()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if upstreamETag != "" {
			w.Header().Set("ETag", upstreamETag)
		}
		if upstreamLastModified != "" {
			w.Header().Set("Last-Modified", upstreamLastModified)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(upstream.Close)

	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.HTTPClient = upstream.Client()

	return proxy, upstream
}

func TestProxyCached_SetsETagAndLastModified(t *testing.T) {
	lm := testLastModified
	proxy, upstream := setupCachedProxy(t, `"abc123"`, lm)

	// First request populates the cache
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	proxy.ProxyCached(w, req, upstream.URL+"/test", "test-eco", "test-key")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("ETag"); got != `"abc123"` {
		t.Errorf("ETag = %q, want %q", got, `"abc123"`)
	}
	if got := w.Header().Get("Last-Modified"); got != lm {
		t.Errorf("Last-Modified = %q, want %q", got, lm)
	}
	if got := w.Header().Get("Content-Length"); got != "11" {
		t.Errorf("Content-Length = %q, want %q", got, "11")
	}
	if w.Body.String() != `{"ok":true}` {
		t.Errorf("body = %q, want %q", w.Body.String(), `{"ok":true}`)
	}
}

func TestProxyCached_IfNoneMatch_Returns304(t *testing.T) {
	proxy, upstream := setupCachedProxy(t, `"abc123"`, "")

	// Populate cache
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	proxy.ProxyCached(w, req, upstream.URL+"/test", "test-eco", "etag-key")
	if w.Code != http.StatusOK {
		t.Fatalf("initial request: status = %d, want 200", w.Code)
	}

	// Conditional request with matching ETag
	req = httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("If-None-Match", `"abc123"`)
	w = httptest.NewRecorder()
	proxy.ProxyCached(w, req, upstream.URL+"/test", "test-eco", "etag-key")

	if w.Code != http.StatusNotModified {
		t.Errorf("conditional request: status = %d, want 304", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("304 response should have empty body, got %d bytes", w.Body.Len())
	}
}

func TestProxyCached_IfNoneMatch_NonMatching_Returns200(t *testing.T) {
	proxy, upstream := setupCachedProxy(t, `"abc123"`, "")

	// Populate cache
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	proxy.ProxyCached(w, req, upstream.URL+"/test", "test-eco", "etag-nm-key")
	if w.Code != http.StatusOK {
		t.Fatalf("initial request: status = %d, want 200", w.Code)
	}

	// Conditional request with non-matching ETag
	req = httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("If-None-Match", `"different"`)
	w = httptest.NewRecorder()
	proxy.ProxyCached(w, req, upstream.URL+"/test", "test-eco", "etag-nm-key")

	if w.Code != http.StatusOK {
		t.Errorf("non-matching ETag: status = %d, want 200", w.Code)
	}
}

func TestProxyCached_IfModifiedSince_Returns304(t *testing.T) {
	lm := testLastModified
	proxy, upstream := setupCachedProxy(t, "", lm)

	// Populate cache
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	proxy.ProxyCached(w, req, upstream.URL+"/test", "test-eco", "lm-key")
	if w.Code != http.StatusOK {
		t.Fatalf("initial request: status = %d, want 200", w.Code)
	}

	// Conditional request with If-Modified-Since equal to Last-Modified
	req = httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("If-Modified-Since", lm)
	w = httptest.NewRecorder()
	proxy.ProxyCached(w, req, upstream.URL+"/test", "test-eco", "lm-key")

	if w.Code != http.StatusNotModified {
		t.Errorf("conditional request: status = %d, want 304", w.Code)
	}
}

func TestProxyCached_IfModifiedSince_OlderDate_Returns200(t *testing.T) {
	lm := testLastModified
	proxy, upstream := setupCachedProxy(t, "", lm)

	// Populate cache
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	proxy.ProxyCached(w, req, upstream.URL+"/test", "test-eco", "lm-old-key")
	if w.Code != http.StatusOK {
		t.Fatalf("initial request: status = %d, want 200", w.Code)
	}

	// Conditional request with If-Modified-Since older than Last-Modified
	req = httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("If-Modified-Since", "Mon, 01 Dec 2024 12:00:00 GMT")
	w = httptest.NewRecorder()
	proxy.ProxyCached(w, req, upstream.URL+"/test", "test-eco", "lm-old-key")

	if w.Code != http.StatusOK {
		t.Errorf("older If-Modified-Since: status = %d, want 200", w.Code)
	}
}

func TestProxyCached_NoValidators_OmitsHeaders(t *testing.T) {
	proxy, upstream := setupCachedProxy(t, "", "")

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	proxy.ProxyCached(w, req, upstream.URL+"/test", "test-eco", "no-val-key")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("ETag"); got != "" {
		t.Errorf("ETag should be empty when upstream has none, got %q", got)
	}
	if got := w.Header().Get("Last-Modified"); got != "" {
		t.Errorf("Last-Modified should be empty when upstream has none, got %q", got)
	}
}
