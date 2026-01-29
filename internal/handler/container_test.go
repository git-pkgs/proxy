package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
			path:       "invalid/path",
			wantName:   "",
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
