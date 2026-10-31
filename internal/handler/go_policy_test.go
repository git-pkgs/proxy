package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/proxy/internal/config"
)

func TestGoCooldownOverridesIgnoreModuleCase(t *testing.T) {
	for _, tc := range []struct {
		name, rule, fallback, override string
		status                         int
	}{
		{"lowercase restriction", "example.com/module", "0", "7d", http.StatusForbidden},
		{"mixed-case restriction", "example.com/Module", "0", "7d", http.StatusForbidden},
		{"lowercase exemption", "example.com/module", "7d", "0", http.StatusOK},
		{"mixed-case exemption", "example.com/Module", "7d", "0", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, ".info") {
					_ = json.NewEncoder(w).Encode(goInfo{Version: "v1.0.0", Time: time.Now()})
					return
				}
				_, _ = w.Write([]byte("module zip"))
			}))
			defer upstream.Close()
			h, proxy := newGoCaseHandler(t, upstream)
			cfg := config.CooldownConfig{Packages: map[string]string{"pkg:golang/" + tc.rule: tc.override}}
			proxy.Cooldown = &cooldown.Config{Default: tc.fallback, Packages: cfg.NormalizedPackages()}
			for _, path := range []string{
				"/example.com/!module/@v/v1.0.0.zip", "/example.com/module/@v/v1.0.0.zip",
				"/example.com/!module/@v/v1.0.0.info", "/example.com/module/@v/v1.0.0.info",
			} {
				if response := getGo(h, path); response.Code != tc.status {
					t.Errorf("GET %s = %d %s, want %d", path, response.Code, response.Body, tc.status)
				}
			}
		})
	}
}
