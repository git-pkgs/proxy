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
		"https://deb.example.org/debian",
		cfg.BaseURL + "/debian/",
		"https://security.example.org/debian-security",
		cfg.BaseURL + "/debian/security/",
		"https://charts.example.org",
		cfg.BaseURL + "/helm/charts/",
		"https://registry.npmjs.org",
		cfg.BaseURL + "/npm/",
		// upstream.apk is empty, so the page lists the mirror the proxy uses.
		"https://dl-cdn.alpinelinux.org/alpine",
		cfg.BaseURL + "/apk/alpine/",
		// So is the checksum database with upstream.go_sumdbs empty.
		cfg.BaseURL + "/go/sumdb/sum.golang.org/",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page doesn't show %q", want)
		}
	}
	if strings.Contains(body, "chart-token-secret") {
		t.Error("page shows the upstream token")
	}
}

func TestUpstreamEcosystems(t *testing.T) {
	u := config.Default().Upstream
	u.NPM = "https://user:url-password@npm.example.org/registry?token=query-secret#frag"
	u.Generic = map[string]string{"files": "https://files.example.org/base"}
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
	for key, want := range map[string]UpstreamRow{
		"upstream.npm ":                      {Setting: "upstream.npm", Path: "http://proxy.test/npm/", URL: "https://npm.example.org/registry", Auth: "basic"},
		"upstream.generic files":             {Setting: "upstream.generic", Name: "files", Path: "http://proxy.test/generic/files/", URL: "https://files.example.org/base", Auth: "header"},
		"upstream.git github":                {Setting: "upstream.git", Name: "github", Path: "http://proxy.test/git/github/", URL: "https://github.com"},
		"upstream.go_sumdbs sum.example.org": {Setting: "upstream.go_sumdbs", Name: "sum.example.org", Path: "http://proxy.test/go/sumdb/sum.example.org/", URL: "https://sum.example.org"},
		"upstream.arch archlinux":            {Setting: "upstream.arch", Name: "archlinux", Path: "http://proxy.test/arch/archlinux/", URL: "https://geo.mirror.pkgbuild.com"},
		"upstream.freebsd freebsd":           {Setting: "upstream.freebsd", Name: "freebsd", Path: "http://proxy.test/freebsd/freebsd/", URL: "https://pkg.freebsd.org"},
		"upstream.cargo_download ":           {Setting: "upstream.cargo_download", Path: "http://proxy.test/cargo/", URL: "https://static.crates.io/crates"},
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
	if got := rows["upstream.terraform registry.opentofu.org"].URL; got != "https://registry.opentofu.org" {
		t.Errorf("default Terraform registry = %q", got)
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
	for _, want := range []string{"https://npm.example.org", "basic", "header", "bearer"} {
		if !strings.Contains(body, want) {
			t.Errorf("page doesn't show %q", want)
		}
	}
}

func TestDisplayURLHidesWhatItCannotParse(t *testing.T) {
	for _, raw := range []string{"https://user:pw@[::1", "not a url with secret", ""} {
		if got := displayURL(raw); strings.Contains(got, "secret") || strings.Contains(got, "pw") {
			t.Errorf("displayURL(%q) = %q", raw, got)
		}
	}
}
