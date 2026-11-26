package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/git-pkgs/proxy/internal/config"
)

func TestUpstreamsPage(t *testing.T) {
	cfg, _ := startGitTestServer(t, func(cfg *config.Config) {
		cfg.Upstream.Debian = "https://deb.example.org/debian"
		cfg.Upstream.DebianRepositories = map[string]string{"security": "https://security.example.org/debian-security"}
		cfg.Upstream.Helm = map[string]string{"charts": "https://charts.example.org"}
		cfg.Upstream.Auth = map[string]config.AuthConfig{
			"https://charts.example.org": {Type: "bearer", Token: "chart-token-secret"},
		}
	})

	if _, body := request(t, http.MethodGet, cfg.BaseURL+"/ui/", "", nil); !strings.Contains(body, `href="/ui/upstreams"`) {
		t.Error("navigation doesn't link the upstreams page")
	}
	status, body := request(t, http.MethodGet, cfg.BaseURL+"/ui/upstreams", "", nil)
	if status != http.StatusOK {
		t.Fatalf("status %d\n%s", status, body)
	}
	for _, want := range []string{
		cfg.BaseURL + "/debian/",
		cfg.BaseURL + "/debian/security/",
		cfg.BaseURL + "/helm/charts/",
		cfg.BaseURL + "/npm/",
		// upstream.apk is empty, so the page lists the repository the proxy
		// serves by default.
		cfg.BaseURL + "/apk/alpine/",
		// So is the checksum database with upstream.go_sumdbs empty.
		cfg.BaseURL + "/go/sumdb/sum.golang.org/",
		cfg.BaseURL + "/v2/homebrew/core/",
		"upstream.debian_repositories · security",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page doesn't show %q", want)
		}
	}
	for _, hidden := range []string{"deb.example.org", "security.example.org", "charts.example.org", "registry.npmjs.org", "dl-cdn.alpinelinux.org", "chart-token-secret", "bearer"} {
		if strings.Contains(body, hidden) {
			t.Errorf("page shows %q", hidden)
		}
	}
}

func TestUpstreamEcosystems(t *testing.T) {
	u := config.Default().Upstream
	u.NPM = "https://user:url-password@npm.example.org/registry?token=query-secret#frag"
	u.Generic = map[string]string{
		"files":    "https://files.example.org/base",
		"my files": "https://morefiles.example.org/base",
	}
	u.Git = map[string]config.GitUpstream{"github": {URL: "https://github.com"}}
	u.GoSumDBs = map[string]string{"sum.example.org": "https://sum.example.org"}
	u.Auth = map[string]config.AuthConfig{
		"https://npm.example.org":   {Type: "basic", Username: "basic-user", Password: "basic-secret"},
		"https://files.example.org": {Type: "header", HeaderName: "X-Api-Key", HeaderValue: "header-secret"},
	}
	ecosystems := upstreamEcosystems("http://proxy.test/", &u)

	rows := map[string]UpstreamRow{}
	for _, e := range ecosystems {
		for _, row := range e.Upstreams {
			rows[row.Setting+" "+row.Name] = row
		}
	}
	// host is what the status is looked up by: no userinfo, no path.
	for key, want := range map[string]UpstreamRow{
		"upstream.npm ":                            {Setting: "upstream.npm", Path: "http://proxy.test/npm/", host: "npm.example.org"},
		"upstream.generic files":                   {Setting: "upstream.generic", Name: "files", Path: "http://proxy.test/generic/files/", host: "files.example.org"},
		"upstream.generic my files":                {Setting: "upstream.generic", Name: "my files", Path: "http://proxy.test/generic/my%20files/", host: "morefiles.example.org"},
		"upstream.git github":                      {Setting: "upstream.git", Name: "github", Path: "http://proxy.test/git/github/", host: "github.com"},
		"upstream.go_sumdbs sum.example.org":       {Setting: "upstream.go_sumdbs", Name: "sum.example.org", Path: "http://proxy.test/go/sumdb/sum.example.org/", host: "sum.example.org"},
		"upstream.arch archlinux":                  {Setting: "upstream.arch", Name: "archlinux", Path: "http://proxy.test/arch/archlinux/", host: "geo.mirror.pkgbuild.com"},
		"upstream.freebsd freebsd":                 {Setting: "upstream.freebsd", Name: "freebsd", Path: "http://proxy.test/freebsd/freebsd/", host: "pkg.freebsd.org"},
		"upstream.cargo_download ":                 {Setting: "upstream.cargo_download", Path: "http://proxy.test/cargo/", host: "static.crates.io"},
		"upstream.homebrew_api ":                   {Setting: "upstream.homebrew_api", Path: "http://proxy.test/homebrew/", host: "formulae.brew.sh"},
		"upstream.homebrew_artifact ":              {Setting: "upstream.homebrew_artifact", Path: "http://proxy.test/v2/homebrew/core/", host: "ghcr.io"},
		"upstream.terraform registry.opentofu.org": {Setting: "upstream.terraform", Name: "registry.opentofu.org", Path: "http://proxy.test/terraform/providers/registry.opentofu.org/", host: "registry.opentofu.org"},
	} {
		if got := rows[key]; got != want {
			t.Errorf("%s = %+v, want %+v", key, got, want)
		}
	}
	if _, ok := rows["upstream.go_sumdbs sum.golang.org"]; ok {
		t.Error("lists the default checksum database although upstream.go_sumdbs replaces it")
	}
	if _, ok := rows["upstream.helm "]; ok {
		t.Error("lists an empty upstream.helm")
	}
	for _, e := range ecosystems {
		if len(e.Upstreams) == 0 {
			t.Errorf("lists %s without upstreams", e.Name)
		}
	}
}

func TestUpstreamEcosystemsSortNames(t *testing.T) {
	u := config.Default().Upstream
	u.Generic = map[string]string{}
	for _, name := range []string{"d", "b", "e", "a", "c", "f", "h", "g"} {
		u.Generic[name] = "https://" + name + ".example.org"
	}
	for _, e := range upstreamEcosystems("http://proxy.test", &u) {
		if e.Name != "Generic" {
			continue
		}
		var names []string
		for _, row := range e.Upstreams {
			names = append(names, row.Name)
		}
		if got := strings.Join(names, ""); got != "abcdefgh" {
			t.Errorf("generic upstreams in order %q", got)
		}
	}
}

func TestUpstreamsPageShowsNoSecrets(t *testing.T) {
	u := config.Default().Upstream
	u.NPM = "https://user:url-password@npm.example.org?token=query-secret#frag-secret"
	u.Generic = map[string]string{"files": "https://files.example.org"}
	u.Auth = map[string]config.AuthConfig{
		"https://npm.example.org":   {Type: "basic", Username: "basic-user", Password: "basic-secret"},
		"https://files.example.org": {Type: "header", HeaderName: "X-Api-Key", HeaderValue: "header-secret"},
		"https://pypi.org":          {Type: "bearer", Token: "bearer-secret"},
	}
	w := httptest.NewRecorder()
	if err := (&Templates{}).Render(w, "upstreams", UpstreamsData{Ecosystems: upstreamEcosystems("http://proxy.test", &u)}); err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()
	for _, secret := range []string{"url-password", "user@", "query-secret", "frag-secret", "basic-user", "basic-secret", "X-Api-Key", "header-secret", "bearer-secret"} {
		if strings.Contains(body, secret) {
			t.Errorf("page shows %q", secret)
		}
	}
	// The hosts and which of them have credentials attached aren't shown
	// either: /ui has no auth of its own. ("header" is in the layout.)
	for _, hidden := range []string{"npm.example.org", "files.example.org", "pypi.org", "basic", "bearer", "auth:"} {
		if strings.Contains(body, hidden) {
			t.Errorf("page shows %q", hidden)
		}
	}
	if !strings.Contains(body, "http://proxy.test/generic/files/") {
		t.Error("page doesn't show the generic upstream's path")
	}
}

func TestUpstreamEcosystemsTerraformHostnames(t *testing.T) {
	u := config.Default().Upstream
	u.Terraform = map[string]string{"Registry.Example.ORG": "https://registry.example.org/"}
	var paths []string
	for _, e := range upstreamEcosystems("http://proxy.test", &u) {
		if e.Name != "Terraform" {
			continue
		}
		for _, row := range e.Upstreams {
			paths = append(paths, row.Path)
		}
	}
	if want := "http://proxy.test/terraform/providers/registry.example.org/"; len(paths) != 1 || paths[0] != want {
		t.Errorf("terraform paths %q, want [%q]", paths, want)
	}
}

// Requests through the proxy reach the page's lights: a failing upstream
// shows red, one never asked shows no traffic, and neither shows its host.
func TestUpstreamsPageShowsStatus(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(failing.Close)
	cfg, _ := startGitTestServer(t, func(cfg *config.Config) {
		cfg.Upstream.NPM = failing.URL
	})

	request(t, http.MethodGet, cfg.BaseURL+"/npm/left-pad", "", nil)
	status, body := request(t, http.MethodGet, cfg.BaseURL+"/ui/upstreams", "", nil)
	if status != http.StatusOK {
		t.Fatalf("status %d\n%s", status, body)
	}
	npm := section(t, body, `id="ecosystem-npm"`)
	if !strings.Contains(npm, `data-status="error"`) || !strings.Contains(npm, "requests failed in the last 15 minutes") || !strings.Contains(npm, "HTTP 503") {
		t.Errorf("npm section doesn't show the failing upstream:\n%s", npm)
	}
	if cargo := section(t, body, `id="ecosystem-cargo"`); !strings.Contains(cargo, `data-status="idle"`) || strings.Contains(cargo, `data-status="error"`) {
		t.Errorf("cargo section isn't shown as without traffic:\n%s", cargo)
	}
	if summary := section(t, body, `id="upstream-summary"`); !strings.Contains(summary, `data-status="error"`) || !strings.Contains(summary, "Problems with 1 ecosystem") {
		t.Errorf("summary doesn't show the problem:\n%s", summary)
	}
	if host := strings.TrimPrefix(failing.URL, "http://"); strings.Contains(body, host) {
		t.Errorf("page shows the upstream host %s", host)
	}
}

// section returns body from marker to the next ecosystem card.
func section(t *testing.T, body, marker string) string {
	t.Helper()
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("page has no %s", marker)
	}
	rest := body[i+len(marker):]
	if j := strings.Index(rest, `id="ecosystem-`); j >= 0 {
		rest = rest[:j]
	}
	return rest
}
