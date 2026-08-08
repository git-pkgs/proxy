package handler

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/registries/fetch"
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

		// Additional sdist archive formats
		{"lxml-4.9.3.tar.xz", "lxml", "4.9.3"},
		{"docutils-0.20.1.tgz", "docutils", "0.20.1"},
		{"psycopg2-2.9.9.tar.bz2", "psycopg2", "2.9.9"},

		// Wheel formats
		{"requests-2.31.0-py3-none-any.whl", "requests", "2.31.0"},
		{"numpy-1.26.2-cp311-cp311-manylinux_2_17_x86_64.whl", "numpy", "1.26.2"},
		{"cryptography-41.0.5-cp37-abi3-manylinux_2_28_x86_64.whl", "cryptography", "41.0.5"},

		// Wheels with a build tag must not fold the tag into the version
		{"foo-1.0-1-py3-none-any.whl", "foo", "1.0"},
		{"tensorflow-2.15.0-2-cp311-cp311-manylinux_2_17_x86_64.whl", "tensorflow", "2.15.0"},

		// PEP 658 core-metadata sidecars resolve to the distribution they describe
		{"backports_asyncio_runner-1.2.0-py3-none-any.whl.metadata", "backports_asyncio_runner", "1.2.0"},
		{"requests-2.31.0-py3-none-any.whl.metadata", "requests", "2.31.0"},
		{"requests-2.31.0.tar.gz.metadata", "requests", "2.31.0"},

		// Eggs: {name}-{version}-py{X.Y}(-{platform})?.egg. Unescaped hyphens in
		// the name must not be mistaken for the field separator before the version.
		{"numpy-1.8.0-py2.7-macosx-10.9-x86_64.egg", "numpy", "1.8.0"},
		{"aws-sdk-1.0.0-py3.11.egg", "aws-sdk", "1.0.0"},
		{"aws-sdk-1.0.0-py2.7-macosx-10.9-x86_64.egg", "aws-sdk", "1.0.0"},
		{"aws-sdk-1.0.0.egg", "aws-sdk", "1.0.0"},
		// A "py{N}" component inside the name is not the interpreter field, so
		// the interpreter must be located from the end of the filename.
		{"django-rest-py3-1.0-py3.6.egg", "django-rest-py3", "1.0"},

		// Windows installers: {name}-{version}.{platform}(-py{X.Y})?.{exe,msi}.
		// The platform is not part of the version, and may contain a hyphen.
		{"foo-1.0.win32-py2.0.exe", "foo", "1.0"},
		{"pywin32-223.win32-py2.7.exe", "pywin32", "223"},
		{"numpy-1.8.0.win-amd64-py2.7.exe", "numpy", "1.8.0"},
		{"aws-sdk-1.0.0.win32-py2.7.exe", "aws-sdk", "1.0.0"},
		{"pywin32-223.win32.exe", "pywin32", "223"},
		{"cx_Oracle-5.1.2.win32-py2.7.msi", "cx_Oracle", "5.1.2"},
		{"numpy-1.8.0.win-amd64.msi", "numpy", "1.8.0"},
		// A trailing build variant belongs to neither the name nor the version.
		{"cx_Oracle-5.1.2-11g.win32-py2.7.exe", "cx_Oracle", "5.1.2"},
		// A prerelease version has no purely numeric field to anchor on.
		{"foo-1.0b1.win32-py2.7.exe", "foo", "1.0b1"},

		// Invalid
		{"invalid", "", ""},
		{"invalid.metadata", "", ""},
		{"backports.ssl_match_hostname-3.4.0.2-py2.7.whl", "", ""},
		{"invalid.exe", "", ""},
		{"foo-1.0.exe", "", ""},
		// An egg with an interpreter field but no version must not promote the
		// trailing component of a hyphenated name to the version.
		{"aws-sdk-py2.7.egg", "", ""},
	}

	for _, tt := range tests {
		name, version := h.parseFilename(tt.filename)
		if name != tt.wantName || version != tt.wantVersion {
			t.Errorf("parseFilename(%q) = (%q, %q), want (%q, %q)",
				tt.filename, name, version, tt.wantName, tt.wantVersion)
		}
	}
}

func TestPyPIRewriteJSONMetadataCooldown(t *testing.T) {
	now := time.Now()
	old := now.Add(-10 * 24 * time.Hour).Format(time.RFC3339)
	recent := now.Add(-1 * time.Hour).Format(time.RFC3339)

	proxy := &Proxy{Logger: slog.Default()}
	proxy.Cooldown = &cooldown.Config{Default: "3d"}

	h := &PyPIHandler{
		proxy:    proxy,
		proxyURL: "http://localhost:8080",
	}

	input := `{
		"info": {"name": "requests"},
		"releases": {
			"2.30.0": [{"url": "https://files.pythonhosted.org/packages/ab/cd/requests-2.30.0.tar.gz", "upload_time_iso_8601": "` + old + `"}],
			"2.31.0": [{"url": "https://files.pythonhosted.org/packages/ab/cd/requests-2.31.0.tar.gz", "upload_time_iso_8601": "` + recent + `"}]
		},
		"urls": [{"url": "https://files.pythonhosted.org/packages/ab/cd/requests-2.31.0.tar.gz", "upload_time_iso_8601": "` + recent + `"}]
	}`

	output, err := h.rewriteJSONMetadata([]byte(input))
	if err != nil {
		t.Fatalf("rewriteJSONMetadata failed: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	releases := result["releases"].(map[string]any)

	if _, ok := releases["2.30.0"]; !ok {
		t.Error("version 2.30.0 should not be filtered")
	}
	if _, ok := releases["2.31.0"]; ok {
		t.Error("version 2.31.0 should be filtered by cooldown")
	}

	// urls array should be empty since the current version is filtered
	urls := result["urls"].([]any)
	if len(urls) != 0 {
		t.Errorf("urls should be empty, got %d entries", len(urls))
	}
}

// TestPyPIParseFilenameNoHashFallback guards the identifier used for caching:
// a filename that parses to an empty name makes handleDownload fall back to a
// "_hash_<digest>" package name, which surfaces as a bogus PURL in the package
// overview.
func TestPyPIParseFilenameNoHashFallback(t *testing.T) {
	h := &PyPIHandler{proxy: &Proxy{Logger: slog.Default()}}

	filenames := []string{
		"backports_asyncio_runner-1.2.0-py3-none-any.whl",
		"backports_asyncio_runner-1.2.0-py3-none-any.whl.metadata",
		"backports_asyncio_runner-1.2.0.tar.gz",
	}

	for _, filename := range filenames {
		name, version := h.parseFilename(filename)
		if name != "backports_asyncio_runner" || version != "1.2.0" {
			t.Errorf("parseFilename(%q) = (%q, %q), want (%q, %q)",
				filename, name, version, "backports_asyncio_runner", "1.2.0")
		}
	}
}

func TestPyPIHandler_DownloadUpstreamURL(t *testing.T) {
	proxy, _, _, fetcher := setupTestProxy(t)
	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("wheel data")),
		ContentType: "application/octet-stream",
	}

	h := NewPyPIHandler(proxy, "http://localhost")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	// The path wildcard {path...} captures everything after /packages/,
	// which includes "packages/" from the rewritten URL. The upstream URL
	// must not double the "packages" segment.
	resp, err := http.Get(srv.URL + "/packages/packages/ab/cd/ef0123456789/requests-2.31.0-py3-none-any.whl")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if !fetcher.fetchCalled {
		t.Fatal("expected fetcher to be called on cache miss")
	}

	want := "https://files.pythonhosted.org/packages/ab/cd/ef0123456789/requests-2.31.0-py3-none-any.whl"
	if fetcher.fetchedURL != want {
		t.Errorf("upstream URL = %q, want %q", fetcher.fetchedURL, want)
	}
}

func TestPyPIHandler_DownloadCacheHit(t *testing.T) {
	proxy, db, store, _ := setupTestProxy(t)
	seedPackage(t, db, store, "pypi", "requests", "2.31.0",
		"requests-2.31.0-py3-none-any.whl", "wheel binary data")

	h := NewPyPIHandler(proxy, "http://localhost")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/packages/packages/ab/cd/ef0123456789/requests-2.31.0-py3-none-any.whl")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "wheel binary data" {
		t.Errorf("body = %q, want %q", body, "wheel binary data")
	}
}

func TestPyPIHandler_DownloadCacheMiss(t *testing.T) {
	proxy, _, _, fetcher := setupTestProxy(t)
	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("fetched wheel")),
		ContentType: "application/octet-stream",
	}

	h := NewPyPIHandler(proxy, "http://localhost")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/packages/packages/ab/cd/ef0123456789/newpkg-1.0.0.tar.gz")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if !fetcher.fetchCalled {
		t.Error("expected fetcher to be called on cache miss")
	}
}

func TestPyPIDownloadCooldown(t *testing.T) {
	now := time.Now()
	releases := `{"releases": {
		"1.0.0": [{"upload_time_iso_8601": "` + now.Add(-30*24*time.Hour).Format(time.RFC3339) + `"}],
		"2.0.0": [{"upload_time_iso_8601": "` + now.Add(-1*time.Hour).Format(time.RFC3339) + `"}]
	}}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentTypeJSON)
		_, _ = io.WriteString(w, releases)
	}))
	defer upstream.Close()

	tests := []struct {
		name       string
		filename   string
		wantStatus int
	}{
		{"published before the window serves the file", "newpkg-1.0.0.tar.gz", http.StatusOK},
		{"published inside the window is withheld", "newpkg-2.0.0.tar.gz", http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxy, _, _, fetcher := setupTestProxy(t)
			proxy.HTTPClient = upstream.Client()
			proxy.Cooldown = &cooldown.Config{Default: "7d"}
			fetcher.artifact = &fetch.Artifact{
				Body:        io.NopCloser(strings.NewReader("sdist data")),
				ContentType: "application/octet-stream",
			}

			h := &PyPIHandler{
				proxy:       proxy,
				upstreamURL: upstream.URL,
				proxyURL:    "http://localhost",
			}
			srv := httptest.NewServer(h.Routes())
			defer srv.Close()

			resp, err := http.Get(srv.URL + "/packages/packages/ab/cd/ef0123456789/" + tt.filename)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if tt.wantStatus == http.StatusNotFound && fetcher.fetchCalled {
				t.Error("fetched a version that is still inside the cooldown window")
			}
		})
	}
}
