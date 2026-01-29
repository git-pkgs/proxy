package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRPMHandler_parseRPMPath(t *testing.T) {
	h := &RPMHandler{}

	tests := []struct {
		path        string
		wantName    string
		wantVersion string
		wantArch    string
	}{
		{
			path:        "releases/39/Everything/x86_64/os/Packages/n/nginx-1.24.0-1.fc39.x86_64.rpm",
			wantName:    "nginx",
			wantVersion: "1.24.0-1.fc39",
			wantArch:    "x86_64",
		},
		{
			path:        "Packages/kernel-core-6.5.5-200.fc38.x86_64.rpm",
			wantName:    "kernel-core",
			wantVersion: "6.5.5-200.fc38",
			wantArch:    "x86_64",
		},
		{
			path:        "updates/39/Everything/aarch64/Packages/g/git-2.42.0-1.fc39.aarch64.rpm",
			wantName:    "git",
			wantVersion: "2.42.0-1.fc39",
			wantArch:    "aarch64",
		},
		{
			path:        "vim-enhanced-9.0.1000-1.fc38.noarch.rpm",
			wantName:    "vim-enhanced",
			wantVersion: "9.0.1000-1.fc38",
			wantArch:    "noarch",
		},
		{
			path:        "invalid.rpm",
			wantName:    "",
			wantVersion: "",
			wantArch:    "",
		},
		{
			path:        "not-an-rpm-file",
			wantName:    "",
			wantVersion: "",
			wantArch:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			name, version, arch := h.parseRPMPath(tt.path)
			if name != tt.wantName {
				t.Errorf("parseRPMPath() name = %q, want %q", name, tt.wantName)
			}
			if version != tt.wantVersion {
				t.Errorf("parseRPMPath() version = %q, want %q", version, tt.wantVersion)
			}
			if arch != tt.wantArch {
				t.Errorf("parseRPMPath() arch = %q, want %q", arch, tt.wantArch)
			}
		})
	}
}

func TestRPMHandler_Routes(t *testing.T) {
	h := NewRPMHandler(nil, "http://localhost:8080")

	// Test that handler doesn't panic on initialization
	handler := h.Routes()
	if handler == nil {
		t.Fatal("Routes() returned nil")
	}

	// Test method not allowed
	req := httptest.NewRequest(http.MethodPost, "/repodata/repomd.xml", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST request: got status %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}
