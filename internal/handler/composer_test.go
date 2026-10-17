package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/purl"
	"github.com/git-pkgs/registries/fetch"
)

func TestComposerRewriteMetadata(t *testing.T) {
	h := &ComposerHandler{
		proxy:    testProxy(),
		proxyURL: "http://localhost:8080",
	}

	input := `{
		"packages": {
			"symfony/console": [
				{
					"version": "6.0.0",
					"dist": {
						"url": "https://repo.packagist.org/files/symfony/console/6.0.0/abc123.zip",
						"type": "zip"
					}
				}
			]
		}
	}`

	output, err := h.rewriteMetadata([]byte(input))
	if err != nil {
		t.Fatalf("rewriteMetadata failed: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	packages := result["packages"].(map[string]any)
	versions := packages["symfony/console"].([]any)
	v := versions[0].(map[string]any)
	dist := v["dist"].(map[string]any)

	expected := "http://localhost:8080/composer/files/symfony/console/6.0.0/abc123.zip"
	if dist["url"] != expected {
		t.Errorf("dist url = %q, want %q", dist["url"], expected)
	}
}

func TestComposerRewriteMetadataMinifiedInheritance(t *testing.T) {
	h := &ComposerHandler{
		proxy:    testProxy(),
		proxyURL: "http://localhost:8080",
	}

	// Minified format: first version has all fields, subsequent versions
	// only include fields that changed. The proxy keeps the format, so once
	// Composer expands it every version still has all fields (including
	// "name").
	input := `{
		"minified": "composer/2.0",
		"packages": {
			"symfony/console": [
				{
					"name": "symfony/console",
					"description": "Symfony Console Component",
					"version": "6.0.0",
					"dist": {
						"url": "https://repo.packagist.org/files/symfony/console/6.0.0/abc123.zip",
						"type": "zip"
					}
				},
				{
					"version": "5.4.0",
					"dist": {
						"url": "https://repo.packagist.org/files/symfony/console/5.4.0/def456.zip",
						"type": "zip"
					}
				}
			]
		}
	}`

	output, err := h.rewriteMetadata([]byte(input))
	if err != nil {
		t.Fatalf("rewriteMetadata failed: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	if result["minified"] != "composer/2.0" {
		t.Errorf("minified = %v, want the format kept", result["minified"])
	}

	versions := composerVersions(t, output, "symfony/console")

	// Second version should have inherited the "name" and "description" fields
	v1 := versions[1]
	if v1["name"] != "symfony/console" {
		t.Errorf("second version name = %v, want %q", v1["name"], "symfony/console")
	}
	if v1["description"] != "Symfony Console Component" {
		t.Errorf("second version description = %v, want %q", v1["description"], "Symfony Console Component")
	}
}

func TestComposerRewriteMetadataMinifiedDevReset(t *testing.T) {
	h := &ComposerHandler{
		proxy:    testProxy(),
		proxyURL: "http://localhost:8080",
	}

	// The ~dev sentinel resets the inheritance chain for dev versions.
	input := `{
		"minified": "composer/2.0",
		"packages": {
			"symfony/console": [
				{
					"name": "symfony/console",
					"description": "Symfony Console Component",
					"license": ["MIT"],
					"version": "6.0.0",
					"dist": {
						"url": "https://repo.packagist.org/files/symfony/console/6.0.0/abc123.zip",
						"type": "zip"
					}
				},
				"~dev",
				{
					"name": "symfony/console",
					"version": "dev-main",
					"dist": {
						"url": "https://repo.packagist.org/files/symfony/console/dev-main/xyz789.zip",
						"type": "zip"
					}
				}
			]
		}
	}`

	output, err := h.rewriteMetadata([]byte(input))
	if err != nil {
		t.Fatalf("rewriteMetadata failed: %v", err)
	}

	versions := composerVersions(t, output, "symfony/console")

	if len(versions) != 2 {
		t.Fatalf("expected 2 versions, got %d", len(versions))
	}

	// Dev version should NOT have inherited "license" or "description"
	// from the tagged version (the ~dev sentinel resets inheritance).
	devVersion := versions[1]
	if devVersion["version"] != "dev-main" {
		t.Errorf("dev version = %v, want %q", devVersion["version"], "dev-main")
	}
	if _, ok := devVersion["license"]; ok {
		t.Error("dev version should not have inherited license field after ~dev reset")
	}
	if _, ok := devVersion["description"]; ok {
		t.Error("dev version should not have inherited description field after ~dev reset")
	}
}

func TestComposerRewriteMetadataUnset(t *testing.T) {
	h := &ComposerHandler{
		proxy:    &Proxy{Logger: slog.Default()},
		proxyURL: "http://localhost:8080",
	}

	// In the minified format, "__unset" removes a field from the inherited
	// state. v1.29.0 has require-dev, v1.28.0 unsets it, v1.27.0 inherits the
	// unset state. Composer rejects metadata where require-dev (or any link
	// field) is the literal string "__unset" rather than an object.
	input := `{
		"minified": "composer/2.0",
		"packages": {
			"venturecraft/revisionable": [
				{
					"name": "venturecraft/revisionable",
					"version": "1.29.0",
					"require": {"php": ">=5.4"},
					"require-dev": {"orchestra/testbench": "~3.0"},
					"dist": {"url": "https://example.com/a.zip", "type": "zip"}
				},
				{
					"version": "1.28.0",
					"require-dev": "__unset"
				},
				{
					"version": "1.27.0"
				},
				{
					"version": "1.26.0",
					"require-dev": {"foo/bar": "1.0"}
				}
			]
		}
	}`

	output, err := h.rewriteMetadata([]byte(input))
	if err != nil {
		t.Fatalf("rewriteMetadata failed: %v", err)
	}

	versions := composerVersions(t, output, "venturecraft/revisionable")
	if len(versions) != 4 {
		t.Fatalf("expected 4 versions, got %d", len(versions))
	}

	byVersion := map[string]map[string]any{}
	for _, vmap := range versions {
		byVersion[vmap["version"].(string)] = vmap
	}

	if _, ok := byVersion["1.29.0"]["require-dev"].(map[string]any); !ok {
		t.Errorf("1.29.0 require-dev should be an object, got %T", byVersion["1.29.0"]["require-dev"])
	}
	if rd, ok := byVersion["1.28.0"]["require-dev"]; ok {
		t.Errorf("1.28.0 require-dev should be absent, got %v", rd)
	}
	if rd, ok := byVersion["1.27.0"]["require-dev"]; ok {
		t.Errorf("1.27.0 require-dev should be absent (inherited unset), got %v", rd)
	}
	if _, ok := byVersion["1.26.0"]["require-dev"].(map[string]any); !ok {
		t.Errorf("1.26.0 require-dev should be an object, got %T", byVersion["1.26.0"]["require-dev"])
	}
	if _, ok := byVersion["1.27.0"]["require"].(map[string]any); !ok {
		t.Error("1.27.0 should still inherit require from 1.29.0")
	}
}

func TestComposerRewriteMetadataCooldownPreservesNames(t *testing.T) {
	now := time.Now()
	old := now.Add(-10 * 24 * time.Hour).Format(time.RFC3339)
	veryOld := now.Add(-20 * 24 * time.Hour).Format(time.RFC3339)
	recent := now.Add(-1 * time.Hour).Format(time.RFC3339)

	proxy := &Proxy{Logger: slog.Default()}
	proxy.Cooldown = &cooldown.Config{Default: "3d"}

	h := &ComposerHandler{
		proxy:    proxy,
		proxyURL: "http://localhost:8080",
	}

	// Minified format where "name" only appears in first version.
	// When cooldown filters the first version, remaining versions must
	// still have the "name" field once Composer expands them.
	input := `{
		"minified": "composer/2.0",
		"packages": {
			"symfony/console": [
				{
					"name": "symfony/console",
					"description": "Symfony Console Component",
					"version": "7.0.0",
					"time": "` + recent + `",
					"dist": {"url": "https://repo.packagist.org/7.0.0.zip", "type": "zip"}
				},
				{
					"version": "6.0.0",
					"time": "` + old + `",
					"dist": {"url": "https://repo.packagist.org/6.0.0.zip", "type": "zip"}
				},
				{
					"version": "5.0.0",
					"time": "` + veryOld + `",
					"dist": {"url": "https://repo.packagist.org/5.0.0.zip", "type": "zip"}
				}
			]
		}
	}`

	output, err := h.rewriteMetadata([]byte(input))
	if err != nil {
		t.Fatalf("rewriteMetadata failed: %v", err)
	}

	versions := composerVersions(t, output, "symfony/console")

	// v7.0.0 should be filtered by cooldown, leaving v6.0.0 and v5.0.0
	if len(versions) != 2 {
		t.Fatalf("expected 2 versions after cooldown, got %d", len(versions))
	}

	// Both remaining versions must have the "name" field
	for _, vmap := range versions {
		if vmap["name"] != "symfony/console" {
			t.Errorf("version %v missing name field, got %v", vmap["version"], vmap["name"])
		}
	}
}

func TestComposerRewriteDistURLGitHubZipball(t *testing.T) {
	// GitHub zipball URLs end with a bare commit hash, no file extension.
	// The proxy must produce a filename with .zip extension so that the
	// archives library can detect the format when browsing source.
	h := &ComposerHandler{
		proxy:    testProxy(),
		proxyURL: "http://localhost:8080",
	}

	url, ok := h.proxyDistURL("symfony/asset", "v7.4.8",
		"https://api.github.com/repos/symfony/asset/zipball/d2e2f014ccd6ec9fae8dbe6336a4164346a2a856", "zip")
	if !ok {
		t.Fatal("dist URL was not rewritten")
	}

	// The rewritten URL's filename must have a .zip extension
	if !strings.HasSuffix(url, ".zip") {
		t.Errorf("rewritten dist URL filename has no .zip extension: %s", url)
	}
}

func TestComposerRewriteMetadataGitHubZipballFilenames(t *testing.T) {
	// End-to-end: metadata with GitHub zipball URLs should produce
	// download URLs that end in .zip so browse source can open them.
	h := &ComposerHandler{
		proxy:    testProxy(),
		proxyURL: "http://localhost:8080",
	}

	input := `{
		"packages": {
			"symfony/config": [
				{
					"version": "v7.4.8",
					"dist": {
						"url": "https://api.github.com/repos/symfony/config/zipball/c7369cc1da250fcbfe0c5a9d109e419661549c39",
						"type": "zip",
						"reference": "c7369cc1da250fcbfe0c5a9d109e419661549c39"
					}
				}
			]
		}
	}`

	output, err := h.rewriteMetadata([]byte(input))
	if err != nil {
		t.Fatalf("rewriteMetadata failed: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	packages := result["packages"].(map[string]any)
	versions := packages["symfony/config"].([]any)
	v := versions[0].(map[string]any)
	dist := v["dist"].(map[string]any)
	url := dist["url"].(string)

	if !strings.HasSuffix(url, ".zip") {
		t.Errorf("rewritten URL should end in .zip, got %s", url)
	}
}

func TestComposerExpandMinifiedSharedDistReferences(t *testing.T) {
	// When a minified version inherits the dist field from a previous version
	// (i.e. it doesn't include its own dist), rewriting must still give each
	// version a dist URL naming its own version.
	h := &ComposerHandler{
		proxy:    testProxy(),
		proxyURL: "http://localhost:8080",
	}

	// In this minified payload, v5.3.0 does NOT include a dist field,
	// so it inherits v5.4.0's dist. After URL rewriting, each version must
	// have its own correct dist URL.
	input := `{
		"minified": "composer/2.0",
		"packages": {
			"vendor/pkg": [
				{
					"name": "vendor/pkg",
					"version": "5.4.0",
					"dist": {
						"url": "https://api.github.com/repos/vendor/pkg/zipball/aaa111",
						"type": "zip",
						"reference": "aaa111"
					}
				},
				{
					"version": "5.3.0"
				}
			]
		}
	}`

	output, err := h.rewriteMetadata([]byte(input))
	if err != nil {
		t.Fatalf("rewriteMetadata failed: %v", err)
	}

	versions := composerVersions(t, output, "vendor/pkg")
	if len(versions) != 2 {
		t.Fatalf("expected 2 versions, got %d", len(versions))
	}

	url1 := distURL(t, versions[0])
	url2 := distURL(t, versions[1])

	// Each version must have its own URL with its own version in the path
	if !strings.Contains(url1, "/5.4.0/") {
		t.Errorf("v5.4.0 dist URL should contain /5.4.0/, got %s", url1)
	}
	if !strings.Contains(url2, "/5.3.0/") {
		t.Errorf("v5.3.0 dist URL should contain /5.3.0/, got %s", url2)
	}

	// The two URLs must be different
	if url1 == url2 {
		t.Errorf("both versions have the same dist URL (shared reference bug): %s", url1)
	}
}

// TestComposerDownloadDevVersionUsesDevMetadata is a regression test for the
// bug that made it impossible to install a *-dev dependency from dist.
//
// Packagist serves development versions (e.g. "3.x-dev", "dev-master") from a
// separate "{package}~dev.json" metadata file; the regular "{package}.json"
// file contains only tagged releases. The download handler used to fetch only
// the regular file, so it could never find the dist URL for a dev version and
// returned 404 — causing Composer to silently fall back to a git clone.
//
// This test serves both files from a mock upstream and asserts that:
//   - the OLD behavior (regular file only) cannot resolve the dev version, and
//   - the FIXED behavior (consulting the ~dev file) does.
func TestComposerDownloadDevVersionUsesDevMetadata(t *testing.T) {
	const (
		pkg     = "phpmd/phpmd"
		vendor  = "phpmd"
		name    = "phpmd"
		version = "3.x-dev"
		distURL = "https://api.github.com/repos/phpmd/phpmd/zipball/2a9217f60aaf27bf6ddad9188f254d020ab70745"
	)

	// Regular metadata: tagged releases only — no dev versions.
	stableBody := `{
		"packages": {
			"phpmd/phpmd": [
				{"version": "2.15.0", "dist": {"url": "https://example.com/2.15.0.zip", "type": "zip"}}
			]
		}
	}`

	// ~dev metadata: where the 3.x-dev version actually lives.
	devBody := `{
		"packages": {
			"phpmd/phpmd": [
				{"version": "3.x-dev", "dist": {"url": "` + distURL + `", "type": "zip"}}
			]
		}
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/p2/phpmd/phpmd.json":
			_, _ = w.Write([]byte(stableBody))
		case "/p2/phpmd/phpmd~dev.json":
			_, _ = w.Write([]byte(devBody))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	h := &ComposerHandler{
		proxy:    testProxy(),
		repoURL:  srv.URL,
		proxyURL: "http://localhost:8080",
	}

	ctx := context.Background()

	// OLD behavior: fetching only the regular file fails to resolve the dev
	// version, which is what produced the 404 before the fix.
	stableURL := srv.URL + "/p2/phpmd/phpmd.json"
	found, err := h.findDownloadURLFromMetadata(ctx, stableURL, pkg, version)
	if err != nil {
		t.Fatalf("unexpected error fetching regular metadata: %v", err)
	}
	if found.url != "" {
		t.Fatalf("regular metadata unexpectedly contained dev version %q (got %q); "+
			"the test no longer reproduces the original bug", version, found.url)
	}

	// FIXED behavior: the handler consults the ~dev file (it is first in the
	// candidate list for dev versions) and resolves the dist URL.
	files := h.metadataFilesForVersion(vendor, name, version)
	if len(files) == 0 || !strings.HasSuffix(files[0].url, "/p2/phpmd/phpmd~dev.json") || files[0].cacheKey != "phpmd/phpmd~dev" {
		t.Fatalf("dev version should consult the ~dev metadata file first, got %v", files)
	}

	var resolved string
	for _, f := range files {
		found, err := h.findDownloadURLFromMetadata(ctx, f.url, pkg, version)
		if err != nil {
			t.Fatalf("unexpected error fetching metadata %q: %v", f.url, err)
		}
		resolved = found.url
		if resolved != "" {
			break
		}
	}

	if resolved != distURL {
		t.Errorf("dev version dist URL = %q, want %q", resolved, distURL)
	}
}

const (
	composerCommitA = "1111111111111111111111111111111111111111"
	composerCommitB = "2222222222222222222222222222222222222222"
)

// setupComposerBranch returns a caching Composer handler whose upstream lists
// distPath as the dist of v/p's dev-master and 1.0.0 and serves archives by
// request URI.
func setupComposerBranch(t *testing.T, distPath string, archives map[string]string) (http.Handler, *Proxy) {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/p2/v/p.json" || r.URL.Path == "/p2/v/p~dev.json" {
			dist := fmt.Sprintf(`"dist":{"url":%q,"type":"zip"}`, srv.URL+distPath)
			_, _ = fmt.Fprintf(w, `{"packages":{"v/p":[{"version":"dev-master",%s},{"version":"1.0.0",%s}]}}`, dist, dist)
			return
		}
		body, ok := archives[r.URL.RequestURI()]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	proxy, _, _, _ := setupTestProxy(t)
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(srv.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	return NewComposerHandlerWithUpstreams(proxy, "http://proxy", srv.URL, srv.URL).Routes(), proxy
}

// cachedComposerFile returns the cached bytes of v/p dev-master's filename,
// or "" when nothing is cached under it.
func cachedComposerFile(t *testing.T, proxy *Proxy, filename string) string {
	t.Helper()
	cached, err := proxy.GetCachedArtifact(context.Background(), "composer", "v/p", "dev-master", filename)
	if err != nil {
		t.Fatalf("reading cache for %s: %v", filename, err)
	}
	if cached == nil {
		return ""
	}
	defer func() { _ = cached.Reader.Close() }()
	data, err := io.ReadAll(cached.Reader)
	if err != nil {
		t.Fatalf("reading cached %s: %v", filename, err)
	}
	return string(data)
}

// TestComposerDownloadPinnedBranchCommit checks that a lock file pinning an
// older commit of a branch gets that commit rather than the head the branch's
// metadata lists now, and that the head is not cached under the older name.
// The second round is served from the cache.
func TestComposerDownloadPinnedBranchCommit(t *testing.T) {
	tests := []struct {
		name, distPath, filename, cached string
	}{
		{"github zipball", "/repos/v/p/zipball/%s", "%s.zip", "%s.zip"},
		{"github zipball without .zip", "/repos/v/p/zipball/%s", "%s", "%s"},
		{"bitbucket archive", "/v/p/get/%s.zip", "%s.zip", "%s.zip"},
		{"gitlab archive", "/api/v4/projects/v%%2Fp/repository/archive.zip?sha=%s", "archive.zip?sha=%s", "archive-%s.zip"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, proxy := setupComposerBranch(t, fmt.Sprintf(tt.distPath, composerCommitB), map[string]string{
				fmt.Sprintf(tt.distPath, composerCommitA): "commit A",
				fmt.Sprintf(tt.distPath, composerCommitB): "commit B",
			})
			for range 2 {
				for _, c := range []struct{ commit, want string }{
					{composerCommitA, "commit A"},
					{composerCommitB, "commit B"},
				} {
					filename, want := fmt.Sprintf(tt.filename, c.commit), c.want
					w := httptest.NewRecorder()
					h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/files/v/p/dev-master/"+filename, nil))
					if w.Code != http.StatusOK || w.Body.String() != want {
						t.Errorf("%s: status %d, body %q, want 200 %q", filename, w.Code, w.Body.String(), want)
					}
					cached := fmt.Sprintf(tt.cached, c.commit)
					if got := cachedComposerFile(t, proxy, cached); got != want {
						t.Errorf("%s cached as %q, want %q", cached, got, want)
					}
				}
			}
		})
	}
}

// composerMovingBranch is an upstream whose dev-master head can move. It
// serves the archives of commits A and B under distPath and counts how often
// they are fetched.
type composerMovingBranch struct {
	h       http.Handler
	proxy   *Proxy
	url     string
	head    atomic.Value
	fetches atomic.Int32
}

func setupComposerMovingBranch(t *testing.T, distPath string) *composerMovingBranch {
	t.Helper()
	b := &composerMovingBranch{}
	b.head.Store(composerCommitA)
	archives := map[string]string{
		fmt.Sprintf(distPath, composerCommitA): "commit A",
		fmt.Sprintf(distPath, composerCommitB): "commit B",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/p2/v/p.json" || r.URL.Path == "/p2/v/p~dev.json" {
			dist := b.url + fmt.Sprintf(distPath, b.head.Load())
			_, _ = fmt.Fprintf(w, `{"packages":{"v/p":[{"version":"dev-master","dist":{"url":%q,"type":"zip"}}]}}`, dist)
			return
		}
		body, ok := archives[r.URL.RequestURI()]
		if !ok {
			http.NotFound(w, r)
			return
		}
		b.fetches.Add(1)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	b.url = srv.URL

	b.proxy, _, _, _ = setupTestProxy(t)
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(srv.Client()), fetch.WithMaxRetries(0))
	b.proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	b.h = NewComposerHandlerWithUpstreams(b.proxy, "http://proxy", srv.URL, srv.URL).Routes()
	return b
}

// TestComposerDownloadBranchMovesOn checks a lock file pinning A while the
// branch moves from A to B: A is cached while it is the head and still served
// from the cache afterwards, and B, the head a new lock file pins, comes from
// upstream.
func TestComposerDownloadBranchMovesOn(t *testing.T) {
	tests := []struct {
		name, distPath, filename, cached string
	}{
		{"github zipball", "/repos/v/p/zipball/%s", "%s.zip", "%s.zip"},
		{"gitlab archive", "/api/v4/projects/v%%2Fp/repository/archive.zip?sha=%s", "archive.zip?sha=%s", "archive-%s.zip"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := setupComposerMovingBranch(t, tt.distPath)
			get := func(commit, want string, wantFetches int32) {
				t.Helper()
				filename := fmt.Sprintf(tt.filename, commit)
				w := httptest.NewRecorder()
				b.h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/files/v/p/dev-master/"+filename, nil))
				if w.Code != http.StatusOK || w.Body.String() != want {
					t.Errorf("%s: status %d, body %q, want 200 %q", filename, w.Code, w.Body.String(), want)
				}
				if got := cachedComposerFile(t, b.proxy, fmt.Sprintf(tt.cached, commit)); got != want {
					t.Errorf("%s cached as %q, want %q", filename, got, want)
				}
				if n := b.fetches.Load(); n != wantFetches {
					t.Errorf("after %s: %d upstream fetches, want %d", filename, n, wantFetches)
				}
			}
			get(composerCommitA, "commit A", 1)
			b.head.Store(composerCommitB)
			get(composerCommitA, "commit A", 1)
			get(composerCommitB, "commit B", 2)
			get(composerCommitB, "commit B", 2)
		})
	}
}

// TestComposerDownloadGitLabSharedName checks that an archive an older proxy
// cached under GitLab's shared archive.zip name is dropped once a commit is
// fetched under its own name.
func TestComposerDownloadGitLabSharedName(t *testing.T) {
	distPath := "/api/v4/projects/v%%2Fp/repository/archive.zip?sha=%s"
	b := setupComposerMovingBranch(t, distPath)
	shared, err := b.proxy.GetOrFetchArtifactFromURL(context.Background(), "composer", "v/p", "dev-master",
		"archive.zip", b.url+fmt.Sprintf(distPath, composerCommitB))
	if err != nil {
		t.Fatalf("seeding shared entry: %v", err)
	}
	_ = shared.Reader.Close()

	w := httptest.NewRecorder()
	b.h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/files/v/p/dev-master/archive.zip?sha="+composerCommitA, nil))
	if w.Code != http.StatusOK || w.Body.String() != "commit A" {
		t.Errorf("status %d, body %q, want 200 %q", w.Code, w.Body.String(), "commit A")
	}
	if got := cachedComposerFile(t, b.proxy, "archive.zip"); got != "" {
		t.Errorf("archive.zip still cached as %q", got)
	}
}

// TestComposerDownloadCachedFromOtherCommit checks that an archive an older
// proxy cached under a commit's name but fetched from another commit's URL is
// fetched again, while a correct entry is served from the cache.
func TestComposerDownloadCachedFromOtherCommit(t *testing.T) {
	zipball := "/repos/v/p/zipball/"
	var fetches atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/p2/v/p.json", "/p2/v/p~dev.json":
			_, _ = fmt.Fprintf(w, `{"packages":{"v/p":[{"version":"dev-master","dist":{"url":%q,"type":"zip"}}]}}`, srv.URL+zipball+composerCommitB)
		case zipball + composerCommitA:
			fetches.Add(1)
			_, _ = io.WriteString(w, "commit A")
		case zipball + composerCommitB:
			fetches.Add(1)
			_, _ = io.WriteString(w, "commit B")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	proxy, _, _, _ := setupTestProxy(t)
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(srv.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	h := NewComposerHandlerWithUpstreams(proxy, "http://proxy", srv.URL, srv.URL).Routes()

	// What proxy versions before the pinned-commit fix stored: the head
	// under the name of the commit the lock file pins.
	stale, err := proxy.GetOrFetchArtifactFromURL(context.Background(), "composer", "v/p", "dev-master",
		composerCommitA+".zip", srv.URL+zipball+composerCommitB)
	if err != nil {
		t.Fatalf("seeding stale entry: %v", err)
	}
	_ = stale.Reader.Close()
	fetches.Store(0)

	for range 2 {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/files/v/p/dev-master/"+composerCommitA+".zip", nil))
		if w.Code != http.StatusOK || w.Body.String() != "commit A" {
			t.Errorf("status %d, body %q, want 200 %q", w.Code, w.Body.String(), "commit A")
		}
	}
	if got := cachedComposerFile(t, proxy, composerCommitA+".zip"); got != "commit A" {
		t.Errorf("cached as %q, want %q", got, "commit A")
	}
	if n := fetches.Load(); n != 1 {
		t.Errorf("upstream archive fetched %d times, want 1", n)
	}
}

// TestComposerDownloadGitLabTaggedRelease checks that a tagged release on
// GitLab is only served as the commit its metadata lists.
func TestComposerDownloadGitLabTaggedRelease(t *testing.T) {
	archive := "/api/v4/projects/v%2Fp/repository/archive.zip?sha="
	h, _ := setupComposerBranch(t, archive+composerCommitB, map[string]string{
		archive + composerCommitA: "commit A",
		archive + composerCommitB: "commit B",
	})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/files/v/p/1.0.0/archive.zip?sha="+composerCommitA, nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("other commit: status %d, body %q, want 404", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/files/v/p/1.0.0/archive.zip?sha="+composerCommitB, nil))
	if w.Code != http.StatusOK || w.Body.String() != "commit B" {
		t.Errorf("listed commit: status %d, body %q, want 200 %q", w.Code, w.Body.String(), "commit B")
	}
}

// TestComposerDownloadBranchWithSlash checks that a branch whose name holds a
// slash gets a dist URL the download route matches.
func TestComposerDownloadBranchWithSlash(t *testing.T) {
	const version = "dev-feature/x"
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/zip/x.zip" {
			_, _ = io.WriteString(w, "feature")
			return
		}
		_, _ = fmt.Fprintf(w, `{"packages":{"v/p":[{"version":%q,"dist":{"url":%q,"type":"zip"}}]}}`, version, srv.URL+"/zip/x.zip")
	}))
	t.Cleanup(srv.Close)
	proxy, _, _, _ := setupTestProxy(t)
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(srv.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	h := NewComposerHandlerWithUpstreams(proxy, "http://proxy", srv.URL, srv.URL)

	distURL, ok := h.proxyDistURL("v/p", version, srv.URL+"/zip/x.zip", "zip")
	if want := "http://proxy/composer/files/v/p/dev-feature%2Fx/x.zip"; !ok || distURL != want {
		t.Fatalf("dist URL = %q, want %q", distURL, want)
	}

	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, strings.TrimPrefix(distURL, "http://proxy/composer"), nil))
	if w.Code != http.StatusOK || w.Body.String() != "feature" {
		t.Errorf("status %d, body %q, want 200 %q", w.Code, w.Body.String(), "feature")
	}
}

// TestComposerDownloadUnlistedFile checks that a file the metadata no longer
// lists, and that names no commit to fetch instead, is a 404 rather than the
// listed archive cached under the requested name.
func TestComposerDownloadUnlistedFile(t *testing.T) {
	tests := []struct {
		name, distPath, listed, stale string
	}{
		{"plain name", "/dist/main.zip", "main.zip", "old.zip"},
		{"escaped name", "/dist/p%2Bdev-main.zip", "p%2Bdev-main.zip", "old.zip"},
		{"gitlab archive with other query", "/api/v4/projects/v%2Fp/repository/archive.zip?sha=" + composerCommitB,
			"archive.zip?sha=" + composerCommitB, "archive.zip?sha=" + composerCommitA + "&path=src"},
		{"branch name for a commit", "/repos/v/p/zipball/" + composerCommitB, composerCommitB + ".zip", "master.zip"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, proxy := setupComposerBranch(t, tt.distPath, map[string]string{tt.distPath: "head"})

			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/files/v/p/dev-master/"+tt.stale, nil))
			if w.Code != http.StatusNotFound {
				t.Errorf("%s: status %d, body %q, want 404", tt.stale, w.Code, w.Body.String())
			}
			staleName, _, _ := strings.Cut(tt.stale, "?")
			if got := cachedComposerFile(t, proxy, staleName); got != "" {
				t.Errorf("%s cached as %q, want nothing", staleName, got)
			}

			w = httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/files/v/p/dev-master/"+tt.listed, nil))
			if w.Code != http.StatusOK || w.Body.String() != "head" {
				t.Errorf("%s: status %d, body %q, want 200 %q", tt.listed, w.Code, w.Body.String(), "head")
			}
		})
	}
}

// TestComposerDownloadTaggedReleaseCommit checks that a tagged release is only
// served as the commit its metadata lists, so a client cannot have another
// commit stored as that release.
func TestComposerDownloadTaggedReleaseCommit(t *testing.T) {
	zipball := "/repos/v/p/zipball/"
	h, proxy := setupComposerBranch(t, zipball+composerCommitB, map[string]string{
		zipball + composerCommitA: "commit A",
		zipball + composerCommitB: "commit B",
	})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/files/v/p/1.0.0/"+composerCommitA+".zip", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("other commit: status %d, body %q, want 404", w.Code, w.Body.String())
	}
	cached, err := proxy.GetCachedArtifact(context.Background(), "composer", "v/p", "1.0.0", composerCommitA+".zip")
	if err != nil || cached != nil {
		t.Errorf("other commit cached as 1.0.0: %v, %v", cached, err)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/files/v/p/1.0.0/"+composerCommitB+".zip", nil))
	if w.Code != http.StatusOK || w.Body.String() != "commit B" {
		t.Errorf("listed commit: status %d, body %q, want 200 %q", w.Code, w.Body.String(), "commit B")
	}
}

func TestComposerUpstreamDistURL(t *testing.T) {
	const zipball = "https://api.github.com/repos/v/p/zipball/"
	const gitlab = "https://gitlab.com/api/v4/projects/v%2Fp/repository/archive.zip?sha="
	sha256A := strings.Repeat("a", 64)
	tests := []struct {
		name, listed, distType, filename, want string
	}{
		{"listed file", zipball + composerCommitB, "zip", composerCommitB + ".zip", zipball + composerCommitB},
		{"older commit", zipball + composerCommitB, "zip", composerCommitA + ".zip", zipball + composerCommitA},
		{"sha256 commit", zipball + composerCommitB, "zip", sha256A + ".zip", zipball + sha256A},
		{"bare listed commit", zipball + composerCommitB, "zip", composerCommitB, zipball + composerCommitB},
		{"bare older commit", zipball + composerCommitB, "zip", composerCommitA, zipball + composerCommitA},
		{"tar dist keeps bare name", zipball + composerCommitB, "tar", composerCommitA, zipball + composerCommitA},
		{"short hash", zipball + composerCommitB, "zip", "1111111.zip", ""},
		{"upper-case hash", zipball + composerCommitB, "zip", strings.ToUpper(sha256A) + ".zip", ""},
		{"other name", "https://example.com/dist/main.zip", "zip", "old.zip", ""},
		{"gitlab older commit", gitlab + composerCommitB, "zip", "archive.zip?sha=" + composerCommitA, gitlab + composerCommitA},
		{"gitlab short hash", gitlab + composerCommitB, "zip", "archive.zip?sha=1111111", ""},
		{"gitlab other archive", gitlab + composerCommitB, "zip", "other.zip?sha=" + composerCommitA, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := upstreamDistURL(tt.listed, tt.distType, tt.filename, true)
			if got != tt.want || ok != (tt.want != "") {
				t.Errorf("upstreamDistURL(%q, %q) = %q, %v; want %q", tt.listed, tt.filename, got, ok, tt.want)
			}
		})
	}
	if got, ok := upstreamDistURL(zipball+composerCommitB, "zip", composerCommitA+".zip", false); ok {
		t.Errorf("tagged release rebuilt to %q, want only the listed archive", got)
	}
	if got, ok := upstreamDistURL(gitlab+composerCommitB, "zip", "archive.zip?sha="+composerCommitA, false); ok {
		t.Errorf("tagged GitLab release rebuilt to %q, want only the listed archive", got)
	}
}

func TestComposerRewriteMetadataCooldown(t *testing.T) {
	now := time.Now()
	old := now.Add(-10 * 24 * time.Hour).Format(time.RFC3339)
	recent := now.Add(-1 * time.Hour).Format(time.RFC3339)

	proxy := &Proxy{Logger: slog.Default()}
	proxy.Cooldown = &cooldown.Config{Default: "3d"}

	h := &ComposerHandler{
		proxy:    proxy,
		proxyURL: "http://localhost:8080",
	}

	input := `{
		"packages": {
			"symfony/console": [
				{
					"version": "5.0.0",
					"time": "` + old + `",
					"dist": {"url": "https://repo.packagist.org/5.0.0.zip", "type": "zip"}
				},
				{
					"version": "6.0.0",
					"time": "` + recent + `",
					"dist": {"url": "https://repo.packagist.org/6.0.0.zip", "type": "zip"}
				}
			]
		}
	}`

	output, err := h.rewriteMetadata([]byte(input))
	if err != nil {
		t.Fatalf("rewriteMetadata failed: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	packages := result["packages"].(map[string]any)
	versions := packages["symfony/console"].([]any)

	if len(versions) != 1 {
		t.Fatalf("expected 1 version after cooldown, got %d", len(versions))
	}

	v := versions[0].(map[string]any)
	if v["version"] != "5.0.0" {
		t.Errorf("expected version 5.0.0, got %v", v["version"])
	}
}

func TestComposerRetentionKey(t *testing.T) {
	tests := []struct {
		key  string
		want string
		ok   bool
	}{
		{"pkg:composer/symfony/console", "pkg:composer/symfony/console", true},
		{"pkg:composer/Symfony/Console", "pkg:composer/symfony/console", true},
		{"pkg:composer/console", "", false},
		{"pkg:composer/symfony/console/extra", "", false},
		{"pkg:npm/symfony/console", "", false},
	}
	for _, tt := range tests {
		p, err := purl.Parse(tt.key)
		if err != nil {
			t.Fatalf("Parse(%s): %v", tt.key, err)
		}
		got, ok := composerRetentionKey(p)
		if ok != tt.ok || (ok && got != tt.want) {
			t.Errorf("composerRetentionKey(%s) = %q, %v, want %q, %v", tt.key, got, ok, tt.want, tt.ok)
		}
	}
	if _, ok := composerRetentionKey(nil); ok {
		t.Error("composerRetentionKey(nil) accepted")
	}
}

// TestComposerDownloadCooldown checks that an archive still in cooldown is
// withheld when a lock file requests it directly, without the metadata that
// would have filtered it out. In minified metadata a version inherits the
// time of the entry before it.
func TestComposerDownloadCooldown(t *testing.T) {
	now := time.Now()
	old := now.Add(-10 * 24 * time.Hour).Format(time.RFC3339)
	recent := now.Add(-1 * time.Hour).Format(time.RFC3339)

	var archiveRequests atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dist := func(version string) string {
			return fmt.Sprintf(`"dist":{"url":%q,"type":"zip"}`, srv.URL+"/archives/"+version+".zip")
		}
		switch r.URL.Path {
		case "/p2/v/p.json":
			_, _ = fmt.Fprintf(w, `{"minified":"composer/2.0","packages":{"v/p":[`+
				`{"version":"2.0.0","time":%q,%s},`+
				`{"version":"1.9.0",%s},`+
				`{"version":"1.0.0","time":%q,%s}]}}`,
				recent, dist("2.0.0"), dist("1.9.0"), old, dist("1.0.0"))
		case "/p2/v/p~dev.json":
			http.NotFound(w, r)
		default:
			archiveRequests.Add(1)
			_, _ = io.WriteString(w, "archive "+r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	proxy, _, _, _ := setupTestProxy(t)
	proxy.Cooldown = &cooldown.Config{Default: "3d"}
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(srv.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	h := NewComposerHandlerWithUpstreams(proxy, "http://proxy", srv.URL, srv.URL).Routes()

	tests := []struct {
		version    string
		wantStatus int
	}{
		{"2.0.0", http.StatusNotFound},
		{"1.9.0", http.StatusNotFound},
		{"1.0.0", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			archiveRequests.Store(0)
			req := httptest.NewRequest(http.MethodGet, "/files/v/p/"+tt.version+"/"+tt.version+".zip", nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", w.Code, tt.wantStatus, w.Body.String())
			}
			wantRequests := int32(0)
			if tt.wantStatus == http.StatusOK {
				wantRequests = 1
			}
			if got := archiveRequests.Load(); got != wantRequests {
				t.Errorf("upstream archive requests = %d, want %d", got, wantRequests)
			}
		})
	}
}
