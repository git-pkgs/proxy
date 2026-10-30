package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/git-pkgs/proxy/internal/database"
)

const swiftWidgetArchive = "acme widget archive"

// swiftMirrorUpstream serves acme.widget 1.0.0 the way a Swift package
// registry does, with https://github.com/acme/widget as its source
// repository, and counts archive downloads and identifier lookups.
func swiftMirrorUpstream(t *testing.T) (srv *httptest.Server, downloads, lookups *atomic.Int64) {
	t.Helper()
	downloads, lookups = new(atomic.Int64), new(atomic.Int64)
	sum := sha256.Sum256([]byte(swiftWidgetArchive))
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Version", "1")
		switch r.URL.Path {
		case "/identifiers":
			lookups.Add(1)
			if r.URL.Query().Get("url") != "https://github.com/acme/widget" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"identifiers":["acme.widget"]}`)
		case "/acme/widget":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"releases":{"1.0.0":{}}}`)
		case "/acme/widget/1.0.0/Package.swift":
			w.Header().Set("Content-Type", "text/x-swift")
			_, _ = io.WriteString(w, "// swift-tools-version:5.9\n")
		case "/acme/widget/1.0.0":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"id":"acme.widget","version":"1.0.0","resources":[{"name":"source-archive","type":"application/zip","checksum":%q}]}`, hex.EncodeToString(sum[:]))
		case "/acme/widget/1.0.0.zip":
			downloads.Add(1)
			w.Header().Set("Content-Type", "application/zip")
			_, _ = io.WriteString(w, swiftWidgetArchive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, downloads, lookups
}

// writeSwiftMirrorConfig writes a config using upstream as the Swift
// registry, denying the denied package PURLs.
func writeSwiftMirrorConfig(t *testing.T, dir, upstream string, denied ...string) string {
	t.Helper()
	cfg := "storage:\n" +
		"  url: file://" + filepath.Join(dir, "artifacts") + "\n" +
		"database:\n" +
		"  path: " + filepath.Join(dir, "proxy.db") + "\n" +
		"upstream:\n" +
		"  swift: " + upstream + "\n" +
		"  allow_loopback: true\n"
	if len(denied) > 0 {
		cfg += "denylist:\n  packages:\n"
		for _, p := range denied {
			cfg += "    - " + p + "\n"
		}
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

func cachedArtifacts(t *testing.T, dir string) int64 {
	t.Helper()
	db, err := database.Open(filepath.Join(dir, "proxy.db"))
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	defer func() { _ = db.Close() }()
	stats, err := db.GetCacheStats()
	if err != nil {
		t.Fatalf("GetCacheStats: %v", err)
	}
	return stats.TotalArtifacts
}

// Without a denylist the fake registry mirrors the release, so the tests
// below fail for the denylist and not for the setup.
func TestMirrorSwiftRelease(t *testing.T) {
	upstream, downloads, _ := swiftMirrorUpstream(t)
	dir := t.TempDir()
	configPath := writeSwiftMirrorConfig(t, dir, upstream.URL)

	output := runMirrorProcess(t, 0, "-config", configPath, "pkg:swift/acme/widget@1.0.0")

	if !strings.Contains(output, "1 downloaded") {
		t.Errorf("output:\n%s", output)
	}
	if n := downloads.Load(); n != 1 {
		t.Errorf("archive downloads = %d, want 1", n)
	}
	if n := cachedArtifacts(t, dir); n != 1 {
		t.Errorf("cached artifacts = %d, want 1", n)
	}
}

// A denylist entry naming the package by its source repository blocks the
// mirror download of the registry identity it resolves to.
func TestMirrorDenylistSwiftSourceEntry(t *testing.T) {
	upstream, downloads, _ := swiftMirrorUpstream(t)
	dir := t.TempDir()
	configPath := writeSwiftMirrorConfig(t, dir, upstream.URL, "pkg:swift/github.com/acme/widget@1.0.0")

	output := runMirrorProcess(t, 1, "-config", configPath, "pkg:swift/acme/widget@1.0.0")

	if !strings.Contains(output, "0 downloaded") || !strings.Contains(output, "1 failed") {
		t.Errorf("output:\n%s", output)
	}
	if n := downloads.Load(); n != 0 {
		t.Errorf("archive downloads = %d, want 0", n)
	}
	if n := cachedArtifacts(t, dir); n != 0 {
		t.Errorf("cached artifacts = %d, want 0", n)
	}
}

// A source repository entry the registry can't resolve stops the mirror
// before it downloads anything, as it stops serve: left out, it would let
// the version through.
func TestMirrorDenylistUnresolvedSwiftSource(t *testing.T) {
	upstream, downloads, lookups := swiftMirrorUpstream(t)
	dir := t.TempDir()
	configPath := writeSwiftMirrorConfig(t, dir, upstream.URL, "pkg:swift/github.com/acme/unknown@1.0.0")

	output := runMirrorProcess(t, 1, "-config", configPath, "pkg:swift/acme/widget@1.0.0")

	if !strings.Contains(output, "configuring denylist") || !strings.Contains(output, "github.com/acme/unknown") {
		t.Errorf("output does not report the unresolved entry:\n%s", output)
	}
	if strings.Contains(output, "Mirror complete") {
		t.Errorf("mirror ran:\n%s", output)
	}
	if lookups.Load() == 0 {
		t.Error("the entry was not looked up")
	}
	if n := downloads.Load(); n != 0 {
		t.Errorf("archive downloads = %d, want 0", n)
	}
}
