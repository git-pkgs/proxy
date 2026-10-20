package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	registryclient "github.com/git-pkgs/registries/client"
	"github.com/git-pkgs/registries/fetch"
)

func TestMirrorProxySharesHoldAfterBearerRetry(t *testing.T) {
	var calls atomic.Int32
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/token" {
			_, _ = io.WriteString(w, `{"token":"fixture-token"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="test",scope="repository:test:pull"`, upstream.URL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Retry-After", "9223372036854775807")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()
	cfg := config.Default()
	cfg.Upstream.AllowLoopback = true
	proxy, _ := NewMirrorProxy(cfg, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	response, err := proxy.HTTPClient.Get(upstream.URL + "/v2/test/manifests/latest")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests || calls.Load() != 3 {
		t.Fatalf("bearer retry: status %d, calls %d", response.StatusCode, calls.Load())
	}
	_, err = proxy.Fetcher.Fetch(context.Background(), upstream.URL+"/v2/test/blobs/content")
	if !errors.Is(err, registryclient.ErrUpstreamHeld) {
		t.Fatalf("artifact fetch = %v", err)
	}
	metadata := registryclient.NewClient(registryclient.WithHTTPClient(proxy.HTTPClient))
	metadata.BaseDelay = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = metadata.GetBody(ctx, upstream.URL+"/metadata")
	if !errors.Is(err, registryclient.ErrUpstreamHeld) || calls.Load() != 3 {
		t.Fatalf("metadata fetch = %v, upstream calls %d", err, calls.Load())
	}
	var held *registryclient.HeldError
	if !errors.As(proxy.RateLimits.Held(upstream.URL), &held) || held.RetryAfter < 59*time.Minute || len(proxy.RateLimits.Holds()) != 1 {
		t.Fatalf("shared hold = %+v", held)
	}
}

func TestMirrorProxyAppliesArtifactSizeOption(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "12345")
	}))
	defer upstream.Close()
	cfg := config.Default()
	cfg.Upstream.AllowLoopback = true
	cfg.ArtifactMaxSize = "4"
	proxy, _ := NewMirrorProxy(cfg, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := proxy.Fetcher.Fetch(context.Background(), upstream.URL+"/artifact"); !errors.Is(err, fetch.ErrArtifactTooLarge) {
		t.Fatalf("Fetch = %v", err)
	}
}
