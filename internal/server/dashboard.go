package server

import (
	"html/template"

	"github.com/git-pkgs/proxy/internal/database"
)

// DashboardData contains data for rendering the dashboard.
type DashboardData struct {
	Stats           DashboardStats
	EnrichmentStats EnrichmentStatsView
	RecentPackages  []PackageInfo
	PopularPackages []PackageInfo
}

// DashboardStats contains cache statistics for the dashboard.
type DashboardStats struct {
	CachedArtifacts int64
	TotalSize       string
	TotalPackages   int64
	TotalVersions   int64
}

// EnrichmentStatsView contains enrichment statistics for display.
type EnrichmentStatsView struct {
	EnrichedPackages     int64
	VulnSyncedPackages   int64
	TotalVulnerabilities int64
	CriticalVulns        int64
	HighVulns            int64
	MediumVulns          int64
	LowVulns             int64
	HasVulns             bool
}

// PackageInfo contains information about a cached package.
type PackageInfo struct {
	Ecosystem       string
	Name            string
	Version         string
	Size            string
	Hits            int64
	CachedAt        string
	License         string
	LicenseCategory string
	VulnCount       int64
	LatestVersion   string
	IsOutdated      bool
}

// RegistryConfig contains configuration instructions for a package registry.
type RegistryConfig struct {
	ID           string
	Name         string
	Language     string
	Endpoint     string
	Instructions template.HTML
}

// PackageShowData contains data for rendering the package show page.
type PackageShowData struct {
	Package         *database.Package
	Versions        []database.Version
	Vulnerabilities []database.Vulnerability
	LicenseCategory string
}

// VersionShowData contains data for rendering the version show page.
type VersionShowData struct {
	Package           *database.Package
	Version           *database.Version
	Artifacts         []database.Artifact
	Vulnerabilities   []database.Vulnerability
	IsOutdated        bool
	LicenseCategory   string
	HasCachedArtifact bool
}

// SearchPageData contains data for rendering the search results page.
type SearchPageData struct {
	Query      string
	Ecosystem  string
	Results    []SearchResultItem
	Count      int
	Page       int
	PerPage    int
	TotalPages int
}

// SearchResultItem represents a single search result for display.
type SearchResultItem struct {
	Ecosystem       string
	Name            string
	LatestVersion   string
	License         string
	LicenseCategory string
	Hits            int64
	Size            int64
	SizeFormatted   string
	CachedAt        string
	VulnCount       int64
}

// PackagesListPageData contains data for rendering the packages list page.
type PackagesListPageData struct {
	Ecosystem  string
	SortBy     string
	Results    []SearchResultItem
	Count      int
	Page       int
	PerPage    int
	TotalPages int
}

func getRegistryConfigs(baseURL string) []RegistryConfig {
	return []RegistryConfig{
		{
			ID:       "npm",
			Name:     "npm",
			Language: "JavaScript",
			Endpoint: "/npm/",
			Instructions: template.HTML(`<p class="config-note">Configure npm to use the proxy:</p>
<pre><code># In ~/.npmrc or project .npmrc
registry=` + baseURL + `/npm/

# Or via environment variable
npm_config_registry=` + baseURL + `/npm/ npm install</code></pre>`),
		},
		{
			ID:       "cargo",
			Name:     "Cargo",
			Language: "Rust",
			Endpoint: "/cargo/",
			Instructions: template.HTML(`<p class="config-note">Configure Cargo to use the proxy (sparse index protocol):</p>
<pre><code># In ~/.cargo/config.toml or project .cargo/config.toml
[source.crates-io]
replace-with = "proxy"

[source.proxy]
registry = "sparse+` + baseURL + `/cargo/"</code></pre>`),
		},
		{
			ID:       "gem",
			Name:     "RubyGems",
			Language: "Ruby",
			Endpoint: "/gem/",
			Instructions: template.HTML(`<p class="config-note">Configure Bundler/RubyGems to use the proxy:</p>
<pre><code># In Gemfile
source "` + baseURL + `/gem"

# Or configure globally
gem sources --add ` + baseURL + `/gem/
bundle config mirror.https://rubygems.org ` + baseURL + `/gem</code></pre>`),
		},
		{
			ID:       "go",
			Name:     "Go Modules",
			Language: "Go",
			Endpoint: "/go/",
			Instructions: template.HTML(`<p class="config-note">Set the GOPROXY environment variable:</p>
<pre><code>export GOPROXY=` + baseURL + `/go,direct

# Add to your shell profile for persistence</code></pre>`),
		},
		{
			ID:       "hex",
			Name:     "Hex",
			Language: "Elixir",
			Endpoint: "/hex/",
			Instructions: template.HTML(`<p class="config-note">Configure Hex to use the proxy:</p>
<pre><code># In ~/.hex/hex.config
{default_url, &lt;&lt;"` + baseURL + `/hex"&gt;&gt;}.

# Or via environment variable
export HEX_MIRROR=` + baseURL + `/hex</code></pre>`),
		},
		{
			ID:       "pub",
			Name:     "pub.dev",
			Language: "Dart/Flutter",
			Endpoint: "/pub/",
			Instructions: template.HTML(`<p class="config-note">Set the PUB_HOSTED_URL environment variable:</p>
<pre><code>export PUB_HOSTED_URL=` + baseURL + `/pub</code></pre>`),
		},
		{
			ID:       "pypi",
			Name:     "PyPI",
			Language: "Python",
			Endpoint: "/pypi/",
			Instructions: template.HTML(`<p class="config-note">Configure pip to use the proxy:</p>
<pre><code># Via command line
pip install --index-url ` + baseURL + `/pypi/simple/ package_name

# In ~/.pip/pip.conf
[global]
index-url = ` + baseURL + `/pypi/simple/</code></pre>`),
		},
		{
			ID:       "maven",
			Name:     "Maven",
			Language: "Java",
			Endpoint: "/maven/",
			Instructions: template.HTML(`<p class="config-note">Configure Maven to use the proxy:</p>
<pre><code>&lt;!-- In ~/.m2/settings.xml --&gt;
&lt;settings&gt;
  &lt;mirrors&gt;
    &lt;mirror&gt;
      &lt;id&gt;proxy&lt;/id&gt;
      &lt;mirrorOf&gt;central&lt;/mirrorOf&gt;
      &lt;url&gt;` + baseURL + `/maven/&lt;/url&gt;
    &lt;/mirror&gt;
  &lt;/mirrors&gt;
&lt;/settings&gt;</code></pre>`),
		},
		{
			ID:       "nuget",
			Name:     "NuGet",
			Language: ".NET",
			Endpoint: "/nuget/",
			Instructions: template.HTML(`<p class="config-note">Configure NuGet to use the proxy:</p>
<pre><code>&lt;!-- In nuget.config --&gt;
&lt;configuration&gt;
  &lt;packageSources&gt;
    &lt;clear /&gt;
    &lt;add key="proxy" value="` + baseURL + `/nuget/v3/index.json" /&gt;
  &lt;/packageSources&gt;
&lt;/configuration&gt;

# Or via CLI
dotnet nuget add source ` + baseURL + `/nuget/v3/index.json -n proxy</code></pre>`),
		},
		{
			ID:       "composer",
			Name:     "Composer",
			Language: "PHP",
			Endpoint: "/composer/",
			Instructions: template.HTML(`<p class="config-note">Configure Composer to use the proxy:</p>
<pre><code>// In composer.json
{
    "repositories": [
        {
            "type": "composer",
            "url": "` + baseURL + `/composer"
        }
    ]
}

# Or globally
composer config -g repositories.proxy composer ` + baseURL + `/composer</code></pre>`),
		},
		{
			ID:       "conan",
			Name:     "Conan",
			Language: "C/C++",
			Endpoint: "/conan/",
			Instructions: template.HTML(`<p class="config-note">Configure Conan to use the proxy:</p>
<pre><code>conan remote add proxy ` + baseURL + `/conan
conan remote disable conancenter</code></pre>`),
		},
		{
			ID:       "conda",
			Name:     "Conda",
			Language: "Python/R",
			Endpoint: "/conda/",
			Instructions: template.HTML(`<p class="config-note">Configure Conda to use the proxy:</p>
<pre><code># In ~/.condarc
channels:
  - ` + baseURL + `/conda/main
  - ` + baseURL + `/conda/conda-forge
default_channels:
  - ` + baseURL + `/conda/main

# Or via command
conda config --add channels ` + baseURL + `/conda/main</code></pre>`),
		},
		{
			ID:       "cran",
			Name:     "CRAN",
			Language: "R",
			Endpoint: "/cran/",
			Instructions: template.HTML(`<p class="config-note">Configure R to use the proxy:</p>
<pre><code># In R session
options(repos = c(CRAN = "` + baseURL + `/cran"))

# In ~/.Rprofile for persistence
local({
  r &lt;- getOption("repos")
  r["CRAN"] &lt;- "` + baseURL + `/cran"
  options(repos = r)
})</code></pre>`),
		},
		{
			ID:       "oci",
			Name:     "Container Registry",
			Language: "Docker/OCI",
			Endpoint: "/v2/",
			Instructions: template.HTML(`<p class="config-note">Configure Docker to use the proxy as a mirror:</p>
<pre><code># In /etc/docker/daemon.json
{
  "registry-mirrors": ["` + baseURL + `"]
}

# Then restart Docker
sudo systemctl restart docker

# Or pull directly
docker pull ` + baseURL[8:] + `/library/nginx:latest</code></pre>`),
		},
		{
			ID:       "deb",
			Name:     "Debian/APT",
			Language: "Debian/Ubuntu",
			Endpoint: "/debian/",
			Instructions: template.HTML(`<p class="config-note">Configure APT to use the proxy:</p>
<pre><code># In /etc/apt/sources.list or /etc/apt/sources.list.d/proxy.list
deb ` + baseURL + `/debian stable main contrib

# Replace your existing sources.list entries with the proxy URL
# Then run:
sudo apt update</code></pre>`),
		},
		{
			ID:       "rpm",
			Name:     "RPM/Yum",
			Language: "Fedora/RHEL",
			Endpoint: "/rpm/",
			Instructions: template.HTML(`<p class="config-note">Configure yum/dnf to use the proxy:</p>
<pre><code># In /etc/yum.repos.d/proxy.repo
[proxy-fedora]
name=Fedora via Proxy
baseurl=` + baseURL + `/rpm/releases/$releasever/Everything/$basearch/os/
enabled=1
gpgcheck=0

# Then run:
sudo dnf clean all
sudo dnf update</code></pre>`),
		},
	}
}
