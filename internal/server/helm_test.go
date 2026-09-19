package server

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/go-chi/chi/v5"
)

func TestHelmIndexUsesConfiguredOCIRegistries(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/index.yaml" {
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
		_, _ = fmt.Fprintf(w, "apiVersion: v1\nentries:\n  demo:\n    - digest: %s\n      urls: [oci://ghcr.io/owner/demo:1.0.0]\n", strings.Repeat("a", 64))
	}))
	defer upstream.Close()
	for _, named := range []bool{false, true} {
		t.Run(fmt.Sprintf("named=%t", named), func(t *testing.T) {
			cfg := config.Default()
			cfg.BaseURL = "https://proxy.example"
			cfg.Upstream.Helm = map[string]string{"mixed": upstream.URL}
			cfg.Upstream.OCIDefault = "https://ghcr.io"
			want := "oci://proxy.example/owner/demo:1.0.0"
			if named {
				cfg.Upstream.OCI = map[string]string{"ghcr": "https://ghcr.io"}
				want = "oci://proxy.example/upstream/ghcr/owner/demo:1.0.0"
			}
			p := &handler.Proxy{HTTPClient: upstream.Client(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			s := &Server{cfg: cfg}
			r := chi.NewRouter()
			s.mountProtocolHandlers(r, p)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/helm/mixed/index.yaml", nil))
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), want) {
				t.Fatalf("index status=%d body=%s; want %s", w.Code, w.Body.String(), want)
			}
		})
	}
}
