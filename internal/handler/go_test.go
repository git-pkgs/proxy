package handler

import (
	"testing"
)

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
