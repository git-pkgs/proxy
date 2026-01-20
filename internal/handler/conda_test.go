package handler

import (
	"log/slog"
	"testing"
)

func TestCondaParseFilename(t *testing.T) {
	h := &CondaHandler{proxy: &Proxy{Logger: slog.Default()}}

	tests := []struct {
		filename    string
		wantName    string
		wantVersion string
	}{
		{"numpy-1.24.0-py311h64a7726_0.conda", "numpy", "1.24.0"},
		{"scipy-1.11.4-py310h64a7726_0.tar.bz2", "scipy", "1.11.4"},
		{"python-dateutil-2.8.2-pyhd8ed1ab_0.conda", "python-dateutil", "2.8.2"},
		{"ca-certificates-2023.11.17-hbcca054_0.conda", "ca-certificates", "2023.11.17"},
		{"invalid", "", ""},
	}

	for _, tt := range tests {
		name, version := h.parseFilename(tt.filename)
		if name != tt.wantName || version != tt.wantVersion {
			t.Errorf("parseFilename(%q) = (%q, %q), want (%q, %q)",
				tt.filename, name, version, tt.wantName, tt.wantVersion)
		}
	}
}

func TestCondaIsPackageFile(t *testing.T) {
	h := &CondaHandler{}

	tests := []struct {
		filename string
		want     bool
	}{
		{"numpy-1.24.0-py311h64a7726_0.conda", true},
		{"scipy-1.11.4-py310h64a7726_0.tar.bz2", true},
		{"repodata.json", false},
		{"repodata.json.bz2", false},
	}

	for _, tt := range tests {
		got := h.isPackageFile(tt.filename)
		if got != tt.want {
			t.Errorf("isPackageFile(%q) = %v, want %v", tt.filename, got, tt.want)
		}
	}
}
