package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDebianHandler_parsePoolPath(t *testing.T) {
	h := &DebianHandler{}

	tests := []struct {
		path        string
		wantName    string
		wantVersion string
		wantArch    string
	}{
		{
			path:        "pool/main/n/nginx/nginx_1.18.0-6_amd64.deb",
			wantName:    "nginx",
			wantVersion: "1.18.0-6",
			wantArch:    "amd64",
		},
		{
			path:        "pool/main/libn/libncurses/libncurses6_6.2-1_amd64.deb",
			wantName:    "libncurses6",
			wantVersion: "6.2-1",
			wantArch:    "amd64",
		},
		{
			path:        "pool/contrib/v/virtualbox/virtualbox_6.1.38-1_amd64.deb",
			wantName:    "virtualbox",
			wantVersion: "6.1.38-1",
			wantArch:    "amd64",
		},
		{
			path:        "pool/main/g/git/git_2.39.2-1_arm64.deb",
			wantName:    "git",
			wantVersion: "2.39.2-1",
			wantArch:    "arm64",
		},
		{
			path:        "invalid/path",
			wantName:    "",
			wantVersion: "",
			wantArch:    "",
		},
		{
			path:        "pool/main/n/nginx/nginx.deb",
			wantName:    "",
			wantVersion: "",
			wantArch:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			name, version, arch := h.parsePoolPath(tt.path)
			if name != tt.wantName {
				t.Errorf("parsePoolPath() name = %q, want %q", name, tt.wantName)
			}
			if version != tt.wantVersion {
				t.Errorf("parsePoolPath() version = %q, want %q", version, tt.wantVersion)
			}
			if arch != tt.wantArch {
				t.Errorf("parsePoolPath() arch = %q, want %q", arch, tt.wantArch)
			}
		})
	}
}

func TestDebianHandler_Routes(t *testing.T) {
	h := NewDebianHandler(nil, "http://localhost:8080")

	// Test that handler doesn't panic on initialization
	handler := h.Routes()
	if handler == nil {
		t.Fatal("Routes() returned nil")
	}

	// Test method not allowed
	req := httptest.NewRequest(http.MethodPost, "/dists/stable/Release", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST request: got status %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}
