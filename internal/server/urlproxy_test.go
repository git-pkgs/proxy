package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/handler"
)

func newURLProxyForTest(t *testing.T, upstream config.UpstreamConfig) *handler.Proxy {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &Server{cfg: &config.Config{Upstream: upstream}, logger: logger}
	p := s.newURLProxy(handler.NewProxy(nil, nil, nil, nil, logger))
	t.Cleanup(func() {
		if c, ok := p.Fetcher.(io.Closer); ok {
			_ = c.Close()
		}
	})
	return p
}

func TestURLProxyRefusesLoopbackTargets(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = io.WriteString(w, "internal")
	}))
	defer srv.Close()

	p := newURLProxyForTest(t, config.UpstreamConfig{})
	_, err := p.Fetcher.Fetch(context.Background(), srv.URL+"/secret")
	if err == nil {
		t.Fatal("fetching a loopback target succeeded")
	}
	t.Logf("refused: %v", err)
	if hits != 0 {
		t.Errorf("loopback target received %d requests", hits)
	}
}

func TestURLProxySendsNoConfiguredCredentials(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	p := newURLProxyForTest(t, config.UpstreamConfig{
		AllowLoopback: true,
		Auth:          map[string]config.AuthConfig{srv.URL: {Type: "bearer", Token: "s3cret"}},
	})
	artifact, err := p.Fetcher.Fetch(context.Background(), srv.URL+"/file")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	_ = artifact.Body.Close()
	if gotAuth != "" {
		t.Errorf("upstream received Authorization %q", gotAuth)
	}
}
