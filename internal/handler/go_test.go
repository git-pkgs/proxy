package handler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/git-pkgs/registries/fetch"
)

func TestGoModuleDownloadUpstreamErrors(t *testing.T) {
	tests := []struct {
		name       string
		fetchErr   error
		wantStatus int
	}{
		{
			name:       "module not found",
			fetchErr:   fetch.ErrNotFound,
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "upstream failure",
			fetchErr:   errors.New("connection refused"),
			wantStatus: http.StatusBadGateway,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxy, _, _, fetcher := setupTestProxy(t)
			fetcher.fetchErr = tt.fetchErr
			handler := NewGoHandler(proxy, "http://localhost:8080")

			req := httptest.NewRequest(http.MethodGet, "/example.com/mod/@v/v1.0.0.zip", nil)
			resp := httptest.NewRecorder()
			handler.Routes().ServeHTTP(resp, req)

			if resp.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.Code, tt.wantStatus)
			}
		})
	}
}

func TestDecodeGoModule(t *testing.T) {
	tests := []struct {
		encoded string
		want    string
	}{
		{"github.com/user/repo", "github.com/user/repo"},
		{"github.com/!user/!repo", "github.com/User/Repo"},
		{"golang.org/x/text", "golang.org/x/text"},
		{"!azure!s!d!k", "AzureSDK"},
	}

	for _, tt := range tests {
		got := decodeGoModule(tt.encoded)
		if got != tt.want {
			t.Errorf("decodeGoModule(%q) = %q, want %q", tt.encoded, got, tt.want)
		}
	}
}

func TestLastComponent(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"github.com/user/repo", "repo"},
		{"golang.org/x/text", "text"},
		{"simple", "simple"},
	}

	for _, tt := range tests {
		got := lastComponent(tt.path)
		if got != tt.want {
			t.Errorf("lastComponent(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}
