package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/registries/fetch"
	"github.com/go-chi/chi/v5"
)

func TestMountProtocolHandlersAppliesPackageRoutes(t *testing.T) {
	routed := make(chan string, 2)
	private := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routed <- r.URL.Path
		http.NotFound(w, r)
	}))
	defer private.Close()

	cfg := config.Default()
	cfg.BaseURL = "http://proxy.test"
	cfg.Upstream.Composer = "http://default.invalid"
	cfg.Upstream.ComposerRepository = "http://default.invalid"
	cfg.Upstream.NPM = "http://default.invalid"
	cfg.Upstream.ComposerRoutes = map[string]string{"example/*": private.URL + "/composer"}
	cfg.Upstream.NPMRoutes = map[string]string{"@example/*": private.URL + "/npm"}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := handler.NewProxy(nil, nil, fetch.NewFetcher(), fetch.NewResolver(), logger)
	proxy.HTTPClient = private.Client()
	s := &Server{cfg: cfg, logger: logger}
	r := chi.NewRouter()
	s.mountProtocolHandlers(r, proxy)

	for path, wantUpstreamPath := range map[string]string{
		"/composer/p2/example/library.json": "/composer/p2/example/library.json",
		"/npm/@example%2fwidgets":           "/npm/@example/widgets",
	} {
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
		select {
		case got := <-routed:
			if got != wantUpstreamPath {
				t.Errorf("%s reached route upstream at %q, want %q", path, got, wantUpstreamPath)
			}
		default:
			t.Errorf("%s did not reach the configured route", path)
		}
	}
}
