package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/go-chi/chi/v5"
)

func TestGoVulnDBReachesGoHandler(t *testing.T) {
	const dbJSON = `{"modified":"2026-10-21T00:27:29Z"}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/index/db.json" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, dbJSON)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.BaseURL = "https://proxy.example"
	cfg.Upstream.GoVulnDB = upstream.URL
	p := &handler.Proxy{HTTPClient: upstream.Client(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s := &Server{cfg: cfg}
	r := chi.NewRouter()
	s.mountProtocolHandlers(r, p)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/go/vulndb/index/db.json", nil))
	if w.Code != http.StatusOK || w.Body.String() != dbJSON {
		t.Errorf("status = %d, body = %q; want index/db.json from upstream.go_vulndb", w.Code, w.Body.String())
	}
}
