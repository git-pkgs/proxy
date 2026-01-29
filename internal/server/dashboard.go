package server

import (
	"html/template"
)

// DashboardData contains data for rendering the dashboard.
type DashboardData struct {
	Stats           DashboardStats
	EnrichmentStats EnrichmentStatsView
	RecentPackages  []PackageInfo
	PopularPackages []PackageInfo
	Registries      []RegistryConfig
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

var dashboardTemplate = template.Must(template.New("dashboard").Parse(`<!DOCTYPE html>
<html lang="en" class="h-full">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>git-pkgs proxy</title>
    <script src="/static/tailwind.js"></script>
    <script>
      tailwind.config = {
        darkMode: 'class',
        theme: {
          extend: {
            fontFamily: {
              sans: ['Inter', 'system-ui', '-apple-system', 'sans-serif'],
              mono: ['JetBrains Mono', 'SF Mono', 'Monaco', 'monospace'],
            }
          }
        }
      }
    </script>
    <script>
      // Check for dark mode preference
      if (localStorage.theme === 'dark' || (!('theme' in localStorage) && window.matchMedia('(prefers-color-scheme: dark)').matches)) {
        document.documentElement.classList.add('dark')
      } else {
        document.documentElement.classList.remove('dark')
      }
    </script>
</head>
<body class="h-full bg-gray-50 dark:bg-gray-950 text-gray-900 dark:text-gray-100 transition-colors">
    <!-- Header -->
    <header class="sticky top-0 z-50 border-b border-gray-200 dark:border-gray-800 bg-white/80 dark:bg-gray-900/80 backdrop-blur-sm">
        <div class="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8">
            <div class="flex justify-between items-center h-16">
                <div class="flex items-center gap-2">
                    <svg class="w-8 h-8 text-primary-600 dark:text-primary-400" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                        <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M20 7l-8-4-8 4m16 0l-8 4m8-4v10l-8 4m0-10L4 7m8 4v10M4 7v10l8 4"/>
                    </svg>
                    <span class="text-xl font-semibold">git-pkgs proxy</span>
                </div>
                <nav class="flex items-center gap-6">
                    <a href="/health" class="text-sm text-gray-600 dark:text-gray-400 hover:text-gray-900 dark:hover:text-gray-100">Health</a>
                    <a href="/stats" class="text-sm text-gray-600 dark:text-gray-400 hover:text-gray-900 dark:hover:text-gray-100">API</a>
                    <button id="theme-toggle" class="p-2 rounded-lg hover:bg-gray-100 dark:hover:bg-gray-800 transition-colors">
                        <svg class="w-5 h-5 hidden dark:block" fill="currentColor" viewBox="0 0 20 20">
                            <path d="M10 2a1 1 0 011 1v1a1 1 0 11-2 0V3a1 1 0 011-1zm4 8a4 4 0 11-8 0 4 4 0 018 0zm-.464 4.95l.707.707a1 1 0 001.414-1.414l-.707-.707a1 1 0 00-1.414 1.414zm2.12-10.607a1 1 0 010 1.414l-.706.707a1 1 0 11-1.414-1.414l.707-.707a1 1 0 011.414 0zM17 11a1 1 0 100-2h-1a1 1 0 100 2h1zm-7 4a1 1 0 011 1v1a1 1 0 11-2 0v-1a1 1 0 011-1zM5.05 6.464A1 1 0 106.465 5.05l-.708-.707a1 1 0 00-1.414 1.414l.707.707zm1.414 8.486l-.707.707a1 1 0 01-1.414-1.414l.707-.707a1 1 0 011.414 1.414zM4 11a1 1 0 100-2H3a1 1 0 000 2h1z"/>
                        </svg>
                        <svg class="w-5 h-5 block dark:hidden" fill="currentColor" viewBox="0 0 20 20">
                            <path d="M17.293 13.293A8 8 0 016.707 2.707a8.001 8.001 0 1010.586 10.586z"/>
                        </svg>
                    </button>
                </nav>
            </div>
        </div>
    </header>

    <main class="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8 py-8">
        <!-- Stats Grid -->
        <div class="grid grid-cols-2 md:grid-cols-4 gap-4 mb-8">
            <div class="bg-white dark:bg-gray-900 rounded-xl p-6 shadow-sm border border-gray-200 dark:border-gray-800">
                <div class="text-sm text-gray-500 dark:text-gray-400">Cached Artifacts</div>
                <div class="text-3xl font-bold mt-1">{{.Stats.CachedArtifacts}}</div>
            </div>
            <div class="bg-white dark:bg-gray-900 rounded-xl p-6 shadow-sm border border-gray-200 dark:border-gray-800">
                <div class="text-sm text-gray-500 dark:text-gray-400">Total Size</div>
                <div class="text-3xl font-bold mt-1">{{.Stats.TotalSize}}</div>
            </div>
            <div class="bg-white dark:bg-gray-900 rounded-xl p-6 shadow-sm border border-gray-200 dark:border-gray-800">
                <div class="text-sm text-gray-500 dark:text-gray-400">Packages</div>
                <div class="text-3xl font-bold mt-1">{{.Stats.TotalPackages}}</div>
            </div>
            <div class="bg-white dark:bg-gray-900 rounded-xl p-6 shadow-sm border border-gray-200 dark:border-gray-800">
                <div class="text-sm text-gray-500 dark:text-gray-400">Versions</div>
                <div class="text-3xl font-bold mt-1">{{.Stats.TotalVersions}}</div>
            </div>
        </div>

        {{if .EnrichmentStats.HasVulns}}
        <!-- Security Overview -->
        <div class="bg-white dark:bg-gray-900 rounded-xl shadow-sm border border-gray-200 dark:border-gray-800 mb-8">
            <div class="px-6 py-4 border-b border-gray-200 dark:border-gray-800">
                <h2 class="text-lg font-semibold">Security Overview</h2>
            </div>
            <div class="p-6">
                <div class="grid grid-cols-2 md:grid-cols-4 gap-4">
                    <div class="text-center p-4 rounded-lg bg-red-50 dark:bg-red-950 border border-red-200 dark:border-red-900">
                        <div class="text-2xl font-bold text-red-600 dark:text-red-400">{{.EnrichmentStats.CriticalVulns}}</div>
                        <div class="text-xs text-red-600 dark:text-red-400 uppercase font-medium">Critical</div>
                    </div>
                    <div class="text-center p-4 rounded-lg bg-orange-50 dark:bg-orange-950 border border-orange-200 dark:border-orange-900">
                        <div class="text-2xl font-bold text-orange-600 dark:text-orange-400">{{.EnrichmentStats.HighVulns}}</div>
                        <div class="text-xs text-orange-600 dark:text-orange-400 uppercase font-medium">High</div>
                    </div>
                    <div class="text-center p-4 rounded-lg bg-yellow-50 dark:bg-yellow-950 border border-yellow-200 dark:border-yellow-900">
                        <div class="text-2xl font-bold text-yellow-600 dark:text-yellow-400">{{.EnrichmentStats.MediumVulns}}</div>
                        <div class="text-xs text-yellow-600 dark:text-yellow-400 uppercase font-medium">Medium</div>
                    </div>
                    <div class="text-center p-4 rounded-lg bg-green-50 dark:bg-green-950 border border-green-200 dark:border-green-900">
                        <div class="text-2xl font-bold text-green-600 dark:text-green-400">{{.EnrichmentStats.LowVulns}}</div>
                        <div class="text-xs text-green-600 dark:text-green-400 uppercase font-medium">Low</div>
                    </div>
                </div>
                <p class="mt-4 text-sm text-gray-500 dark:text-gray-400">
                    {{.EnrichmentStats.TotalVulnerabilities}} vulnerabilities tracked across {{.EnrichmentStats.VulnSyncedPackages}} packages
                </p>
            </div>
        </div>
        {{end}}

        <!-- Two Column Layout -->
        <div class="grid md:grid-cols-2 gap-8 mb-8">
            <!-- Popular Packages -->
            <div class="bg-white dark:bg-gray-900 rounded-xl shadow-sm border border-gray-200 dark:border-gray-800">
                <div class="px-6 py-4 border-b border-gray-200 dark:border-gray-800">
                    <h2 class="text-lg font-semibold">Popular Packages</h2>
                </div>
                <div class="divide-y divide-gray-200 dark:divide-gray-800">
                    {{if .PopularPackages}}
                    {{range .PopularPackages}}
                    <div class="px-6 py-4 flex items-center justify-between">
                        <div class="min-w-0 flex-1">
                            <div class="flex items-center gap-2">
                                <span class="inline-flex items-center px-2 py-0.5 rounded text-xs font-medium ecosystem-{{.Ecosystem}}">{{.Ecosystem}}</span>
                                <span class="font-medium truncate">{{.Name}}</span>
                            </div>
                            <div class="flex items-center gap-2 mt-1">
                                {{if .License}}<span class="inline-flex items-center px-2 py-0.5 rounded text-xs font-medium bg-{{if eq .LicenseCategory "permissive"}}green{{else if eq .LicenseCategory "copyleft"}}pink{{else}}gray{{end}}-100 text-{{if eq .LicenseCategory "permissive"}}green{{else if eq .LicenseCategory "copyleft"}}pink{{else}}gray{{end}}-700 dark:bg-{{if eq .LicenseCategory "permissive"}}green{{else if eq .LicenseCategory "copyleft"}}pink{{else}}gray{{end}}-900 dark:text-{{if eq .LicenseCategory "permissive"}}green{{else if eq .LicenseCategory "copyleft"}}pink{{else}}gray{{end}}-300">{{.License}}</span>{{end}}
                                {{if .VulnCount}}<span class="inline-flex items-center px-2 py-0.5 rounded text-xs font-medium bg-red-100 text-red-700 dark:bg-red-900 dark:text-red-300">{{.VulnCount}} vulns</span>{{end}}
                            </div>
                        </div>
                        <div class="flex items-center gap-4 text-sm text-gray-500 dark:text-gray-400">
                            <span>{{.Hits}} hits</span>
                            <span>{{.Size}}</span>
                        </div>
                    </div>
                    {{end}}
                    {{else}}
                    <div class="px-6 py-12 text-center text-gray-500 dark:text-gray-400">No packages cached yet</div>
                    {{end}}
                </div>
            </div>

            <!-- Recently Cached -->
            <div class="bg-white dark:bg-gray-900 rounded-xl shadow-sm border border-gray-200 dark:border-gray-800">
                <div class="px-6 py-4 border-b border-gray-200 dark:border-gray-800">
                    <h2 class="text-lg font-semibold">Recently Cached</h2>
                </div>
                <div class="divide-y divide-gray-200 dark:divide-gray-800">
                    {{if .RecentPackages}}
                    {{range .RecentPackages}}
                    <div class="px-6 py-4 flex items-center justify-between">
                        <div class="min-w-0 flex-1">
                            <div class="flex items-center gap-2">
                                <span class="inline-flex items-center px-2 py-0.5 rounded text-xs font-medium ecosystem-{{.Ecosystem}}">{{.Ecosystem}}</span>
                                <span class="font-medium truncate">{{.Name}}</span>
                                <span class="text-gray-500 dark:text-gray-400">@{{.Version}}</span>
                            </div>
                            <div class="flex items-center gap-2 mt-1">
                                {{if .License}}<span class="inline-flex items-center px-2 py-0.5 rounded text-xs font-medium bg-{{if eq .LicenseCategory "permissive"}}green{{else if eq .LicenseCategory "copyleft"}}pink{{else}}gray{{end}}-100 text-{{if eq .LicenseCategory "permissive"}}green{{else if eq .LicenseCategory "copyleft"}}pink{{else}}gray{{end}}-700 dark:bg-{{if eq .LicenseCategory "permissive"}}green{{else if eq .LicenseCategory "copyleft"}}pink{{else}}gray{{end}}-900 dark:text-{{if eq .LicenseCategory "permissive"}}green{{else if eq .LicenseCategory "copyleft"}}pink{{else}}gray{{end}}-300">{{.License}}</span>{{end}}
                                {{if .IsOutdated}}<span class="inline-flex items-center px-2 py-0.5 rounded text-xs font-medium bg-amber-100 text-amber-700 dark:bg-amber-900 dark:text-amber-300">outdated</span>{{end}}
                                {{if .VulnCount}}<span class="inline-flex items-center px-2 py-0.5 rounded text-xs font-medium bg-red-100 text-red-700 dark:bg-red-900 dark:text-red-300">{{.VulnCount}} vulns</span>{{end}}
                            </div>
                        </div>
                        <div class="flex items-center gap-4 text-sm text-gray-500 dark:text-gray-400">
                            <span>{{.CachedAt}}</span>
                            <span>{{.Size}}</span>
                        </div>
                    </div>
                    {{end}}
                    {{else}}
                    <div class="px-6 py-12 text-center text-gray-500 dark:text-gray-400">No packages cached yet</div>
                    {{end}}
                </div>
            </div>
        </div>

        <!-- Registry Configuration -->
        <div class="bg-white dark:bg-gray-900 rounded-xl shadow-sm border border-gray-200 dark:border-gray-800">
            <div class="px-6 py-4 border-b border-gray-200 dark:border-gray-800">
                <h2 class="text-lg font-semibold">Configure Your Package Manager</h2>
            </div>
            <div class="divide-y divide-gray-200 dark:divide-gray-800">
                {{range .Registries}}
                <details class="group">
                    <summary class="px-6 py-4 cursor-pointer flex items-center justify-between hover:bg-gray-50 dark:hover:bg-gray-800/50 transition-colors">
                        <div class="flex items-center gap-3">
                            <span class="font-medium">{{.Name}}</span>
                            <span class="text-sm text-gray-500 dark:text-gray-400">{{.Language}}</span>
                        </div>
                        <div class="flex items-center gap-3">
                            <code class="text-sm text-gray-600 dark:text-gray-400 font-mono">{{.Endpoint}}</code>
                            <svg class="w-5 h-5 text-gray-400 transition-transform group-open:rotate-180" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 9l-7 7-7-7"/>
                            </svg>
                        </div>
                    </summary>
                    <div class="px-6 pb-4 prose prose-sm dark:prose-invert max-w-none">
                        {{.Instructions}}
                    </div>
                </details>
                {{end}}
            </div>
        </div>
    </main>

    <style type="text/tailwindcss">
        @layer components {
            .ecosystem-npm { @apply bg-red-100 text-red-700 dark:bg-red-900/50 dark:text-red-300; }
            .ecosystem-cargo { @apply bg-orange-100 text-orange-700 dark:bg-orange-900/50 dark:text-orange-300; }
            .ecosystem-gem { @apply bg-pink-100 text-pink-700 dark:bg-pink-900/50 dark:text-pink-300; }
            .ecosystem-go { @apply bg-cyan-100 text-cyan-700 dark:bg-cyan-900/50 dark:text-cyan-300; }
            .ecosystem-hex { @apply bg-purple-100 text-purple-700 dark:bg-purple-900/50 dark:text-purple-300; }
            .ecosystem-pub { @apply bg-blue-100 text-blue-700 dark:bg-blue-900/50 dark:text-blue-300; }
            .ecosystem-pypi { @apply bg-yellow-100 text-yellow-700 dark:bg-yellow-900/50 dark:text-yellow-300; }
            .ecosystem-maven { @apply bg-red-100 text-red-700 dark:bg-red-900/50 dark:text-red-300; }
            .ecosystem-nuget { @apply bg-indigo-100 text-indigo-700 dark:bg-indigo-900/50 dark:text-indigo-300; }
            .ecosystem-composer { @apply bg-violet-100 text-violet-700 dark:bg-violet-900/50 dark:text-violet-300; }
            .ecosystem-conan { @apply bg-teal-100 text-teal-700 dark:bg-teal-900/50 dark:text-teal-300; }
            .ecosystem-conda { @apply bg-green-100 text-green-700 dark:bg-green-900/50 dark:text-green-300; }
            .ecosystem-cran { @apply bg-slate-100 text-slate-700 dark:bg-slate-800 dark:text-slate-300; }
            .ecosystem-oci { @apply bg-sky-100 text-sky-700 dark:bg-sky-900/50 dark:text-sky-300; }
            .ecosystem-deb { @apply bg-red-100 text-red-800 dark:bg-red-900/50 dark:text-red-300; }
            .ecosystem-rpm { @apply bg-amber-100 text-amber-800 dark:bg-amber-900/50 dark:text-amber-300; }
        }
        pre {
            @apply bg-gray-900 dark:bg-gray-950 text-gray-100 p-4 rounded-lg overflow-x-auto text-sm;
        }
        code {
            @apply font-mono;
        }
        .config-note {
            @apply text-sm text-gray-600 dark:text-gray-400 mb-2;
        }
    </style>

    <script>
        document.getElementById('theme-toggle').addEventListener('click', function() {
            if (document.documentElement.classList.contains('dark')) {
                document.documentElement.classList.remove('dark');
                localStorage.theme = 'light';
            } else {
                document.documentElement.classList.add('dark');
                localStorage.theme = 'dark';
            }
        });
    </script>
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
