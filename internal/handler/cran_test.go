package handler

import (
	"log/slog"
	"testing"
)

func TestCRANParseSourceFilename(t *testing.T) {
	h := &CRANHandler{proxy: &Proxy{Logger: slog.Default()}}

	tests := []struct {
		filename    string
		wantName    string
		wantVersion string
	}{
		{"ggplot2_3.4.4.tar.gz", "ggplot2", "3.4.4"},
		{"data.table_1.14.8.tar.gz", "data.table", "1.14.8"},
		{"Rcpp_1.0.11.tar.gz", "Rcpp", "1.0.11"},
		{"invalid.tar.gz", "", ""},
	}

	for _, tt := range tests {
		name, version := h.parseSourceFilename(tt.filename)
		if name != tt.wantName || version != tt.wantVersion {
			t.Errorf("parseSourceFilename(%q) = (%q, %q), want (%q, %q)",
				tt.filename, name, version, tt.wantName, tt.wantVersion)
		}
	}
}

func TestCRANParseBinaryFilename(t *testing.T) {
	h := &CRANHandler{proxy: &Proxy{Logger: slog.Default()}}

	tests := []struct {
		filename    string
		wantName    string
		wantVersion string
	}{
		{"ggplot2_3.4.4.zip", "ggplot2", "3.4.4"},
		{"ggplot2_3.4.4.tgz", "ggplot2", "3.4.4"},
		{"data.table_1.14.8.zip", "data.table", "1.14.8"},
		{"invalid.zip", "", ""},
	}

	for _, tt := range tests {
		name, version := h.parseBinaryFilename(tt.filename)
		if name != tt.wantName || version != tt.wantVersion {
			t.Errorf("parseBinaryFilename(%q) = (%q, %q), want (%q, %q)",
				tt.filename, name, version, tt.wantName, tt.wantVersion)
		}
	}
}
