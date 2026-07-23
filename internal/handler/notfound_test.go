package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/git-pkgs/registries/fetch"
)

func TestErrUpstreamNotFoundWrapsFetchErrNotFound(t *testing.T) {
	if !errors.Is(ErrUpstreamNotFound, fetch.ErrNotFound) {
		t.Fatal("ErrUpstreamNotFound does not wrap fetch.ErrNotFound")
	}
}

func TestGetOrFetchArtifactFromURL_NotFound(t *testing.T) {
	proxy, _, _, fetcher := setupTestProxy(t)
	fetcher.fetchErr = fetch.ErrNotFound

	_, err := proxy.GetOrFetchArtifactFromURL(context.Background(),
		"maven", "org.example:missing", "1.0", "missing-1.0.jar",
		"http://upstream.test/org/example/missing/1.0/missing-1.0.jar")

	if !errors.Is(err, ErrUpstreamNotFound) {
		t.Fatalf("want ErrUpstreamNotFound, got %v", err)
	}
}

func TestMavenHandler_UpstreamNotFoundReturns404(t *testing.T) {
	proxy, _, _, fetcher := setupTestProxy(t)
	fetcher.fetchErr = fetch.ErrNotFound

	h := NewMavenHandler(proxy, "http://localhost", "http://upstream.test", "http://portal.test")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/org/example/missing/1.0/missing-1.0.jar")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("want 404 for missing upstream artifact, got %d", resp.StatusCode)
	}
}

func TestMavenHandler_PluginPortalFallback(t *testing.T) {
	proxy, _, _, fetcher := setupTestProxy(t)
	fetcher.fetchErrByURL = map[string]error{
		"http://upstream.test/org/example/plugin/1.0/plugin-1.0.jar": fetch.ErrNotFound,
	}
	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("portal artifact")),
		ContentType: "application/java-archive",
	}

	h := NewMavenHandler(proxy, "http://localhost", "http://upstream.test", "http://portal.test")
	srv := httptest.NewServer(h.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/org/example/plugin/1.0/plugin-1.0.jar")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200 via plugin portal fallback, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "portal artifact" {
		t.Errorf("want portal artifact body, got %q", body)
	}
	if fetcher.fetchedURL != "http://portal.test/org/example/plugin/1.0/plugin-1.0.jar" {
		t.Errorf("fallback did not hit plugin portal, last URL: %s", fetcher.fetchedURL)
	}
}
