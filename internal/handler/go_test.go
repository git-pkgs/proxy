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

func TestGoModulesDifferingInCaseAreCachedApart(t *testing.T) {
	proxy, _, _, fetcher := setupTestProxy(t)
	handler := NewGoHandler(proxy, "http://localhost:8080")
	get := func(path string) string {
		t.Helper()
		resp := httptest.NewRecorder()
		handler.Routes().ServeHTTP(resp, httptest.NewRequest(http.MethodGet, path, nil))
		if resp.Code != http.StatusOK {
			t.Fatalf("GET %s: status = %d", path, resp.Code)
		}
		return resp.Body.String()
	}

	fetcher.artifact = artifactBody("upper")
	get("/github.com/!sirupsen/logrus/@v/v1.0.6.zip")
	fetcher.artifact = artifactBody("lower")
	if got := get("/github.com/sirupsen/logrus/@v/v1.0.6.zip"); got != "lower" {
		t.Fatalf("lowercase module served %q", got)
	}
	if got := get("/github.com/!sirupsen/logrus/@v/v1.0.6.zip"); got != "upper" {
		t.Fatalf("capitalised module served %q", got)
	}
}

func TestGoZipFilename(t *testing.T) {
	tests := []struct {
		module string
		want   string
	}{
		{"github.com/sirupsen/logrus", "logrus@v1.0.6.zip"},
		{"github.com/Sirupsen/logrus", "github.com_!sirupsen_logrus@v1.0.6.zip"},
		{"github.com/SIRUPSEN/logrus", "github.com_!s!i!r!u!p!s!e!n_logrus@v1.0.6.zip"},
	}
	for _, tt := range tests {
		if got := goZipFilename(tt.module, "v1.0.6"); got != tt.want {
			t.Errorf("goZipFilename(%q) = %q, want %q", tt.module, got, tt.want)
		}
	}
}
