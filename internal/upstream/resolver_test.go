package upstream

import (
	"context"
	"testing"
)

func TestResolveWithoutRegistry(t *testing.T) {
	r := NewResolver()

	tests := []struct {
		ecosystem    string
		name         string
		version      string
		wantURL      string
		wantFilename string
	}{
		{
			ecosystem:    "npm",
			name:         "lodash",
			version:      "4.17.21",
			wantURL:      "https://registry.npmjs.org/lodash/-/lodash-4.17.21.tgz",
			wantFilename: "lodash-4.17.21.tgz",
		},
		{
			ecosystem:    "npm",
			name:         "@babel/core",
			version:      "7.23.0",
			wantURL:      "https://registry.npmjs.org/@babel/core/-/core-7.23.0.tgz",
			wantFilename: "core-7.23.0.tgz",
		},
		{
			ecosystem:    "cargo",
			name:         "serde",
			version:      "1.0.193",
			wantURL:      "https://static.crates.io/crates/serde/serde-1.0.193.crate",
			wantFilename: "serde-1.0.193.crate",
		},
		{
			ecosystem:    "gem",
			name:         "rails",
			version:      "7.1.2",
			wantURL:      "https://rubygems.org/downloads/rails-7.1.2.gem",
			wantFilename: "rails-7.1.2.gem",
		},
		{
			ecosystem:    "golang",
			name:         "github.com/stretchr/testify",
			version:      "v1.8.4",
			wantURL:      "https://proxy.golang.org/github.com/stretchr/testify/@v/v1.8.4.zip",
			wantFilename: "testify@v1.8.4.zip",
		},
		{
			ecosystem:    "golang",
			name:         "github.com/Azure/azure-sdk-for-go",
			version:      "v68.0.0",
			wantURL:      "https://proxy.golang.org/github.com/!azure/azure-sdk-for-go/@v/v68.0.0.zip",
			wantFilename: "azure-sdk-for-go@v68.0.0.zip",
		},
		{
			ecosystem:    "hex",
			name:         "phoenix",
			version:      "1.7.10",
			wantURL:      "https://repo.hex.pm/tarballs/phoenix-1.7.10.tar",
			wantFilename: "phoenix-1.7.10.tar",
		},
		{
			ecosystem:    "pub",
			name:         "flutter",
			version:      "3.16.0",
			wantURL:      "https://pub.dev/packages/flutter/versions/3.16.0.tar.gz",
			wantFilename: "flutter-3.16.0.tar.gz",
		},
	}

	for _, tt := range tests {
		t.Run(tt.ecosystem+"/"+tt.name, func(t *testing.T) {
			info, err := r.Resolve(context.Background(), tt.ecosystem, tt.name, tt.version)
			if err != nil {
				t.Fatalf("Resolve failed: %v", err)
			}

			if info.URL != tt.wantURL {
				t.Errorf("URL = %q, want %q", info.URL, tt.wantURL)
			}
			if info.Filename != tt.wantFilename {
				t.Errorf("Filename = %q, want %q", info.Filename, tt.wantFilename)
			}
		})
	}
}

func TestResolveUnsupportedEcosystem(t *testing.T) {
	r := NewResolver()

	_, err := r.Resolve(context.Background(), "unknown", "pkg", "1.0.0")
	if err == nil {
		t.Error("expected error for unsupported ecosystem")
	}
}

func TestEncodeGoModule(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"github.com/user/repo", "github.com/user/repo"},
		{"github.com/Azure/azure-sdk", "github.com/!azure/azure-sdk"},
		{"github.com/BurntSushi/toml", "github.com/!burnt!sushi/toml"},
		{"golang.org/x/net", "golang.org/x/net"},
	}

	for _, tt := range tests {
		got := encodeGoModule(tt.input)
		if got != tt.want {
			t.Errorf("encodeGoModule(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFilenameFromURL(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://example.com/path/to/file.tar.gz", "file.tar.gz"},
		{"https://example.com/file.zip", "file.zip"},
		{"file.txt", "file.txt"},
	}

	for _, tt := range tests {
		got := filenameFromURL(tt.url)
		if got != tt.want {
			t.Errorf("filenameFromURL(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}
