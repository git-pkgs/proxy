package mirror

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/git-pkgs/registries"
)

func TestSwiftIdentity(t *testing.T) {
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/identifiers" || r.Header.Get("Accept") != "application/vnd.swift.registry.v1+json" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		switch r.URL.Query().Get("url") {
		case "https://github.com/apple/swift-nio":
			_, _ = w.Write([]byte(`{"identifiers":["apple.swift-nio","mirror.swift-nio"]}`))
		case "https://github.com/acme/empty":
			_, _ = w.Write([]byte(`{"identifiers":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer registry.Close()

	for name, want := range map[string]string{
		"apple/swift-nio":            "apple.swift-nio",
		"github.com/apple/swift-nio": "apple.swift-nio",
		"github.com/acme/empty":      "",
		"github.com/acme/unknown":    "",
		"swift-nio":                  "",
		"apple/":                     "",
		"apple/swift-nio/extra":      "",
	} {
		got, err := swiftIdentity(context.Background(), registries.DefaultClient(), registry.URL, name)
		if got != want || (want == "") != (err != nil) {
			t.Errorf("swiftIdentity(%q) = %q, %v; want %q", name, got, err, want)
		}
	}
}
