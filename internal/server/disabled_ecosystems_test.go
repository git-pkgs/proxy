package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/storage"
	"github.com/git-pkgs/registries/fetch"
	"github.com/go-chi/chi/v5"
)

// ecosystemPaths is a request under each disableable ecosystem's route.
var ecosystemPaths = map[string]string{
	"alpine":    "/apk/alpine/v3.22/main/x86_64/APKINDEX.tar.gz",
	"arch":      "/arch/archlinux/core/os/x86_64/core.db",
	"cargo":     "/cargo/config.json",
	"chef":      "/chef/api/v1/cookbooks/nginx",
	"composer":  "/composer/packages.json",
	"conan":     "/conan/v2/ping",
	"conda":     "/conda/conda-forge/noarch/repodata.json",
	"cran":      "/cran/src/contrib/PACKAGES",
	"deb":       "/debian/dists/bookworm/InRelease",
	"freebsd":   "/freebsd/freebsd/FreeBSD:14:amd64/quarterly/meta.conf",
	"gem":       "/gem/specs.4.8.gz",
	"golang":    "/go/github.com/x/y/@v/list",
	"gradle":    "/gradle/cache/abc",
	"helm":      "/helm/bitnami/index.yaml",
	"hex":       "/hex/names",
	"homebrew":  "/homebrew/api/formula.jws.json",
	"julia":     "/julia/registries",
	"maven":     "/maven/org/x/y/maven-metadata.xml",
	"npm":       "/npm/lodash",
	"nuget":     "/nuget/v3/index.json",
	"oci":       "/v2/",
	"pub":       "/pub/api/packages/http",
	"pypi":      "/pypi/simple/requests/",
	"rpm":       "/rpm/releases/43/Everything/x86_64/os/repodata/repomd.xml",
	"swift":     "/swift/apple/swift-argument-parser",
	"terraform": "/terraform/providers/registry.terraform.io/hashicorp/aws/index.json",
}

func newDisabledRouter(t *testing.T, client *http.Client, disabled ...string) *chi.Mux {
	t.Helper()
	db, err := database.Create(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("creating database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := storage.OpenBucket(context.Background(), "file://"+t.TempDir())
	if err != nil {
		t.Fatalf("opening storage: %v", err)
	}
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(client), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	proxy.HTTPClient = client

	cfg := config.Default()
	cfg.BaseURL = "http://proxy.example"
	cfg.DisabledEcosystems = disabled
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	s := &Server{cfg: cfg}
	r := chi.NewRouter()
	s.mountProtocolHandlers(r, proxy)
	return r
}

func isDisabledResponse(w *httptest.ResponseRecorder) bool {
	return w.Code == http.StatusNotFound && strings.Contains(w.Body.String(), "turned off")
}

// Every disableable ecosystem's route answers 404 without contacting
// upstream when it is turned off, and only then.
func TestDisabledEcosystemRoutesAnswer404(t *testing.T) {
	for _, key := range config.DisableableEcosystems {
		path, ok := ecosystemPaths[key]
		if !ok {
			t.Errorf("%s: no request path in the test", key)
			continue
		}
		t.Run(key, func(t *testing.T) {
			transport := &countingTransport{}
			r := newDisabledRouter(t, &http.Client{Transport: transport}, key)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			if !isDisabledResponse(w) {
				t.Errorf("GET %s: status %d, body %q; want the turned-off 404", path, w.Code, w.Body.String())
			}
			if n := transport.hits.Load(); n != 0 {
				t.Errorf("upstream was contacted %d times", n)
			}

			enabled := newDisabledRouter(t, &http.Client{Transport: &countingTransport{}})
			w = httptest.NewRecorder()
			enabled.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			if isDisabledResponse(w) {
				t.Errorf("GET %s answers turned off with nothing disabled", path)
			}
		})
	}
}

// Turning one ecosystem off leaves the others served.
func TestDisabledEcosystemLeavesOthers(t *testing.T) {
	r := newDisabledRouter(t, &http.Client{Transport: &countingTransport{}}, "pypi")
	for key, path := range ecosystemPaths {
		if key == "pypi" {
			continue
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if isDisabledResponse(w) {
			t.Errorf("GET %s answers turned off with only pypi disabled", path)
		}
	}
}

// The install page leaves out turned-off ecosystems, and each of its
// entries names an ecosystem disabled_ecosystems accepts.
func TestInstallPageLeavesOutDisabledEcosystems(t *testing.T) {
	cfg := config.Default()
	cfg.BaseURL = "http://proxy.example"
	all := installRegistries(cfg)
	if len(all) != len(getRegistryConfigs(cfg.BaseURL)) {
		t.Fatalf("installRegistries left out entries with nothing disabled")
	}
	for _, reg := range all {
		cfg := config.Default()
		cfg.BaseURL = "http://proxy.example"
		cfg.DisabledEcosystems = []string{reg.ID}
		if err := cfg.Validate(); err != nil {
			t.Errorf("%s: %v", reg.ID, err)
			continue
		}
		got := installRegistries(cfg)
		if len(got) != len(all)-1 || slices.ContainsFunc(got, func(r RegistryConfig) bool { return r.ID == reg.ID }) {
			t.Errorf("disabling %s: install page lists %d of %d entries", reg.ID, len(got), len(all))
		}
	}
}

// Mirror jobs and schedules refuse what disabled_ecosystems turns off,
// given as PURL types.
func TestServerMirrorRefusesDisabledEcosystems(t *testing.T) {
	cfg := config.Default()
	cfg.DisabledEcosystems = []string{"oci", "npm"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy, _ := NewMirrorProxy(cfg, nil, nil, logger)
	s := &Server{cfg: cfg, logger: logger, ecr: newECRTokens(logger)}

	m := s.newMirror(proxy, 1)
	if m.Disabled == nil {
		t.Fatal("Disabled is nil")
	}
	for eco, want := range map[string]bool{"npm": true, "docker": true, "oci": true, "cargo": false} {
		if got := m.Disabled(eco); got != want {
			t.Errorf("Disabled(%q) = %t, want %t", eco, got, want)
		}
	}
}

// A change feed isn't followed for a turned-off ecosystem.
func TestChangeFeedsSkipDisabledEcosystems(t *testing.T) {
	cfg := config.Default()
	cfg.Upstream.NPMChangeFeed = true
	cfg.Upstream.ComposerChangeFeed = true
	s := &Server{cfg: cfg}
	if npm, composer := s.changeFeedsOn(); !npm || !composer {
		t.Fatalf("changeFeedsOn = %t, %t with nothing disabled; want both", npm, composer)
	}
	cfg.DisabledEcosystems = []string{"npm"}
	if npm, composer := s.changeFeedsOn(); npm || !composer {
		t.Errorf("changeFeedsOn = %t, %t with npm disabled; want composer only", npm, composer)
	}
	cfg.DisabledEcosystems = []string{"composer"}
	if npm, composer := s.changeFeedsOn(); !npm || composer {
		t.Errorf("changeFeedsOn = %t, %t with composer disabled; want npm only", npm, composer)
	}
}
