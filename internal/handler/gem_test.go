package handler

import (
	"log/slog"
	"testing"
)

func TestGemParseFilename(t *testing.T) {
	h := &GemHandler{proxy: &Proxy{Logger: slog.Default()}}

	tests := []struct {
		filename    string
		wantName    string
		wantVersion string
	}{
		{"rails-7.1.0.gem", "rails", "7.1.0"},
		{"aws-sdk-s3-1.142.0.gem", "aws-sdk-s3", "1.142.0"},
		{"nokogiri-1.15.4-x86_64-linux.gem", "nokogiri", "1.15.4-x86_64-linux"},
		{"activerecord-7.0.8.gem", "activerecord", "7.0.8"},
		{"invalid", "", ""},
	}

	for _, tt := range tests {
		name, version := h.parseGemFilename(tt.filename)
		if name != tt.wantName || version != tt.wantVersion {
			t.Errorf("parseGemFilename(%q) = (%q, %q), want (%q, %q)",
				tt.filename, name, version, tt.wantName, tt.wantVersion)
		}
	}
}
