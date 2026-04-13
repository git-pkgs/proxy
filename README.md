# git-pkgs proxy

A caching proxy for package registries. Speeds up package downloads by caching artifacts locally, reducing bandwidth usage and improving reliability.

## Version Cooldown

Most supply chain attacks rely on speed: a malicious version gets published and consumed by automated pipelines within minutes, before anyone notices. The cooldown feature adds a quarantine period to newly published versions. When enabled, the proxy strips versions from metadata responses until they've aged past a configurable threshold.

```yaml
cooldown:
  default: "3d"              # hide versions published less than 3 days ago
  ecosystems:
    npm: "7d"                # npm gets a longer window
    cargo: "0"               # disable for cargo
  packages:
    "pkg:npm/lodash": "0"    # exempt trusted packages
```

A 3-day cooldown means that when `lodash` publishes version `4.18.0`, your builds keep using `4.17.21` until 3 days have passed. If the new release turns out to be compromised, you were never exposed.

Resolution order: package override, then ecosystem override, then global default. This lets you set a conservative default and carve out exceptions for packages where you need faster updates. See [docs/configuration.md](docs/configuration.md) for the full config reference.

## Supported Registries

| Registry | Language/Platform | Cooldown | Completed |
|----------|-------------------|:--------:|:---------:|
| npm | JavaScript | Yes | ✓ |
| Cargo | Rust | Yes | ✓ |
| RubyGems | Ruby | Yes | ✓ |
| Go proxy | Go | | ✓ |
| Hex | Elixir | Yes* | ✓ |
| pub.dev | Dart | Yes | ✓ |
| PyPI | Python | Yes | ✓ |
| Maven | Java | | ✓ |
| NuGet | .NET | Yes | ✓ |
| Composer | PHP | Yes | ✓ |
| Conan | C/C++ | | ✓ |
| Conda | Python/R | Yes | ✓ |
| CRAN | R | | ✓ |
| Container | Docker/OCI | | ✓ |
| Debian | Debian/Ubuntu | | ✓ |
| RPM | RHEL/Fedora | | ✓ |
| Alpine | Alpine Linux | | ✗ |
| Arch | Arch Linux | | ✗ |
| Chef | Chef | | ✗ |
| Generic | Any | | ✗ |
| Helm | Kubernetes | | ✗ |
| Swift | Swift | | ✗ |
| Vagrant | Vagrant | | ✗ |

Cooldown requires publish timestamps in metadata. Registries without a "Yes" in the cooldown column either don't expose timestamps or haven't been wired up yet.

\* Hex cooldown requires disabling registry signature verification (`HEX_NO_VERIFY_REPO_ORIGIN=1`) since the proxy re-encodes the protobuf payload.

## Quick Start

```bash
# Build from source
go build -o proxy ./cmd/proxy

# Run with defaults (listens on :8080)
./proxy

# Run with custom settings
./proxy -listen :3000 -base-url https://proxy.example.com
```

The proxy is now running. Configure your package managers to use it.

## OpenAPI (Swagger)

This repo uses swaggo to generate an OpenAPI spec from annotated handlers.

Generate the spec:

```bash
go install github.com/swaggo/swag/cmd/swag@latest
go generate ./internal/server
```

Generated files are written to `docs/swagger/`.

When the proxy is running, fetch the live spec from:

- `http://localhost:8080/openapi.json`

Or replace `http://localhost:8080` with your configured base URL. This link is also shown on the dashboard.

## Configuring Package Managers

### npm

Create or edit `~/.npmrc`:

```
registry=http://localhost:8080/npm/
```

Or set per-project in `.npmrc`:

```
registry=http://localhost:8080/npm/
```

Or use environment variable:

```bash
npm_config_registry=http://localhost:8080/npm/ npm install
```

### Cargo

Create or edit `~/.cargo/config.toml`:

```toml
[source.crates-io]
replace-with = "proxy"

[source.proxy]
registry = "sparse+http://localhost:8080/cargo/"
```

Or set per-project in `.cargo/config.toml` in your project root.

### RubyGems / Bundler

Set the gem source in your `Gemfile`:

```ruby
source "http://localhost:8080/gem"
```

Or configure globally:

```bash
gem sources --add http://localhost:8080/gem/
bundle config mirror.https://rubygems.org http://localhost:8080/gem
```

### Go modules

Set the GOPROXY environment variable:

```bash
export GOPROXY=http://localhost:8080/go,direct
```

Or in your shell profile for persistence.

### Hex (Elixir)

Configure in `~/.hex/hex.config`:

```erlang
{default_url, <<"http://localhost:8080/hex">>}.
```

Or set the environment variable:

```bash
export HEX_MIRROR=http://localhost:8080/hex
```

### pub.dev (Dart/Flutter)

Set the PUB_HOSTED_URL environment variable:

```bash
export PUB_HOSTED_URL=http://localhost:8080/pub
```

### PyPI (pip)

Configure pip to use the proxy:

```bash
pip install --index-url http://localhost:8080/pypi/simple/ package_name
```

Or set in `~/.pip/pip.conf`:

```ini
[global]
index-url = http://localhost:8080/pypi/simple/
```

### Maven

Add to your `~/.m2/settings.xml`:

```xml
<settings>
  <mirrors>
    <mirror>
      <id>proxy</id>
      <mirrorOf>central</mirrorOf>
      <url>http://localhost:8080/maven/</url>
    </mirror>
  </mirrors>
</settings>
```

### NuGet

Configure in `nuget.config`:

```xml
<configuration>
  <packageSources>
    <clear />
    <add key="proxy" value="http://localhost:8080/nuget/v3/index.json" />
  </packageSources>
</configuration>
```

Or use the CLI:

```bash
dotnet nuget add source http://localhost:8080/nuget/v3/index.json -n proxy
```

### Composer (PHP)

Configure in `composer.json`:

```json
{
    "repositories": [
        {
            "type": "composer",
            "url": "http://localhost:8080/composer"
        }
    ]
}
```

Or set globally:

```bash
composer config -g repositories.proxy composer http://localhost:8080/composer
```

### Conan (C/C++)

Add the proxy as a remote:

```bash
conan remote add proxy http://localhost:8080/conan
conan remote disable conancenter
```

Or configure in `~/.conan2/remotes.json`.

### Conda

Configure in `~/.condarc`:

```yaml
channels:
  - http://localhost:8080/conda/main
  - http://localhost:8080/conda/conda-forge
default_channels:
  - http://localhost:8080/conda/main
```

Or set via command:

```bash
conda config --add channels http://localhost:8080/conda/main
```

### CRAN (R)

Set the repository in R:

```r
options(repos = c(CRAN = "http://localhost:8080/cran"))
```

Or in `~/.Rprofile` for persistence:

```r
local({
  r <- getOption("repos")
  r["CRAN"] <- "http://localhost:8080/cran"
  options(repos = r)
})
```

### Docker / Container Registry

Configure Docker to use the proxy as a registry mirror in `/etc/docker/daemon.json`:

```json
{
  "registry-mirrors": ["http://localhost:8080"]
}
```

Then restart Docker:

```bash
sudo systemctl restart docker
```

Or pull images directly:

```bash
docker pull localhost:8080/library/nginx:latest
```

### Debian / APT

Configure APT to use the proxy in `/etc/apt/sources.list.d/proxy.list`:

```
deb http://localhost:8080/debian stable main contrib
```

Replace your existing sources.list entries, then:

```bash
sudo apt update
```

### RPM / Yum / DNF

Configure yum/dnf to use the proxy in `/etc/yum.repos.d/proxy.repo`:

```ini
[proxy-fedora]
name=Fedora via Proxy
baseurl=http://localhost:8080/rpm/releases/$releasever/Everything/$basearch/os/
enabled=1
gpgcheck=0
```

Then:

```bash
sudo dnf clean all
sudo dnf update
```

## Configuration

The proxy can be configured via:
1. Command line flags (highest priority)
2. Environment variables
3. Configuration file (YAML or JSON)

### Command Line Flags

```
-config string           Path to configuration file
-listen string           Address to listen on (default ":8080")
-base-url string         Public URL of this proxy (default "http://localhost:8080")
-storage-url string      Storage URL (file:// or s3://)
-storage-path string     Path to artifact storage directory (deprecated, use -storage-url)
-database-driver string  Database driver: sqlite or postgres (default "sqlite")
-database-path string    Path to SQLite database file (default "./cache/proxy.db")
-database-url string     PostgreSQL connection URL
-log-level string        Log level: debug, info, warn, error (default "info")
-log-format string       Log format: text, json (default "text")
-version                 Print version and exit
```

### Environment Variables

```bash
PROXY_LISTEN=:8080
PROXY_BASE_URL=http://localhost:8080
PROXY_STORAGE_URL=file:///var/cache/proxy/artifacts
PROXY_DATABASE_DRIVER=sqlite
PROXY_DATABASE_PATH=./cache/proxy.db
PROXY_DATABASE_URL=postgres://user:pass@localhost/proxy?sslmode=disable
PROXY_LOG_LEVEL=info
PROXY_LOG_FORMAT=text
```

### Configuration File

```yaml
listen: ":8080"
base_url: "http://localhost:8080"

storage:
  url: "file:///var/cache/proxy/artifacts"
  max_size: "10GB"  # Optional: evict LRU when exceeded

database:
  driver: "sqlite"
  path: "/var/lib/proxy/cache.db"

log:
  level: "info"
  format: "text"

# Optional: override upstream URLs
upstream:
  npm: "https://registry.npmjs.org"
  cargo: "https://index.crates.io"

# Optional: version cooldown (see above)
cooldown:
  default: "3d"
```

Run with config file:

```bash
./proxy -config /etc/proxy/config.yaml
```

### PostgreSQL

SQLite is the default and works well for single-node deployments. For multi-node setups or if you prefer a managed database, switch to Postgres:

```yaml
database:
  driver: "postgres"
  url: "postgres://user:password@localhost:5432/proxy?sslmode=disable"
```

Or via environment variables:

```bash
PROXY_DATABASE_DRIVER=postgres
PROXY_DATABASE_URL=postgres://user:password@localhost:5432/proxy?sslmode=disable
```

The proxy creates tables automatically on first run.

### S3 Storage

The proxy can store cached artifacts in S3 or any S3-compatible service (MinIO, R2, etc.) instead of the local filesystem.

```yaml
storage:
  url: "s3://my-bucket-name?region=us-east-1"
```

For S3-compatible services like MinIO:

```yaml
storage:
  url: "s3://my-bucket?endpoint=http://localhost:9000&disableSSL=true&s3ForcePathStyle=true"
```

Set credentials via standard AWS environment variables (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION`).

## CLI Commands

### serve (default)

Start the proxy server. This is the default command if none is specified.

```bash
proxy serve [flags]
proxy [flags]  # same as 'proxy serve'
```

### mirror

Pre-populate the cache from PURLs, SBOM files, or entire registries. Useful for ensuring offline availability or warming the cache before deployments.

```bash
# Mirror specific package versions
proxy mirror pkg:npm/lodash@4.17.21 pkg:cargo/serde@1.0.0

# Mirror all versions of a package
proxy mirror pkg:npm/lodash

# Mirror from a CycloneDX or SPDX SBOM
proxy mirror --sbom sbom.cdx.json

# Preview what would be mirrored
proxy mirror --dry-run pkg:npm/lodash

# Control parallelism
proxy mirror --concurrency 8 pkg:npm/lodash@4.17.21
```

The mirror command accepts the same storage and database flags as `serve`. Already-cached artifacts are skipped.

A mirror API is also available when the server is running:

```bash
# Start a mirror job
curl -X POST http://localhost:8080/api/mirror \
  -H "Content-Type: application/json" \
  -d '{"purls": ["pkg:npm/lodash@4.17.21"]}'

# Check job status
curl http://localhost:8080/api/mirror/mirror-1

# Cancel a running job
curl -X DELETE http://localhost:8080/api/mirror/mirror-1
```

### stats

Show cache statistics without running the server.

```bash
# Text output
proxy stats

# JSON output
proxy stats -json

# Custom database path
proxy stats -database-path /var/lib/proxy/cache.db

# With PostgreSQL
proxy stats -database-driver postgres -database-url postgres://user:pass@localhost/proxy

# Show top 20 most popular packages
proxy stats -popular 20
```

Example output:

```
Cache Statistics
================

Packages:   45
Versions:   128
Artifacts:  128
Total size: 892.4 MB
Total hits: 1547

Packages by ecosystem:
  npm        32
  cargo      13

Most popular packages:
   1. npm/lodash (342 hits, 24.7 KB)
   2. npm/react (198 hits, 89.3 KB)
   3. cargo/serde (156 hits, 234.1 KB)

Recently cached:
  npm/express@4.18.2 (2024-01-15 14:32, 54.2 KB)
  cargo/tokio@1.35.0 (2024-01-15 14:28, 412.8 KB)
```

## API Endpoints

### Registry Protocols

| Endpoint | Description |
|----------|-------------|
| `GET /` | Dashboard (web UI) |
| `GET /health` | Health check (returns "ok" if healthy) |
| `GET /stats` | Cache statistics (JSON) |
| `GET /metrics` | Prometheus metrics |
| `GET /npm/*` | npm registry protocol |
| `GET /cargo/*` | Cargo sparse index protocol |
| `GET /gem/*` | RubyGems protocol |
| `GET /go/*` | Go module proxy protocol |
| `GET /hex/*` | Hex.pm protocol |
| `GET /pub/*` | pub.dev protocol |
| `GET /pypi/*` | PyPI simple/JSON API |
| `GET /maven/*` | Maven repository protocol |
| `GET /nuget/*` | NuGet V3 API |
| `GET /composer/*` | Composer/Packagist protocol |
| `GET /conan/*` | Conan C/C++ protocol |
| `GET /conda/*` | Conda/Anaconda protocol |
| `GET /cran/*` | CRAN (R) protocol |
| `GET /v2/*` | OCI/Docker registry protocol |
| `GET /debian/*` | Debian/APT repository protocol |
| `GET /rpm/*` | RPM/Yum repository protocol |

### Mirror API

| Endpoint | Description |
|----------|-------------|
| `POST /api/mirror` | Start a mirror job (JSON body with `purls`) |
| `GET /api/mirror/{id}` | Get job status and progress |
| `DELETE /api/mirror/{id}` | Cancel a running job |

### Enrichment API

The proxy provides REST endpoints for package metadata enrichment, vulnerability scanning, and outdated detection.

| Endpoint | Description |
|----------|-------------|
| `GET /api/package/{ecosystem}/{name}` | Get package metadata |
| `GET /api/package/{ecosystem}/{name}/{version}` | Get version metadata with vulnerabilities |
| `GET /api/vulns/{ecosystem}/{name}` | Get all vulnerabilities for a package |
| `GET /api/vulns/{ecosystem}/{name}/{version}` | Get vulnerabilities for a specific version |
| `POST /api/outdated` | Check multiple packages for outdated versions |
| `POST /api/bulk` | Bulk package metadata lookup |

#### Get Package Metadata

```bash
curl http://localhost:8080/api/package/npm/lodash
```

Response:

```json
{
  "ecosystem": "npm",
  "name": "lodash",
  "latest_version": "4.17.21",
  "license": "MIT",
  "license_category": "permissive",
  "description": "Lodash modular utilities",
  "homepage": "https://lodash.com/",
  "repository": "https://github.com/lodash/lodash",
  "registry_url": "https://registry.npmjs.org"
}
```

#### Get Version with Vulnerabilities

```bash
curl http://localhost:8080/api/package/npm/lodash/4.17.0
```

Response:

```json
{
  "package": {
    "ecosystem": "npm",
    "name": "lodash",
    "latest_version": "4.17.21",
    "license": "MIT",
    "license_category": "permissive"
  },
  "version": {
    "ecosystem": "npm",
    "name": "lodash",
    "version": "4.17.0",
    "license": "MIT",
    "published_at": "2016-06-17T03:59:56Z",
    "yanked": false,
    "is_outdated": true
  },
  "vulnerabilities": [
    {
      "id": "GHSA-p6mc-m468-83gw",
      "summary": "Prototype Pollution in lodash",
      "severity": "HIGH",
      "cvss_score": 7.4,
      "fixed_version": "4.17.12"
    }
  ],
  "is_outdated": true,
  "license_category": "permissive"
}
```

#### Check Outdated Packages

```bash
curl -X POST http://localhost:8080/api/outdated \
  -H "Content-Type: application/json" \
  -d '{
    "packages": [
      {"ecosystem": "npm", "name": "lodash", "version": "4.17.0"},
      {"ecosystem": "pypi", "name": "requests", "version": "2.25.0"}
    ]
  }'
```

Response:

```json
{
  "results": [
    {
      "ecosystem": "npm",
      "name": "lodash",
      "version": "4.17.0",
      "latest_version": "4.17.21",
      "is_outdated": true
    },
    {
      "ecosystem": "pypi",
      "name": "requests",
      "version": "2.25.0",
      "latest_version": "2.31.0",
      "is_outdated": true
    }
  ]
}
```

#### Bulk Package Lookup

```bash
curl -X POST http://localhost:8080/api/bulk \
  -H "Content-Type: application/json" \
  -d '{
    "purls": [
      "pkg:npm/lodash@4.17.21",
      "pkg:pypi/requests@2.28.0"
    ]
  }'
```

Response:

```json
{
  "packages": {
    "pkg:npm/lodash": {
      "ecosystem": "npm",
      "name": "lodash",
      "latest_version": "4.17.21",
      "license": "MIT",
      "license_category": "permissive"
    },
    "pkg:pypi/requests": {
      "ecosystem": "pypi",
      "name": "requests",
      "latest_version": "2.31.0",
      "license": "Apache-2.0",
      "license_category": "permissive"
    }
  }
}
```

### Stats Response (HTTP endpoint)

```json
{
  "cached_artifacts": 142,
  "total_size_bytes": 523456789,
  "total_size": "499.2 MB",
  "storage_url": "file:///path/to/cache/artifacts",
  "database_path": "./cache/proxy.db"
}
```

## How It Works

1. Package manager requests package metadata from the proxy
2. Proxy fetches metadata from upstream, rewrites artifact URLs to point at proxy
3. Package manager requests artifact (tarball, crate, etc.)
4. Proxy checks local cache:
   - **Cache hit**: Serve from local storage
   - **Cache miss**: Fetch from upstream, store locally, serve to client
5. Subsequent requests for the same artifact are served from cache

```
┌─────────────┐     ┌─────────┐     ┌──────────┐
│   npm/cargo │────▶│  proxy  │────▶│ upstream │
│   client    │◀────│         │◀────│ registry │
└─────────────┘     └─────────┘     └──────────┘
                         │
                         ▼
                    ┌─────────┐
                    │  cache  │
                    │ storage │
                    └─────────┘
```

## Web Interface

The proxy serves a web UI at the root URL. No separate frontend build is needed -- templates and assets are embedded in the binary.

- **Dashboard** (`/`) -- cache stats, popular packages, recently cached artifacts, and vulnerability overview.
- **Install guide** (`/install`) -- per-ecosystem configuration instructions, so you don't have to look them up here.
- **Package browser** (`/packages`) -- browse all cached packages with filtering by ecosystem and sorting by hits, size, name, or vulnerability count.
- **Search** (`/search?q=...`) -- search cached packages by name.
- **Package detail** (`/package/{ecosystem}/{name}`) -- metadata, license, vulnerabilities, and version list for a package. You can select two versions to compare.
- **Version detail** (`/package/{ecosystem}/{name}/{version}`) -- per-version metadata, integrity hash, artifact cache status, and hit counts.
- **Source browser** (`/package/{ecosystem}/{name}/{version}/browse`) -- browse files inside cached archives with syntax highlighting for text files and image previews.
- **Version diff** (`/package/{ecosystem}/{name}/compare/{v1}...{v2}`) -- side-by-side diff of two cached versions showing added, removed, and changed files.

## Monitoring

The proxy exposes Prometheus metrics at `GET /metrics`. All metric names are prefixed with `proxy_`.

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `proxy_cache_hits_total` | counter | `ecosystem` | Cache hits |
| `proxy_cache_misses_total` | counter | `ecosystem` | Cache misses |
| `proxy_cache_size_bytes` | gauge | | Total size of cached artifacts |
| `proxy_cached_artifacts_total` | gauge | | Number of cached artifacts |
| `proxy_upstream_fetch_duration_seconds` | histogram | `ecosystem` | Time spent fetching from upstream |
| `proxy_upstream_errors_total` | counter | `ecosystem`, `error_type` | Upstream fetch failures |
| `proxy_storage_operation_duration_seconds` | histogram | `operation` | Storage read/write latency |
| `proxy_storage_errors_total` | counter | `operation` | Storage read/write failures |
| `proxy_active_requests` | gauge | | In-flight requests |

Cache size and artifact count are refreshed every 60 seconds. The remaining metrics update on each request.

Scrape config for Prometheus:

```yaml
scrape_configs:
  - job_name: git-pkgs-proxy
    static_configs:
      - targets: ["localhost:8080"]
```

## Production Deployment

### Systemd Service

Create `/etc/systemd/system/proxy.service`:

```ini
[Unit]
Description=git-pkgs proxy
After=network.target

[Service]
Type=simple
User=proxy
ExecStart=/usr/local/bin/proxy -config /etc/proxy/config.yaml
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

Enable and start:

```bash
sudo systemctl enable proxy
sudo systemctl start proxy
```

### Docker

A Dockerfile is included in the repo. Build and run:

```bash
docker build -t proxy .
docker run -p 8080:8080 -v proxy-data:/data proxy
```

With Postgres and S3:

```bash
docker run -p 8080:8080 \
  -e PROXY_DATABASE_DRIVER=postgres \
  -e PROXY_DATABASE_URL=postgres://user:pass@db:5432/proxy \
  -e PROXY_STORAGE_URL=s3://my-bucket?region=us-east-1 \
  -e AWS_ACCESS_KEY_ID=... \
  -e AWS_SECRET_ACCESS_KEY=... \
  proxy
```

### Behind a Reverse Proxy

When running behind nginx, Apache, or another reverse proxy, set `base_url` to your public URL:

```yaml
base_url: "https://proxy.example.com"
```

nginx example:

```nginx
server {
    listen 443 ssl;
    server_name proxy.example.com;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_buffering off;
    }
}
```

## Cache Management

The proxy stores artifacts in the configured storage directory with this structure:

```
cache/artifacts/
├── npm/
│   └── lodash/
│       └── 4.17.21/
│           └── lodash-4.17.21.tgz
├── cargo/
│   └── serde/
│       └── 1.0.193/
│           └── serde-1.0.193.crate
├── oci/
│   └── library/nginx/
│       └── sha256:abc123.../
│           └── sha256:abc123...
├── deb/
│   └── nginx/
│       └── 1.18.0-6/
│           └── nginx_1.18.0-6_amd64.deb
└── rpm/
    └── nginx/
        └── 1.24.0-1.fc39/
            └── nginx-1.24.0-1.fc39.x86_64.rpm
```

Cache metadata is stored in SQLite (default) or PostgreSQL. To clear a local cache:

```bash
rm -rf ./cache/artifacts/*
rm ./cache/proxy.db
```

The proxy will recreate the database on next start.

## Building from Source

Requirements:
- Go 1.25 or later

```bash
git clone https://github.com/git-pkgs/proxy.git
cd proxy
go build -o proxy ./cmd/proxy
```

Run tests:

```bash
go test ./...
```

## License

GPL-3.0-or-later
