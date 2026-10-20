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

func TestPubRewriteMetadata(t *testing.T) {
	h := &PubHandler{
		proxy:    testProxy(),
		proxyURL: "http://localhost:8080",
	}

	input := `{
		"name": "flutter_bloc",
		"versions": [
			{"version": "1.0.0", "archive_url": "https://pub.dev/packages/flutter_bloc/versions/1.0.0.tar.gz"},
			{"version": "2.0.0", "archive_url": "https://pub.dev/packages/flutter_bloc/versions/2.0.0.tar.gz"}
		]
	}`

	output, err := h.rewriteMetadata("flutter_bloc", []byte(input))
	if err != nil {
		t.Fatalf("rewriteMetadata failed: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	versions := result["versions"].([]any)
	if len(versions) != 2 {
		t.Fatalf("expected 2 versions, got %d", len(versions))
	}

	v1 := versions[0].(map[string]any)
	if v1["archive_url"] != "http://localhost:8080/pub/packages/flutter_bloc/versions/1.0.0.tar.gz" {
		t.Errorf("unexpected archive_url: %s", v1["archive_url"])
	}
}

func TestPubRewriteMetadataCooldown(t *testing.T) {
	now := time.Now()
	old := now.Add(-10 * 24 * time.Hour).Format(time.RFC3339)
	recent := now.Add(-1 * time.Hour).Format(time.RFC3339)

	proxy := &Proxy{Logger: slog.Default()}
	proxy.Cooldown = &cooldown.Config{Default: "3d"}

	h := &PubHandler{
		proxy:    proxy,
		proxyURL: "http://localhost:8080",
	}

	input := `{
		"name": "flutter_bloc",
		"latest": {"version": "2.0.0"},
		"versions": [
			{"version": "1.0.0", "published": "` + old + `", "archive_url": "https://pub.dev/1.0.0.tar.gz"},
			{"version": "2.0.0", "published": "` + recent + `", "archive_url": "https://pub.dev/2.0.0.tar.gz"}
		]
	}`

	output, err := h.rewriteMetadata("flutter_bloc", []byte(input))
	if err != nil {
		t.Fatalf("rewriteMetadata failed: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	versions := result["versions"].([]any)
	if len(versions) != 1 {
		t.Fatalf("expected 1 version after cooldown, got %d", len(versions))
	}

	v := versions[0].(map[string]any)
	if v["version"] != "1.0.0" {
		t.Errorf("expected version 1.0.0, got %v", v["version"])
	}

	// latest should be updated
	latest := result["latest"].(map[string]any)
	if latest["version"] != "1.0.0" {
		t.Errorf("latest version = %v, want 1.0.0", latest["version"])
	}
}

// Without the .tar.gz suffix pub.dev answers with the HTML version page, which
// would otherwise be cached under the archive's key.
func TestPubDownloadRequiresArchiveSuffix(t *testing.T) {
	proxy, db, _, fetcher := setupTestProxy(t)
	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("<html>version page</html>")),
		ContentType: "text/html",
	}
	h := NewPubHandlerWithUpstream(proxy, "http://localhost", "https://pub.example.com")

	req := httptest.NewRequest(http.MethodGet, "/packages/http/versions/1.2.0", nil)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	if fetcher.fetchCalled {
		t.Errorf("upstream fetched %q", fetcher.fetchedURL)
	}
	art, err := db.GetArtifact("pkg:pub/http@1.2.0", "http-1.2.0.tar.gz")
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	if art != nil {
		t.Errorf("artifact cached from %q", art.UpstreamURL)
	}
}

func TestPubDownloadRejectsEncodedQueryInName(t *testing.T) {
	proxy, _, _, fetcher := setupTestProxy(t)
	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("archive")),
		ContentType: "application/octet-stream",
	}
	h := NewPubHandlerWithUpstream(proxy, "http://localhost", "https://pub.example.com")

	for _, path := range []string{
		"/packages/http%3Fx/versions/1.2.0.tar.gz",
		"/packages/http%23x/versions/1.2.0.tar.gz",
		"/packages/http/versions/1.2.0%3Fx.tar.gz",
	} {
		fetcher.fetchCalled = false
		fetcher.fetchedURL = ""
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		h.Routes().ServeHTTP(w, req)

		if strings.ContainsAny(fetcher.fetchedURL, "?#") {
			t.Errorf("%s: upstream URL %q carries a query or fragment", path, fetcher.fetchedURL)
		}
		if strings.Contains(path, "/http%") && fetcher.fetchCalled {
			t.Errorf("%s: invalid package name reached upstream as %q", path, fetcher.fetchedURL)
		}
	}
}

// Older versions cached the HTML version page as the archive. Such an entry
// is fetched again instead of being served.
func TestPubDownloadRefetchesCachedVersionPage(t *testing.T) {
	proxy, db, store, fetcher := setupTestProxy(t)
	seedPackage(t, db, store, "pub", "http", "1.2.0", "http-1.2.0.tar.gz", "<html>version page</html>")
	art, err := db.GetArtifact("pkg:pub/http@1.2.0", "http-1.2.0.tar.gz")
	if err != nil || art == nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	art.UpstreamURL = "https://pub.dev/packages/http/versions/1.2.0"
	if err := db.UpsertArtifact(art); err != nil {
		t.Fatalf("UpsertArtifact: %v", err)
	}
	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("archive")),
		ContentType: "application/octet-stream",
	}
	h := NewPubHandlerWithUpstream(proxy, "http://localhost", "https://pub.example.com")

	req := httptest.NewRequest(http.MethodGet, "/packages/http/versions/1.2.0.tar.gz", nil)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if !fetcher.fetchCalled {
		t.Fatal("cached version page was served without fetching upstream")
	}
	if got := w.Body.String(); got != "archive" {
		t.Errorf("body = %q, want %q", got, "archive")
	}
}

func TestPubDownloadServesCachedArchive(t *testing.T) {
	proxy, db, store, fetcher := setupTestProxy(t)
	seedPackage(t, db, store, "pub", "http", "1.2.0", "http-1.2.0.tar.gz", "cached archive")
	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("refetched")),
		ContentType: "application/octet-stream",
	}
	h := NewPubHandlerWithUpstream(proxy, "http://localhost", "https://pub.example.com")

	req := httptest.NewRequest(http.MethodGet, "/packages/http/versions/1.2.0.tar.gz", nil)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if fetcher.fetchCalled {
		t.Errorf("cached archive fetched again from %q", fetcher.fetchedURL)
	}
	if got := w.Body.String(); got != "cached archive" {
		t.Errorf("body = %q, want %q", got, "cached archive")
	}
}
