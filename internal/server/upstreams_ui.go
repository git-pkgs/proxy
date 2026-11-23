package server

import (
	"net/http"
	"sort"
	"strings"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/handler"
)

// UpstreamsData is the upstreams page: the upstreams each ecosystem's
// requests go to, as configured.
type UpstreamsData struct {
	Layout
	Ecosystems []UpstreamEcosystem
}

// UpstreamEcosystem is one ecosystem on the upstreams page.
type UpstreamEcosystem struct {
	Name      string
	Upstreams []UpstreamRow
}

// UpstreamRow is one configured upstream. It names where clients reach it
// and the config key that sets it, but not its URL or whether credentials
// apply: /ui has no auth of its own, so that would list internal hosts for
// anyone who can reach the proxy.
type UpstreamRow struct {
	// Setting is the config key that sets the upstream, such as
	// upstream.apk, and Name the entry's key in it for a map.
	Setting, Name string
	// Path is where clients reach the upstream through the proxy.
	Path string
}

func (s *Server) handleUpstreams(w http.ResponseWriter, r *http.Request) {
	data := UpstreamsData{
		Layout:     s.layoutFor(r),
		Ecosystems: upstreamEcosystems(s.cfg.BaseURL, &s.cfg.Upstream),
	}
	if err := s.templates.Render(w, "upstreams", data); err != nil {
		s.logger.Error("failed to render upstreams", "error", err)
	}
}

// upstreamSetting is one config key on the upstreams page: a single URL
// or, with named set, a map of them. path is where it is served, with
// {name} for a map entry's key.
type upstreamSetting struct {
	key   string
	path  string
	url   string
	named map[string]string
}

// upstreamEcosystems lists the upstreams u configures, per ecosystem.
// A map the proxy fills with a default when it is empty, such as
// upstream.apk, lists that default.
func upstreamEcosystems(baseURL string, u *config.UpstreamConfig) []UpstreamEcosystem {
	git := make(map[string]string, len(u.Git))
	for name, upstream := range u.Git {
		git[name] = upstream.URL
	}
	single := func(key, path, url string) upstreamSetting { return upstreamSetting{key: key, path: path, url: url} }
	named := func(key, path string, m map[string]string) upstreamSetting {
		return upstreamSetting{key: key, path: path, named: m}
	}
	groups := []struct {
		name     string
		settings []upstreamSetting
	}{
		{"npm", []upstreamSetting{single("upstream.npm", "/npm/", u.NPM), named("upstream.npm_routes", "/npm/", u.NPMRoutes)}},
		{"Cargo", []upstreamSetting{single("upstream.cargo", "/cargo/", u.Cargo), single("upstream.cargo_download", "/cargo/", u.CargoDownload)}},
		{"RubyGems", []upstreamSetting{single("upstream.gem", "/gem/", u.Gem)}},
		{"Go", []upstreamSetting{
			single("upstream.go", "/go/", u.Go),
			named("upstream.go_sumdbs", "/go/sumdb/{name}/", handler.GoSumDBs(u.GoSumDBs)),
			single("upstream.go_vulndb", "/go/vulndb/", u.GoVulnDB),
		}},
		{"Hex", []upstreamSetting{single("upstream.hex", "/hex/", u.Hex), single("upstream.hex_api", "/hex/", u.HexAPI)}},
		{"pub.dev", []upstreamSetting{single("upstream.pub", "/pub/", u.Pub)}},
		{"PyPI", []upstreamSetting{single("upstream.pypi", "/pypi/", u.PyPI), single("upstream.pypi_download", "/pypi/", u.PyPIDownload)}},
		{"Maven", []upstreamSetting{
			single("upstream.maven", "/maven/", u.Maven),
			named("upstream.maven_repositories", "/maven/{name}/", u.MavenRepositories),
			single("upstream.gradle_plugin_portal", "/maven/", u.GradlePluginPortal),
		}},
		{"NuGet", []upstreamSetting{single("upstream.nuget", "/nuget/", u.NuGet), single("upstream.nuget_search", "/nuget/", u.NuGetSearch)}},
		{"Composer", []upstreamSetting{
			single("upstream.composer", "/composer/", u.Composer),
			single("upstream.composer_repository", "/composer/", u.ComposerRepository),
			named("upstream.composer_routes", "/composer/", u.ComposerRoutes),
		}},
		{"Conan", []upstreamSetting{single("upstream.conan", "/conan/", u.Conan)}},
		{"Chef", []upstreamSetting{single("upstream.chef", "/chef/", u.Chef)}},
		{"Conda", []upstreamSetting{single("upstream.conda", "/conda/", u.Conda)}},
		{"CRAN", []upstreamSetting{single("upstream.cran", "/cran/", u.CRAN)}},
		{"Julia", []upstreamSetting{single("upstream.julia", "/julia/", u.Julia)}},
		{"Swift", []upstreamSetting{single("upstream.swift", "/swift/", u.Swift)}},
		{"Homebrew", []upstreamSetting{
			single("upstream.homebrew_api", "/homebrew/", u.HomebrewAPI),
			// Bottles are served by the container handler (see
			// handler.RegisterHomebrewArtifacts).
			single("upstream.homebrew_artifact", "/v2/homebrew/core/", u.HomebrewArtifact),
		}},
		{"OCI", []upstreamSetting{single("upstream.oci_default", "/v2/", u.OCIDefault), named("upstream.oci", "/v2/upstream/{name}/", u.OCI)}},
		{"Helm", []upstreamSetting{named("upstream.helm", "/helm/{name}/", u.Helm)}},
		{"Alpine", []upstreamSetting{named("upstream.apk", "/apk/{name}/", handler.APKRepositories(u.APK))}},
		{"Arch Linux", []upstreamSetting{named("upstream.arch", "/arch/{name}/", handler.ArchRepositories(u.Arch))}},
		{"FreeBSD", []upstreamSetting{named("upstream.freebsd", "/freebsd/{name}/", handler.FreeBSDRepositories(u.FreeBSD))}},
		{"Terraform", []upstreamSetting{named("upstream.terraform", "/terraform/providers/{name}/", handler.TerraformRegistries(u.Terraform))}},
		{"Debian", []upstreamSetting{single("upstream.debian", "/debian/", u.Debian), named("upstream.debian_repositories", "/debian/{name}/", u.DebianRepositories)}},
		{"RPM", []upstreamSetting{single("upstream.rpm", "/rpm/", u.RPM)}},
		{"Generic", []upstreamSetting{named("upstream.generic", "/generic/{name}/", u.Generic)}},
		{"Git", []upstreamSetting{named("upstream.git", "/git/{name}/", git)}},
	}
	base := strings.TrimSuffix(baseURL, "/")
	ecosystems := make([]UpstreamEcosystem, 0, len(groups))
	for _, group := range groups {
		var rows []UpstreamRow
		for _, setting := range group.settings {
			rows = append(rows, setting.rows(base)...)
		}
		if len(rows) > 0 {
			ecosystems = append(ecosystems, UpstreamEcosystem{Name: group.name, Upstreams: rows})
		}
	}
	return ecosystems
}

// rows lists the setting's upstreams, map entries sorted by name, leaving
// out empty ones.
func (s upstreamSetting) rows(base string) []UpstreamRow {
	row := func(name string) UpstreamRow {
		return UpstreamRow{
			Setting: s.key,
			Name:    name,
			Path:    base + strings.ReplaceAll(s.path, "{name}", name),
		}
	}
	if s.named == nil {
		if s.url == "" {
			return nil
		}
		return []UpstreamRow{row("")}
	}
	names := make([]string, 0, len(s.named))
	for name := range s.named {
		names = append(names, name)
	}
	sort.Strings(names)
	rows := make([]UpstreamRow, 0, len(names))
	for _, name := range names {
		rows = append(rows, row(name))
	}
	return rows
}
