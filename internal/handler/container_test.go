package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/registries/fetch"
)

func TestContainerHandler_parseBlobPath(t *testing.T) {
	h := &ContainerHandler{}

	tests := []struct {
		path       string
		wantName   string
		wantDigest string
	}{
		{
			path:       "library/nginx/blobs/sha256:abc123def456",
			wantName:   "library/nginx",
			wantDigest: "sha256:abc123def456",
		},
		{
			path:       "myorg/myrepo/blobs/sha256:0123456789abcdef",
			wantName:   "myorg/myrepo",
			wantDigest: "sha256:0123456789abcdef",
		},
		{
			path:       "deep/nested/repo/name/blobs/sha256:fedcba9876543210",
			wantName:   "deep/nested/repo/name",
			wantDigest: "sha256:fedcba9876543210",
		},
		{
			path:       "invalid/path",
			wantName:   "",
			wantDigest: "",
		},
		{
			path:       "repo/blobs/md5:invalid",
			wantName:   "",
			wantDigest: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			name, digest := h.parseBlobPath(tt.path)
			if name != tt.wantName {
				t.Errorf("parseBlobPath() name = %q, want %q", name, tt.wantName)
			}
			if digest != tt.wantDigest {
				t.Errorf("parseBlobPath() digest = %q, want %q", digest, tt.wantDigest)
			}
		})
	}
}

func TestContainerHandler_parseManifestPath(t *testing.T) {
	h := &ContainerHandler{}

	tests := []struct {
		path          string
		wantName      string
		wantReference string
	}{
		{
			path:          "library/nginx/manifests/latest",
			wantName:      "library/nginx",
			wantReference: "latest",
		},
		{
			path:          "myorg/myrepo/manifests/v1.0.0",
			wantName:      "myorg/myrepo",
			wantReference: "v1.0.0",
		},
		{
			path:          "repo/manifests/sha256:abc123",
			wantName:      "repo",
			wantReference: "sha256:abc123",
		},
		{
			path:     "invalid/path",
			wantName: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			name, ref := h.parseManifestPath(tt.path)
			if name != tt.wantName {
				t.Errorf("parseManifestPath() name = %q, want %q", name, tt.wantName)
			}
			if ref != tt.wantReference {
				t.Errorf("parseManifestPath() reference = %q, want %q", ref, tt.wantReference)
			}
		})
	}
}

func TestContainerHandler_parseTagsListPath(t *testing.T) {
	h := &ContainerHandler{}

	tests := []struct {
		path     string
		wantName string
	}{
		{
			path:     "library/nginx/tags/list",
			wantName: "library/nginx",
		},
		{
			path:     "myorg/myrepo/tags/list",
			wantName: "myorg/myrepo",
		},
		{
			path:     "invalid/path",
			wantName: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			name := h.parseTagsListPath(tt.path)
			if name != tt.wantName {
				t.Errorf("parseTagsListPath() = %q, want %q", name, tt.wantName)
			}
		})
	}
}

func TestContainerHandler_BlobDownload_CachesWithAuth(t *testing.T) {
	// Set up a mock auth server that returns a token
	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "test-token-123"})
	}))
	defer authServer.Close()

	// Set up mock fetcher that captures headers
	var capturedHeaders http.Header
	mf := &mockFetcherWithHeaders{
		fetchFn: func(_ context.Context, _ string, headers http.Header) (*fetch.Artifact, error) {
			capturedHeaders = headers
			return &fetch.Artifact{
				Body:        io.NopCloser(bytes.NewReader([]byte("blob-content"))),
				Size:        12,
				ContentType: "application/octet-stream",
			}, nil
		},
	}

	dir := t.TempDir()
	db, err := database.Create(dir + "/test.db")
	if err != nil {
		t.Fatalf("failed to create test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store := newMockStorage()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := &Proxy{
		DB:         db,
		Storage:    store,
		Fetcher:    mf,
		Logger:     logger,
		HTTPClient: &http.Client{},
	}

	h := &ContainerHandler{
		proxy:       proxy,
		registryURL: "https://registry-1.docker.io",
		authURL:     authServer.URL,
		proxyURL:    "http://localhost:8080",
	}

	handler := h.Routes()
	req := httptest.NewRequest(http.MethodGet, "/library/nginx/blobs/sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abcd", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Verify auth header was passed to the fetcher
	if capturedHeaders == nil {
		t.Fatal("expected headers to be passed to fetcher, got nil")
	}
	auth := capturedHeaders.Get("Authorization")
	if auth != "Bearer test-token-123" {
		t.Errorf("Authorization = %q, want %q", auth, "Bearer test-token-123")
	}

	// Verify response headers
	if got := w.Header().Get("Docker-Content-Digest"); got != "sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abcd" {
		t.Errorf("Docker-Content-Digest = %q, want digest", got)
	}
}

// mockFetcherWithHeaders captures headers passed to FetchWithHeaders.
type mockFetcherWithHeaders struct {
	fetchFn func(ctx context.Context, url string, headers http.Header) (*fetch.Artifact, error)
}

func (f *mockFetcherWithHeaders) Fetch(ctx context.Context, url string) (*fetch.Artifact, error) {
	return f.FetchWithHeaders(ctx, url, nil)
}

func (f *mockFetcherWithHeaders) FetchWithHeaders(ctx context.Context, url string, headers http.Header) (*fetch.Artifact, error) {
	return f.fetchFn(ctx, url, headers)
}

func (f *mockFetcherWithHeaders) Head(_ context.Context, _ string) (int64, string, error) {
	return 0, "", nil
}

func TestContainerHandler_Routes_VersionCheck(t *testing.T) {
	h := NewContainerHandler(nil, "http://localhost:8080")

	handler := h.Routes()
	if handler == nil {
		t.Fatal("Routes() returned nil")
	}

	// Test /v2/ version check endpoint
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("version check: got status %d, want %d", w.Code, http.StatusOK)
	}

	if got := w.Header().Get("Docker-Distribution-Api-Version"); got != "registry/2.0" {
		t.Errorf("Docker-Distribution-Api-Version = %q, want %q", got, "registry/2.0")
	}
}
