package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/git-pkgs/registries/fetch"
)

func artifactSizeFetcher(t *testing.T) (*fetch.Fetcher, string) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "0123456789")
	}))
	t.Cleanup(upstream.Close)
	f := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxArtifactSize(4))
	t.Cleanup(func() { _ = f.Close() })
	return f, upstream.URL
}

func TestOversizedArtifactIsNotCached(t *testing.T) {
	proxy, db, store, _ := setupTestProxy(t)
	limited, upstream := artifactSizeFetcher(t)
	proxy.Fetcher = limited

	_, err := proxy.GetOrFetchArtifactFromURL(context.Background(), "npm", "big", "1.0.0", "big-1.0.0.tgz", upstream+"/big-1.0.0.tgz")
	if !errors.Is(err, fetch.ErrArtifactTooLarge) {
		t.Fatalf("GetOrFetchArtifactFromURL error = %v, want fetch.ErrArtifactTooLarge", err)
	}
	if len(store.files) != 0 {
		t.Errorf("storage holds %d objects after an oversized fetch, want 0", len(store.files))
	}
	fetchErr := err
	record, err := db.GetArtifact("pkg:npm/big@1.0.0", "big-1.0.0.tgz")
	if err != nil {
		t.Fatalf("GetArtifact failed: %v", err)
	}
	if record != nil {
		t.Error("oversized artifact was recorded in the cache database")
	}

	rec := httptest.NewRecorder()
	proxy.serveArtifactError(rec, fetchErr, "failed to fetch package")
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
}

func TestOversizedStreamedArtifactFailsTheStream(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	proxy.StreamArtifacts = true
	limited, upstream := artifactSizeFetcher(t)
	proxy.Fetcher = limited

	result, err := proxy.GetOrFetchArtifactFromURL(context.Background(), "npm", "big", "1.0.0", "big-1.0.0.tgz", upstream+"/big-1.0.0.tgz")
	if err != nil {
		t.Fatalf("GetOrFetchArtifactFromURL failed: %v", err)
	}
	defer func() { _ = result.Reader.Close() }()
	if _, err := io.ReadAll(result.Reader); !errors.Is(err, fetch.ErrArtifactTooLarge) {
		t.Errorf("streamed body error = %v, want fetch.ErrArtifactTooLarge", err)
	}
}
