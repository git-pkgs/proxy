package handler

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/registries/fetch"
)

const uvPyPIAccept = "application/vnd.pypi.simple.v1+json, application/vnd.pypi.simple.v1+html;q=0.2, text/html;q=0.01"

type pypiRoundTripFunc func(*http.Request) (*http.Response, error)

func (f pypiRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func pypiHTTPResponse(r *http.Request, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode:    http.StatusOK,
		Status:        "200 OK",
		Header:        http.Header{"Content-Type": []string{contentType}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       r,
	}
}

func setupPyPIHandler(t testing.TB, transport pypiRoundTripFunc) (*PyPIHandler, *Proxy) {
	t.Helper()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = &http.Client{Transport: transport}
	h := NewPyPIHandlerWithUpstreams(proxy, "http://proxy.test", "", "")
	h.upstreamURL = "https://pypi.test"
	return h, proxy
}

func TestSelectPyPISimpleRepresentation(t *testing.T) {
	tests := []struct {
		name   string
		accept string
		want   string
	}{
		{"missing header uses legacy html", "", "text/html"},
		{"wildcard uses legacy html", "*/*", "text/html"},
		{"uv prefers json", uvPyPIAccept, pypiSimpleJSON},
		{"json only", pypiSimpleJSON, pypiSimpleJSON},
		{"latest json", pypiSimpleLatestJSON, pypiSimpleJSON},
		{"vendor html", pypiSimpleHTML, pypiSimpleHTML},
		{"higher html quality", pypiSimpleJSON + ";q=0.2, text/html;q=0.8", "text/html"},
		{"json excluded", pypiSimpleJSON + ";q=0, text/html", "text/html"},
		{"application wildcard", "application/*", pypiSimpleJSON},
		{"unsupported type uses legacy html", "application/xml", "text/html"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := selectPyPISimpleRepresentation(tt.accept); got != tt.want {
				t.Errorf("selectPyPISimpleRepresentation(%q) = %q, want %q", tt.accept, got, tt.want)
			}
		})
	}
}

func TestPyPISimplePackageNegotiatesJSON(t *testing.T) {
	const upstreamBody = `{
		"meta":{"api-version":"1.4"},
		"name":"ruff",
		"files":[{
			"filename":"ruff-0.16.0-py3-none-any.whl",
			"url":"https://files.pythonhosted.org/packages/ab/cd/ruff-0.16.0-py3-none-any.whl",
			"hashes":{"sha256":"abc123"},
			"upload-time":"2026-08-01T12:00:00Z"
		}]
	}`

	var upstreamAccept string
	h, _ := setupPyPIHandler(t, func(r *http.Request) (*http.Response, error) {
		upstreamAccept = r.Header.Get("Accept")
		if r.URL.Path != "/simple/ruff/" {
			t.Fatalf("upstream path = %q, want %q", r.URL.Path, "/simple/ruff/")
		}
		return pypiHTTPResponse(r, pypiSimpleJSON, upstreamBody), nil
	})

	req := httptest.NewRequest(http.MethodGet, "/simple/ruff/", nil)
	req.Header.Set("Accept", uvPyPIAccept)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if upstreamAccept != pypiSimpleJSON {
		t.Errorf("upstream Accept = %q, want %q", upstreamAccept, pypiSimpleJSON)
	}
	if got := w.Header().Get("Content-Type"); got != pypiSimpleJSON {
		t.Errorf("Content-Type = %q, want %q", got, pypiSimpleJSON)
	}
	if got := w.Header().Get("Vary"); !strings.Contains(got, "Accept") {
		t.Errorf("Vary = %q, want Accept", got)
	}

	var result struct {
		Meta  map[string]string `json:"meta"`
		Files []struct {
			URL        string `json:"url"`
			UploadTime string `json:"upload-time"`
		} `json:"files"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result.Meta["api-version"] != "1.4" {
		t.Errorf("api-version = %q, want 1.4", result.Meta["api-version"])
	}
	if len(result.Files) != 1 {
		t.Fatalf("files = %d, want 1", len(result.Files))
	}
	if got, want := result.Files[0].URL, "http://proxy.test/pypi/packages/packages/ab/cd/ruff-0.16.0-py3-none-any.whl"; got != want {
		t.Errorf("file URL = %q, want %q", got, want)
	}
	if got := result.Files[0].UploadTime; got != "2026-08-01T12:00:00Z" {
		t.Errorf("upload-time = %q, want %q", got, "2026-08-01T12:00:00Z")
	}
}

func TestPyPISimpleIndexNegotiatesJSON(t *testing.T) {
	const upstreamBody = `{"meta":{"api-version":"1.4"},"projects":[{"name":"ruff"}]}`

	var upstreamAccept string
	h, _ := setupPyPIHandler(t, func(r *http.Request) (*http.Response, error) {
		upstreamAccept = r.Header.Get("Accept")
		return pypiHTTPResponse(r, pypiSimpleJSON, upstreamBody), nil
	})

	req := httptest.NewRequest(http.MethodGet, "/simple/", nil)
	req.Header.Set("Accept", uvPyPIAccept)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if upstreamAccept != pypiSimpleJSON {
		t.Errorf("upstream Accept = %q, want %q", upstreamAccept, pypiSimpleJSON)
	}
	if got := w.Header().Get("Content-Type"); got != pypiSimpleJSON {
		t.Errorf("Content-Type = %q, want %q", got, pypiSimpleJSON)
	}
	if got := w.Header().Get("Vary"); !strings.Contains(got, "Accept") {
		t.Errorf("Vary = %q, want Accept", got)
	}
	if got := w.Body.String(); got != upstreamBody {
		t.Errorf("body = %q, want %q", got, upstreamBody)
	}
}

func TestPyPISimplePackageKeepsHTMLDefault(t *testing.T) {
	const upstreamBody = `<a href="https://files.pythonhosted.org/packages/ab/cd/ruff-0.16.0.tar.gz">ruff-0.16.0.tar.gz</a>`

	var upstreamAccept string
	h, _ := setupPyPIHandler(t, func(r *http.Request) (*http.Response, error) {
		upstreamAccept = r.Header.Get("Accept")
		return pypiHTTPResponse(r, "text/html", upstreamBody), nil
	})

	req := httptest.NewRequest(http.MethodGet, "/simple/ruff/", nil)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if upstreamAccept != "text/html" {
		t.Errorf("upstream Accept = %q, want text/html", upstreamAccept)
	}
	if got := w.Header().Get("Content-Type"); got != "text/html" {
		t.Errorf("Content-Type = %q, want text/html", got)
	}
	if !strings.Contains(w.Body.String(), `href="http://proxy.test/pypi/packages/packages/ab/cd/ruff-0.16.0.tar.gz"`) {
		t.Errorf("download URL was not rewritten: %s", w.Body.String())
	}
}

func TestPyPISimplePackageCachesRepresentationsSeparately(t *testing.T) {
	hits := make(map[string]int)
	h, proxy := setupPyPIHandler(t, func(r *http.Request) (*http.Response, error) {
		accept := r.Header.Get("Accept")
		hits[accept]++
		if accept == pypiSimpleJSON {
			body := `{"meta":{"api-version":"1.4"},"name":"ruff","files":[]}`
			return pypiHTTPResponse(r, pypiSimpleJSON, body), nil
		}
		return pypiHTTPResponse(r, accept, `<a href="/ruff.tar.gz">ruff.tar.gz</a>`), nil
	})
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour

	for range 2 {
		for _, accept := range []string{pypiLegacyHTML, pypiSimpleHTML, uvPyPIAccept} {
			req := httptest.NewRequest(http.MethodGet, "/simple/ruff/", nil)
			req.Header.Set("Accept", accept)
			w := httptest.NewRecorder()
			h.Routes().ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("Accept %q: status = %d, want 200: %s", accept, w.Code, w.Body.String())
			}
		}
	}

	if got := hits[pypiLegacyHTML]; got != 1 {
		t.Errorf("HTML upstream requests = %d, want 1", got)
	}
	if got := hits[pypiSimpleHTML]; got != 1 {
		t.Errorf("vendor HTML upstream requests = %d, want 1", got)
	}
	if got := hits[pypiSimpleJSON]; got != 1 {
		t.Errorf("JSON upstream requests = %d, want 1", got)
	}
}

func TestPyPISimpleJSONCooldown(t *testing.T) {
	now := time.Now()
	old := now.Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	recent := now.Add(-time.Hour).Format(time.RFC3339)

	h, proxy := setupPyPIHandler(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/simple/ruff/":
			body := `{"meta":{"api-version":"1.4"},"name":"ruff","files":[` +
				`{"filename":"ruff-1.0.0.tar.gz","url":"https://files.pythonhosted.org/packages/ab/ruff-1.0.0.tar.gz","upload-time":"` + old + `"},` +
				`{"filename":"ruff-2.0.0.tar.gz","url":"https://files.pythonhosted.org/packages/cd/ruff-2.0.0.tar.gz","upload-time":"` + recent + `"}` +
				`]}`
			return pypiHTTPResponse(r, pypiSimpleJSON, body), nil
		case "/pypi/ruff/json":
			body := `{"releases":{` +
				`"1.0.0":[{"upload_time_iso_8601":"` + old + `"}],` +
				`"2.0.0":[{"upload_time_iso_8601":"` + recent + `"}]` +
				`}}`
			return pypiHTTPResponse(r, "application/json", body), nil
		default:
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
			return nil, nil
		}
	})
	proxy.Cooldown = &cooldown.Config{Default: "7d"}

	req := httptest.NewRequest(http.MethodGet, "/simple/ruff/", nil)
	req.Header.Set("Accept", uvPyPIAccept)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var result struct {
		Files []struct {
			Filename string `json:"filename"`
		} `json:"files"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(result.Files) != 1 || result.Files[0].Filename != "ruff-1.0.0.tar.gz" {
		t.Errorf("files = %#v, want only ruff-1.0.0.tar.gz", result.Files)
	}
}

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

	h := NewPyPIHandlerWithUpstreams(proxy, "http://localhost", "", "")
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

	h := NewPyPIHandlerWithUpstreams(proxy, "http://localhost", "", "")
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

	h := NewPyPIHandlerWithUpstreams(proxy, "http://localhost", "", "")
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

// TestPyPIDownloadCooldownMetadataCache ensures that repeated downloads that
// trigger cooldown filtering reuse the cached PyPI JSON metadata instead of
// fetching it from upstream once per download.
func TestPyPIDownloadCooldownMetadataCache(t *testing.T) {
	now := time.Now()
	releases := `{"releases": {
		"1.0.0": [{"upload_time_iso_8601": "` + now.Add(-30*24*time.Hour).Format(time.RFC3339) + `"}],
		"2.0.0": [{"upload_time_iso_8601": "` + now.Add(-1*time.Hour).Format(time.RFC3339) + `"}]
	}}`

	var metadataRequests atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/pypi/newpkg/json" {
			metadataRequests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, releases)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, "package data")
	}))
	defer upstream.Close()

	proxy, _, _, fetcher := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.Cooldown = &cooldown.Config{Default: "7d"}
	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("package data")),
		ContentType: "application/octet-stream",
	}

	h := &PyPIHandler{
		proxy:       proxy,
		upstreamURL: upstream.URL,
		proxyURL:    "http://localhost",
	}
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	// Two downloads of the same package: one outside the cooldown window
	// (served) and one inside (withheld). Both go through the download path
	// that resolves filtered versions.
	for _, filename := range []string{"newpkg-1.0.0.tar.gz", "newpkg-2.0.0.tar.gz"} {
		resp, err := http.Get(srv.URL + "/packages/packages/ab/cd/ef0123456789/" + filename)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		_ = resp.Body.Close()
	}

	if got := metadataRequests.Load(); got != 1 {
		t.Errorf("upstream metadata JSON requests = %d, want 1 (repeated downloads should reuse the cached metadata)", got)
	}
}
