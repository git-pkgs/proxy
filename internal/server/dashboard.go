package server

import (
	"html/template"
)

// DashboardData contains data for rendering the dashboard.
type DashboardData struct {
	Stats          DashboardStats
	RecentPackages []PackageInfo
	PopularPackages []PackageInfo
	Registries     []RegistryConfig
}

// DashboardStats contains cache statistics for the dashboard.
type DashboardStats struct {
	CachedArtifacts int64
	TotalSize       string
	TotalPackages   int64
	TotalVersions   int64
}

// PackageInfo contains information about a cached package.
type PackageInfo struct {
	Ecosystem string
	Name      string
	Version   string
	Size      string
	Hits      int64
	CachedAt  string
}

// RegistryConfig contains configuration instructions for a package registry.
type RegistryConfig struct {
	ID           string
	Name         string
	Language     string
	Endpoint     string
	Instructions template.HTML
}

var dashboardTemplate = template.Must(template.New("dashboard").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>git-pkgs proxy</title>
    <style>
        * { box-sizing: border-box; margin: 0; padding: 0; }
        body {
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
            line-height: 1.6;
            color: #333;
            background: #f5f5f5;
        }
        .container { max-width: 1200px; margin: 0 auto; padding: 20px; }
        header { background: #2d3748; color: white; padding: 20px 0; margin-bottom: 30px; }
        header .container { display: flex; justify-content: space-between; align-items: center; }
        h1 { font-size: 1.5rem; font-weight: 600; }
        .stats-grid {
            display: grid;
            grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
            gap: 20px;
            margin-bottom: 30px;
        }
        .stat-card {
            background: white;
            padding: 20px;
            border-radius: 8px;
            box-shadow: 0 1px 3px rgba(0,0,0,0.1);
        }
        .stat-card .label { font-size: 0.875rem; color: #666; }
        .stat-card .value { font-size: 1.75rem; font-weight: 600; color: #2d3748; }
        .section { background: white; border-radius: 8px; box-shadow: 0 1px 3px rgba(0,0,0,0.1); margin-bottom: 30px; }
        .section-header { padding: 15px 20px; border-bottom: 1px solid #e2e8f0; font-weight: 600; }
        .section-body { padding: 20px; }
        table { width: 100%; border-collapse: collapse; }
        th, td { text-align: left; padding: 10px 12px; border-bottom: 1px solid #e2e8f0; }
        th { font-weight: 600; color: #666; font-size: 0.875rem; }
        tr:last-child td { border-bottom: none; }
        .ecosystem {
            display: inline-block;
            padding: 2px 8px;
            border-radius: 4px;
            font-size: 0.75rem;
            font-weight: 600;
            text-transform: uppercase;
        }
        .ecosystem-npm { background: #fde8e8; color: #c53030; }
        .ecosystem-cargo { background: #feebc8; color: #c05621; }
        .ecosystem-gem { background: #fed7e2; color: #b83280; }
        .ecosystem-go { background: #c6f6d5; color: #276749; }
        .ecosystem-hex { background: #e9d8fd; color: #6b46c1; }
        .ecosystem-pub { background: #bee3f8; color: #2b6cb0; }
        .ecosystem-pypi { background: #fefcbf; color: #975a16; }
        .ecosystem-maven { background: #fed7d7; color: #c53030; }
        .ecosystem-nuget { background: #c3dafe; color: #3c366b; }
        .ecosystem-composer { background: #faf5ff; color: #6b46c1; }
        .ecosystem-conan { background: #e6fffa; color: #234e52; }
        .ecosystem-conda { background: #c6f6d5; color: #22543d; }
        .ecosystem-cran { background: #e2e8f0; color: #2d3748; }
        .registry-list { display: grid; gap: 15px; }
        .registry-item { border: 1px solid #e2e8f0; border-radius: 6px; overflow: hidden; }
        .registry-header {
            display: flex;
            justify-content: space-between;
            align-items: center;
            padding: 12px 15px;
            background: #f7fafc;
            cursor: pointer;
        }
        .registry-header:hover { background: #edf2f7; }
        .registry-title { font-weight: 600; }
        .registry-lang { font-size: 0.875rem; color: #666; }
        .registry-endpoint { font-family: monospace; font-size: 0.875rem; color: #4a5568; }
        .registry-content { padding: 15px; border-top: 1px solid #e2e8f0; display: none; }
        .registry-item.open .registry-content { display: block; }
        .registry-item.open .toggle::after { content: "−"; }
        .toggle::after { content: "+"; font-weight: bold; color: #666; }
        pre {
            background: #1a202c;
            color: #e2e8f0;
            padding: 15px;
            border-radius: 6px;
            overflow-x: auto;
            font-size: 0.875rem;
            margin: 10px 0;
        }
        code { font-family: "SF Mono", Monaco, "Cascadia Code", monospace; }
        .config-note { font-size: 0.875rem; color: #666; margin-bottom: 10px; }
        .empty { color: #999; font-style: italic; padding: 20px; text-align: center; }
        .two-col { display: grid; grid-template-columns: 1fr 1fr; gap: 30px; }
        @media (max-width: 768px) { .two-col { grid-template-columns: 1fr; } }
        .nav-links a { color: white; margin-left: 20px; text-decoration: none; opacity: 0.8; }
        .nav-links a:hover { opacity: 1; }
    </style>
</head>
<body>
    <header>
        <div class="container">
            <h1>git-pkgs proxy</h1>
            <nav class="nav-links">
                <a href="/health">Health</a>
                <a href="/stats">Stats API</a>
            </nav>
        </div>
    </header>

    <div class="container">
        <div class="stats-grid">
            <div class="stat-card">
                <div class="label">Cached Artifacts</div>
                <div class="value">{{.Stats.CachedArtifacts}}</div>
            </div>
            <div class="stat-card">
                <div class="label">Total Size</div>
                <div class="value">{{.Stats.TotalSize}}</div>
            </div>
            <div class="stat-card">
                <div class="label">Packages</div>
                <div class="value">{{.Stats.TotalPackages}}</div>
            </div>
            <div class="stat-card">
                <div class="label">Versions</div>
                <div class="value">{{.Stats.TotalVersions}}</div>
            </div>
        </div>

        <div class="two-col">
            <div class="section">
                <div class="section-header">Popular Packages</div>
                <div class="section-body">
                    {{if .PopularPackages}}
                    <table>
                        <thead>
                            <tr><th>Package</th><th>Hits</th><th>Size</th></tr>
                        </thead>
                        <tbody>
                            {{range .PopularPackages}}
                            <tr>
                                <td>
                                    <span class="ecosystem ecosystem-{{.Ecosystem}}">{{.Ecosystem}}</span>
                                    {{.Name}}
                                </td>
                                <td>{{.Hits}}</td>
                                <td>{{.Size}}</td>
                            </tr>
                            {{end}}
                        </tbody>
                    </table>
                    {{else}}
                    <div class="empty">No packages cached yet</div>
                    {{end}}
                </div>
            </div>

            <div class="section">
                <div class="section-header">Recently Cached</div>
                <div class="section-body">
                    {{if .RecentPackages}}
                    <table>
                        <thead>
                            <tr><th>Package</th><th>Cached</th><th>Size</th></tr>
                        </thead>
                        <tbody>
                            {{range .RecentPackages}}
                            <tr>
                                <td>
                                    <span class="ecosystem ecosystem-{{.Ecosystem}}">{{.Ecosystem}}</span>
                                    {{.Name}}@{{.Version}}
                                </td>
                                <td>{{.CachedAt}}</td>
                                <td>{{.Size}}</td>
                            </tr>
                            {{end}}
                        </tbody>
                    </table>
                    {{else}}
                    <div class="empty">No packages cached yet</div>
                    {{end}}
                </div>
            </div>
        </div>

        <div class="section">
            <div class="section-header">Configure Your Package Manager</div>
            <div class="section-body">
                <div class="registry-list">
                    {{range .Registries}}
                    <div class="registry-item">
                        <div class="registry-header" onclick="this.parentElement.classList.toggle('open')">
                            <div>
                                <span class="registry-title">{{.Name}}</span>
                                <span class="registry-lang">{{.Language}}</span>
                            </div>
                            <div>
                                <span class="registry-endpoint">{{.Endpoint}}</span>
                                <span class="toggle"></span>
                            </div>
                        </div>
                        <div class="registry-content">
                            {{.Instructions}}
                        </div>
                    </div>
                    {{end}}
                </div>
            </div>
        </div>
    </div>
</body>
</html>
`))

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
	}
}
