package server

import (
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/metrics"
)

// UpstreamsData is the upstreams page: the upstreams each ecosystem's
// requests go to, as configured.
type UpstreamsData struct {
	Layout
	Ecosystems []UpstreamEcosystem
	Summary    StatusSummary
}

// UpstreamEcosystem is one ecosystem on the upstreams page.
type UpstreamEcosystem struct {
	Name      string
	Upstreams []UpstreamRow
	// Key is the ecosystem as the database, metrics and config name it.
	Key string
	// Status is the worst status of the ecosystem's upstreams.
	Status UpstreamStatus
	// Cache and Policy describe the ecosystem rather than one upstream.
	Cache  *EcosystemCache
	Policy EcosystemPolicy
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
	// Status is how the upstream's recent requests went.
	Status UpstreamStatus

	// host is the upstream's host, used to look up its status. It is not
	// rendered.
	host string
}

func (s *Server) handleUpstreams(w http.ResponseWriter, r *http.Request) {
	data := UpstreamsData{
		Layout:     s.layoutFor(r),
		Ecosystems: upstreamEcosystems(s.cfg.BaseURL, &s.cfg.Upstream),
	}
	upstreamStatuses(data.Ecosystems, s.activity, s.breakers.snapshot(), s.rateLimits.Holds(), time.Now())
	data.Summary = statusSummary(data.Ecosystems)

	// The 60-second snapshot the analytics page reads, so a page load costs
	// no query of its own. Without it the page still shows the statuses.
	stats, err := s.ecoStats.Get(s.db)
	if err != nil {
		s.logger.Error("failed to get ecosystem stats", "error", err)
	}
	snap, err := metrics.Gather()
	if err != nil {
		s.logger.Error("failed to gather runtime metrics", "error", err)
	}
	ecosystemCaches(data.Ecosystems, stats, snap)
	ecosystemPolicies(data.Ecosystems, s.cooldown, &s.cfg.Mirror)
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
		name, key string
		settings  []upstreamSetting
	}{
		{"npm", "npm", []upstreamSetting{single("upstream.npm", "/npm/", u.NPM), named("upstream.npm_routes", "/npm/", u.NPMRoutes)}},
		{"Cargo", "cargo", []upstreamSetting{single("upstream.cargo", "/cargo/", u.Cargo), single("upstream.cargo_download", "/cargo/", u.CargoDownload)}},
		{"RubyGems", "gem", []upstreamSetting{single("upstream.gem", "/gem/", u.Gem)}},
		{"Go", "golang", []upstreamSetting{
			single("upstream.go", "/go/", u.Go),
			named("upstream.go_sumdbs", "/go/sumdb/{name}/", handler.GoSumDBs(u.GoSumDBs)),
			single("upstream.go_vulndb", "/go/vulndb/", u.GoVulnDB),
		}},
		{"Hex", "hex", []upstreamSetting{single("upstream.hex", "/hex/", u.Hex), single("upstream.hex_api", "/hex/", u.HexAPI)}},
		{"pub.dev", "pub", []upstreamSetting{single("upstream.pub", "/pub/", u.Pub)}},
		{"PyPI", "pypi", []upstreamSetting{single("upstream.pypi", "/pypi/", u.PyPI), single("upstream.pypi_download", "/pypi/", u.PyPIDownload)}},
		{"Maven", "maven", []upstreamSetting{
			single("upstream.maven", "/maven/", u.Maven),
			named("upstream.maven_repositories", "/maven/{name}/", u.MavenRepositories),
			single("upstream.gradle_plugin_portal", "/maven/", u.GradlePluginPortal),
		}},
		{"NuGet", "nuget", []upstreamSetting{single("upstream.nuget", "/nuget/", u.NuGet), single("upstream.nuget_search", "/nuget/", u.NuGetSearch)}},
		{"Composer", "composer", []upstreamSetting{
			single("upstream.composer", "/composer/", u.Composer),
			single("upstream.composer_repository", "/composer/", u.ComposerRepository),
			named("upstream.composer_routes", "/composer/", u.ComposerRoutes),
		}},
		{"Conan", "conan", []upstreamSetting{single("upstream.conan", "/conan/", u.Conan)}},
		{"Chef", "chef", []upstreamSetting{single("upstream.chef", "/chef/", u.Chef)}},
		{"Conda", "conda", []upstreamSetting{single("upstream.conda", "/conda/", u.Conda)}},
		{"CRAN", "cran", []upstreamSetting{single("upstream.cran", "/cran/", u.CRAN)}},
		{"Julia", "julia", []upstreamSetting{single("upstream.julia", "/julia/", u.Julia)}},
		{"Swift", "swift", []upstreamSetting{single("upstream.swift", "/swift/", u.Swift)}},
		{"Homebrew", "homebrew", []upstreamSetting{
			single("upstream.homebrew_api", "/homebrew/", u.HomebrewAPI),
			// Bottles are served by the container handler (see
			// handler.RegisterHomebrewArtifacts).
			single("upstream.homebrew_artifact", "/v2/homebrew/core/", u.HomebrewArtifact),
		}},
		{"OCI", "oci", []upstreamSetting{single("upstream.oci_default", "/v2/", u.OCIDefault), named("upstream.oci", "/v2/upstream/{name}/", u.OCI)}},
		{"Helm", "helm", []upstreamSetting{named("upstream.helm", "/helm/{name}/", u.Helm)}},
		{"Alpine", "alpine", []upstreamSetting{named("upstream.apk", "/apk/{name}/", handler.APKRepositories(u.APK))}},
		{"Arch Linux", "arch", []upstreamSetting{named("upstream.arch", "/arch/{name}/", handler.ArchRepositories(u.Arch))}},
		{"FreeBSD", "freebsd", []upstreamSetting{named("upstream.freebsd", "/freebsd/{name}/", handler.FreeBSDRepositories(u.FreeBSD))}},
		{"Terraform", "terraform", []upstreamSetting{named("upstream.terraform", "/terraform/providers/{name}/", handler.TerraformRegistries(u.Terraform))}},
		{"Debian", "deb", []upstreamSetting{single("upstream.debian", "/debian/", u.Debian), named("upstream.debian_repositories", "/debian/{name}/", u.DebianRepositories)}},
		{"RPM", "rpm", []upstreamSetting{single("upstream.rpm", "/rpm/", u.RPM)}},
		{"Generic", "generic", []upstreamSetting{named("upstream.generic", "/generic/{name}/", u.Generic)}},
		{"Git", "git", []upstreamSetting{named("upstream.git", "/git/{name}/", git)}},
	}
	base := strings.TrimSuffix(baseURL, "/")
	ecosystems := make([]UpstreamEcosystem, 0, len(groups))
	for _, group := range groups {
		var rows []UpstreamRow
		for _, setting := range group.settings {
			rows = append(rows, setting.rows(base)...)
		}
		if len(rows) > 0 {
			ecosystems = append(ecosystems, UpstreamEcosystem{Name: group.name, Key: group.key, Upstreams: rows})
		}
	}
	return ecosystems
}

// rows lists the setting's upstreams, map entries sorted by name, leaving
// out empty ones.
func (s upstreamSetting) rows(base string) []UpstreamRow {
	row := func(name string) UpstreamRow {
		rawURL := s.url
		if s.named != nil {
			rawURL = s.named[name]
		}
		return UpstreamRow{
			host:    urlHost(rawURL),
			Setting: s.key,
			Name:    name,
			// Names are configured unescaped; clients send reserved
			// characters in them path-escaped.
			Path: base + strings.ReplaceAll(s.path, "{name}", url.PathEscape(name)),
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

// urlHost returns the lower-cased host of rawURL, or "" when it has none.
func urlHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

// upstreamHosts returns the hosts of every upstream the upstreams page
// lists, for UpstreamActivity to track.
func upstreamHosts(u *config.UpstreamConfig) []string {
	var hosts []string
	for _, eco := range upstreamEcosystems("", u) {
		for _, row := range eco.Upstreams {
			if row.host != "" {
				hosts = append(hosts, row.host)
			}
		}
	}
	return hosts
}
