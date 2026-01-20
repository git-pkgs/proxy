# git-pkgs proxy

A caching proxy for package registries. Speeds up package downloads by caching artifacts locally, reducing bandwidth usage and improving reliability.

## Supported Registries

| Registry | Language/Platform | URL Resolution | Handler | Completed |
|----------|-------------------|:--------------:|:-------:|:---------:|
| npm | JavaScript | Yes | Yes | ✓ |
| Cargo | Rust | Yes | Yes | ✓ |
| RubyGems | Ruby | Yes | Yes | ✓ |
| Go proxy | Go | Yes | Yes | ✓ |
| Hex | Elixir | Yes | Yes | ✓ |
| pub.dev | Dart | Yes | Yes | ✓ |
| PyPI | Python | Yes | Yes | ✓ |
| Maven | Java | Yes | Yes | ✓ |
| NuGet | .NET | Yes | Yes | ✓ |
| Composer | PHP | Yes | Yes | ✓ |
| Conan | C/C++ | Yes | Yes | ✓ |
| Conda | Python/R | Yes | Yes | ✓ |
| CRAN | R | Yes | Yes | ✓ |
| Alpine | Alpine Linux | No | No | ✗ |
| Arch | Arch Linux | No | No | ✗ |
| Chef | Chef | No | No | ✗ |
| Container | OCI | No | No | ✗ |
| Debian | Debian/Ubuntu | No | No | ✗ |
| Generic | Any | No | No | ✗ |
| Helm | Kubernetes | No | No | ✗ |
| RPM | RHEL/Fedora | No | No | ✗ |
| Swift | Swift | No | No | ✗ |
| Vagrant | Vagrant | No | No | ✗ |

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

## Configuration

The proxy can be configured via:
1. Command line flags (highest priority)
2. Environment variables
3. Configuration file (YAML or JSON)

### Command Line Flags

```
-config string      Path to configuration file
-listen string      Address to listen on (default ":8080")
-base-url string    Public URL of this proxy (default "http://localhost:8080")
-storage string     Path to artifact storage directory (default "./cache/artifacts")
-database string    Path to SQLite database file (default "./cache/proxy.db")
-log-level string   Log level: debug, info, warn, error (default "info")
-log-format string  Log format: text, json (default "text")
-version            Print version and exit
```

### Environment Variables

```bash
PROXY_LISTEN=:8080
PROXY_BASE_URL=http://localhost:8080
PROXY_STORAGE_PATH=./cache/artifacts
PROXY_DATABASE_PATH=./cache/proxy.db
PROXY_LOG_LEVEL=info
PROXY_LOG_FORMAT=text
```

### Configuration File

```yaml
listen: ":8080"
base_url: "http://localhost:8080"

storage:
  path: "/var/cache/proxy/artifacts"
  max_size: "10GB"  # Optional: evict LRU when exceeded

database:
  path: "/var/lib/proxy/cache.db"

log:
  level: "info"
  format: "text"

# Optional: override upstream URLs
upstream:
  npm: "https://registry.npmjs.org"
  cargo: "https://index.crates.io"
```

Run with config file:

```bash
./proxy -config /etc/proxy/config.yaml
```

## CLI Commands

### serve (default)

Start the proxy server. This is the default command if none is specified.

```bash
proxy serve [flags]
proxy [flags]  # same as 'proxy serve'
```

### stats

Show cache statistics without running the server.

```bash
# Text output
proxy stats

# JSON output
proxy stats -json

# Custom database path
proxy stats -database /var/lib/proxy/cache.db

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

| Endpoint | Description |
|----------|-------------|
| `GET /` | Welcome message and endpoint list |
| `GET /health` | Health check (returns "ok" if healthy) |
| `GET /stats` | Cache statistics (JSON) |
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

### Stats Response (HTTP endpoint)

```json
{
  "cached_artifacts": 142,
  "total_size_bytes": 523456789,
  "total_size": "499.2 MB",
  "storage_path": "./cache/artifacts",
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

```dockerfile
FROM golang:1.23-alpine AS build
WORKDIR /app
COPY . .
RUN go build -o proxy ./cmd/proxy

FROM alpine:latest
RUN apk --no-cache add ca-certificates
COPY --from=build /app/proxy /usr/local/bin/
EXPOSE 8080
VOLUME ["/data"]
CMD ["proxy", "-storage", "/data/artifacts", "-database", "/data/proxy.db"]
```

Build and run:

```bash
docker build -t proxy .
docker run -p 8080:8080 -v proxy-data:/data proxy
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
└── cargo/
    └── serde/
        └── 1.0.193/
            └── serde-1.0.193.crate
```

Cache metadata is stored in an SQLite database. To clear the cache:

```bash
rm -rf ./cache/artifacts/*
rm ./cache/proxy.db
```

The proxy will recreate the database on next start.

## Building from Source

Requirements:
- Go 1.23 or later

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
