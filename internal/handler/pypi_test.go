package handler

import (
	"log/slog"
	"testing"
)

func TestPyPIParseFilename(t *testing.T) {
	h := &PyPIHandler{proxy: &Proxy{Logger: slog.Default()}}

	tests := []struct {
		filename    string
		wantName    string
		wantVersion string
	}{
		// Sdist formats
		{"requests-2.31.0.tar.gz", "requests", "2.31.0"},
		{"Django-4.2.7.tar.gz", "Django", "4.2.7"},
		{"aws-sdk-1.0.0.tar.gz", "aws-sdk", "1.0.0"},
		{"zipp-3.17.0.zip", "zipp", "3.17.0"},

		// Wheel formats
		{"requests-2.31.0-py3-none-any.whl", "requests", "2.31.0"},
		{"numpy-1.26.2-cp311-cp311-manylinux_2_17_x86_64.whl", "numpy", "1.26.2"},
		{"cryptography-41.0.5-cp37-abi3-manylinux_2_28_x86_64.whl", "cryptography", "41.0.5"},

		// Invalid
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

func TestIsPythonTag(t *testing.T) {
	tests := []struct {
		tag  string
		want bool
	}{
		{"py3", true},
		{"py2", true},
		{"cp311", true},
		{"cp37", true},
		{"pp39", true},
		{"none", false},
		{"any", false},
		{"manylinux", false},
	}

	for _, tt := range tests {
		got := isPythonTag(tt.tag)
		if got != tt.want {
			t.Errorf("isPythonTag(%q) = %v, want %v", tt.tag, got, tt.want)
		}
	}
}
