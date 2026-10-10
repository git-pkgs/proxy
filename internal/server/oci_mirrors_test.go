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

func TestOCIMirrorsReachContainerHandler(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/art/v2/owner/app/manifests/latest" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		_, _ = io.WriteString(w, `{"schemaVersion":2}`)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.BaseURL = "https://proxy.example"
	cfg.Upstream.OCI = map[string]string{"art": upstream.URL + "/art"}
	cfg.Upstream.OCIMirrors = map[string][]string{"art": {"ghcr.io"}}
	p := &handler.Proxy{HTTPClient: upstream.Client(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s := &Server{cfg: cfg}
	r := chi.NewRouter()
	s.mountProtocolHandlers(r, p)

	for _, target := range []string{
		"/v2/upstream/art/owner/app/manifests/latest?ns=ghcr.io",
		"/v2/owner/app/manifests/latest?ns=ghcr.io",
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
		if w.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200 through upstream.oci_mirrors: %s", target, w.Code, w.Body.String())
		}
	}
}
