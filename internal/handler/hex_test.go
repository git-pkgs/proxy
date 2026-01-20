package handler

import (
	"log/slog"
	"testing"
)

func TestHexParseTarballFilename(t *testing.T) {
	h := &HexHandler{proxy: &Proxy{Logger: slog.Default()}}

	tests := []struct {
		filename    string
		wantName    string
		wantVersion string
	}{
		{"phoenix-1.7.10.tar", "phoenix", "1.7.10"},
		{"ecto-3.11.0.tar", "ecto", "3.11.0"},
		{"phoenix_live_view-0.20.1.tar", "phoenix_live_view", "0.20.1"},
		{"invalid", "", ""},
	}

	for _, tt := range tests {
		name, version := h.parseTarballFilename(tt.filename)
		if name != tt.wantName || version != tt.wantVersion {
			t.Errorf("parseTarballFilename(%q) = (%q, %q), want (%q, %q)",
				tt.filename, name, version, tt.wantName, tt.wantVersion)
		}
	}
}
