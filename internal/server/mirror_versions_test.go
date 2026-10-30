package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/git-pkgs/proxy/internal/config"
)

// newPackumentUpstream serves an npm packument with the given versions for
// path and counts the requests it gets.
func newPackumentUpstream(t *testing.T, path string, versions ...string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	entries := make([]string, len(versions))
	for i, v := range versions {
		entries[i] = `"` + v + `":{"version":"` + v + `"}`
	}
	body := `{"name":"x","versions":{` + strings.Join(entries, ",") + `}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.URL.EscapedPath(), path) {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(upstream.Close)
	return upstream, &hits
}

func listMirrorVersions(t *testing.T, cfg *config.Config, ecosystem, name string) ([]string, error) {
	t.Helper()
	cfg.Upstream.AllowLoopback = true
	proxy, _ := NewMirrorProxy(cfg, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	versions, err := NewMirrorVersions(cfg.Upstream, proxy.HTTPClient).ListVersions(context.Background(), ecosystem, name)
	slices.Sort(versions)
	return versions, err
}

func TestMirrorVersionsComeFromConfiguredUpstream(t *testing.T) {
	upstream, hits := newPackumentUpstream(t, "/left-pad", "1.0.0", "1.1.0")
	cfg := config.Default()
	cfg.Upstream.NPM = upstream.URL

	versions, err := listMirrorVersions(t, cfg, "npm", "left-pad")
	if err != nil {
		t.Fatalf("ListVersions() error = %v", err)
	}
	if !slices.Equal(versions, []string{"1.0.0", "1.1.0"}) {
		t.Errorf("versions = %v, want the configured upstream's 1.0.0 and 1.1.0", versions)
	}
	if hits.Load() == 0 {
		t.Error("the configured npm upstream was never asked")
	}
}

func TestMirrorVersionsFollowNPMRoutes(t *testing.T) {
	public, publicHits := newPackumentUpstream(t, "/@acme%2fwidget", "9.9.9")
	private, _ := newPackumentUpstream(t, "/@acme%2fwidget", "1.0.0")
	cfg := config.Default()
	cfg.Upstream.NPM = public.URL
	cfg.Upstream.NPMRoutes = map[string]string{"@acme/*": private.URL}

	versions, err := listMirrorVersions(t, cfg, "npm", "@acme/widget")
	if err != nil {
		t.Fatalf("ListVersions() error = %v", err)
	}
	if !slices.Equal(versions, []string{"1.0.0"}) {
		t.Errorf("versions = %v, want only the routed registry's 1.0.0", versions)
	}
	if publicHits.Load() != 0 {
		t.Error("a routed package was listed from upstream.npm")
	}
}

func TestMirrorVersionsURL(t *testing.T) {
	custom := config.Default().Upstream
	custom.NPM = "https://npm.example.com/"
	custom.Gem = "https://gems.example.com"
	custom.Go = "https://goproxy.example.com"
	custom.HexAPI = "https://hex.example.com"
	custom.Pub = "https://pub.example.com"
	custom.PyPI = "https://pypi.example.com"
	custom.Maven = "https://maven.example.com/maven2"
	custom.NuGet = "https://nuget.example.com"
	custom.Composer = "https://packagist.example.com"
	custom.ComposerRoutes = map[string]string{"acme/*": "https://composer.example.com"}
	custom.CRAN = "https://cran.example.com"
	custom.HomebrewAPI = "https://brew.example.com/api"
	custom.Helm = map[string]string{"stable": "https://charts.example.com/stable/"}
	custom.Cargo = "https://index.example.com"
	custom.Conda = "https://conda.example.com"
	custom.Julia = "https://julia.example.com"
	custom.Swift = "https://swift.example.com/registry/"

	tests := []struct {
		name      string
		upstream  config.UpstreamConfig
		ecosystem string
		pkg       string
		want      string
		wantErr   string
	}{
		{"npm default", config.Default().Upstream, "npm", "left-pad", "https://registry.npmjs.org", ""},
		{"npm unset", config.UpstreamConfig{}, "npm", "left-pad", "https://registry.npmjs.org", ""},
		{"npm", custom, "npm", "left-pad", "https://npm.example.com", ""},
		{"gem", custom, "gem", "rails", "https://gems.example.com", ""},
		{"golang", custom, "golang", "golang.org/x/mod", "https://goproxy.example.com", ""},
		{"hex uses the API URL", custom, "hex", "jason", "https://hex.example.com", ""},
		{"pub", custom, "pub", "http", "https://pub.example.com", ""},
		{"pypi", custom, "pypi", "requests", "https://pypi.example.com", ""},
		{"maven", custom, "maven", "org.example:lib", "https://maven.example.com/maven2", ""},
		{"nuget lists through v3", custom, "nuget", "Newtonsoft.Json", "https://nuget.example.com/v3", ""},
		{"composer", custom, "composer", "monolog/monolog", "https://packagist.example.com", ""},
		{"composer route", custom, "composer", "acme/widget", "", "upstream.composer_routes"},
		{"cran", custom, "cran", "ggplot2", "https://cran.example.com", ""},
		{"brew", custom, "brew", "wget", "https://brew.example.com", ""},
		{"cargo default lists from crates.io", config.Default().Upstream, "cargo", "serde", "", ""},
		{"cargo index", custom, "cargo", "serde", "", "upstream.cargo"},
		{"conda default", config.Default().Upstream, "conda", "numpy", "", ""},
		{"conda channel", custom, "conda", "numpy", "", "upstream.conda"},
		{"julia default", config.Default().Upstream, "julia", "JSON", "", ""},
		{"julia server lists from General", custom, "julia", "JSON", "", ""},
		{"helm", custom, "helm", "stable/nginx", "https://charts.example.com/stable", ""},
		{"helm without repository", custom, "helm", "nginx", "", "namespace"},
		{"helm unknown repository", custom, "helm", "other/nginx", "", "upstream.helm"},
		{"swift", custom, "swift", "apple.swift-nio", "https://swift.example.com/registry", ""},
		{"apk", custom, "apk", "curl", "", "give a version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := mirrorVersionsURL(tt.upstream, tt.ecosystem, tt.pkg)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want one mentioning %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("error = %v", err)
			}
			if got != tt.want {
				t.Errorf("URL = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestServerMirrorListsFromConfiguredUpstream(t *testing.T) {
	upstream, hits := newPackumentUpstream(t, "/left-pad", "1.0.0")
	cfg := config.Default()
	cfg.Upstream.NPM = upstream.URL
	cfg.Upstream.AllowLoopback = true
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy, _ := NewMirrorProxy(cfg, nil, nil, logger)
	s := &Server{cfg: cfg, logger: logger, ecr: newECRTokens(logger)}

	m := s.newMirror(proxy, 1)
	if m.Versions == nil {
		t.Fatal("the server's mirror lists versions from the public registries")
	}
	versions, err := m.Versions.ListVersions(context.Background(), "npm", "left-pad")
	if err != nil {
		t.Fatalf("ListVersions() error = %v", err)
	}
	if !slices.Equal(versions, []string{"1.0.0"}) || hits.Load() == 0 {
		t.Errorf("versions = %v, want 1.0.0 from the configured upstream", versions)
	}
}
