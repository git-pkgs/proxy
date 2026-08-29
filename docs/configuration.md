# Configuration

The proxy can be configured via command line flags, environment variables, or a configuration file. Command line flags take precedence over environment variables, which take precedence over the configuration file.

## Configuration File

Create a YAML or JSON file and pass it with `-config`:

```bash
proxy serve -config config.yaml
```

See `config.example.yaml` in the repository root for a complete example.

## Server Settings

| Config | Environment | Flag | Default | Description |
|--------|-------------|------|---------|-------------|
| `listen` | `PROXY_LISTEN` | `-listen` | `:8080` | Address to listen on |
| `base_url` | `PROXY_BASE_URL` | `-base-url` | `http://localhost:8080` | Public URL package managers use to reach this proxy |
| `ui_base_url` | `PROXY_UI_URL` | - | (defaults to `base_url`) | Public URL where the web UI is reached. Set separately when the UI lives behind a different hostname than package endpoints (e.g. public domain vs Docker network alias). Used for canonical/og:url tags and the install guide banner. The proxy still serves package endpoints on the same listener, so any reverse proxy fronting the UI publicly should restrict the public route to `PathPrefix(/ui)` to avoid exposing package endpoints. |

## Storage

The proxy stores cached artifacts using gocloud.dev/blob, supporting local filesystem and S3-compatible storage.

### Local Filesystem

```yaml
storage:
  url: "file:///var/cache/proxy"
```

Or using the legacy path option:

```yaml
storage:
  path: "./cache/artifacts"
```

| Config | Environment | Flag | Description |
|--------|-------------|------|-------------|
| `storage.url` | `PROXY_STORAGE_URL` | `-storage-url` | Storage URL (file:// or s3://) |
| `storage.path` | `PROXY_STORAGE_PATH` | `-storage-path` | Local path (deprecated, use url) |
| `storage.max_size` | `PROXY_STORAGE_MAX_SIZE` | - | Max cache size (e.g., "10GB") |

### Amazon S3

```yaml
storage:
  url: "s3://my-bucket"
```

Configure credentials via environment variables:

```bash
export AWS_ACCESS_KEY_ID=your-key
export AWS_SECRET_ACCESS_KEY=your-secret
export AWS_REGION=us-east-1
```

### S3-Compatible (MinIO, etc.)

```yaml
storage:
  url: "s3://my-bucket?endpoint=http://localhost:9000&disableSSL=true&s3ForcePathStyle=true"
```

## Database

The proxy supports SQLite (default) and PostgreSQL for storing package metadata.

### SQLite

```yaml
database:
  driver: "sqlite"
  path: "./cache/proxy.db"
```

| Config | Environment | Flag | Description |
|--------|-------------|------|-------------|
| `database.driver` | `PROXY_DATABASE_DRIVER` | `-database-driver` | `sqlite` or `postgres` |
| `database.path` | `PROXY_DATABASE_PATH` | `-database-path` | SQLite file path |

### PostgreSQL

```yaml
database:
  driver: "postgres"
  url: "postgres://user:password@localhost:5432/proxy?sslmode=disable"
```

| Config | Environment | Flag | Description |
|--------|-------------|------|-------------|
| `database.url` | `PROXY_DATABASE_URL` | `-database-url` | PostgreSQL connection URL |

## Logging

```yaml
log:
  level: "info"
  format: "text"
```

| Config | Environment | Flag | Values |
|--------|-------------|------|--------|
| `log.level` | `PROXY_LOG_LEVEL` | `-log-level` | `debug`, `info`, `warn`, `error` |
| `log.format` | `PROXY_LOG_FORMAT` | `-log-format` | `text`, `json` |

## Access Log

The optional access log records client requests and each HTTP exchange with an upstream registry. It is always written as JSONL, with one JSON object per line. Records for the same client request share a `request_id`.

```yaml
access_log:
  path: "/var/log/proxy/access.jsonl"
```

| Config | Environment | Flag | Description |
|--------|-------------|------|-------------|
| `access_log.path` | `PROXY_ACCESS_LOG_PATH` | `-access-log` | File to append JSONL records to; empty disables the log |

The parent directory must exist and be writable when the proxy starts. A newly created log file is readable and writable only by the proxy process owner.

A request that receives a rate limit response from an upstream can produce records like these:

```json
{"time":"2026-08-16T12:00:00Z","event":"upstream","request_id":"host/example-000001","method":"GET","url":"https://registry.example/packages/example","status_code":429,"duration_ms":42}
{"time":"2026-08-16T12:00:00Z","event":"request","request_id":"host/example-000001","method":"GET","path":"/npm/example","status_code":502,"duration_ms":43,"remote_addr":"192.0.2.10:41234"}
```

Upstream retries and OCI authentication calls are separate `upstream` records, so the log preserves every status returned over the wire. Network failures have an `error` field and no `status_code`. URL credentials, query strings, and fragments are omitted from both upstream URLs and client paths.

## Upstream Registries

Each upstream used by a built-in package route can be set in YAML or JSON under `upstream`, or with its matching environment variable. Existing installations keep the same public upstreams by default. Trailing slashes are ignored.

| Config | Environment | Default |
|--------|-------------|---------|
| `upstream.allow_private_hosts` | `PROXY_UPSTREAM_ALLOW_PRIVATE_HOSTS` | `[]` |
| `upstream.allow_loopback` | `PROXY_UPSTREAM_ALLOW_LOOPBACK` | `false` |
| `upstream.npm` | `PROXY_UPSTREAM_NPM` | `https://registry.npmjs.org` |
| `upstream.cargo` | `PROXY_UPSTREAM_CARGO` | `https://index.crates.io` |
| `upstream.cargo_download` | `PROXY_UPSTREAM_CARGO_DOWNLOAD` | `https://static.crates.io/crates` |
| `upstream.gem` | `PROXY_UPSTREAM_GEM` | `https://rubygems.org` |
| `upstream.go` | `PROXY_UPSTREAM_GO` | `https://proxy.golang.org` |
| `upstream.hex` | `PROXY_UPSTREAM_HEX` | `https://repo.hex.pm` |
| `upstream.hex_api` | `PROXY_UPSTREAM_HEX_API` | `https://hex.pm` |
| `upstream.pub` | `PROXY_UPSTREAM_PUB` | `https://pub.dev` |
| `upstream.pypi` | `PROXY_UPSTREAM_PYPI` | `https://pypi.org` |
| `upstream.pypi_download` | `PROXY_UPSTREAM_PYPI_DOWNLOAD` | `https://files.pythonhosted.org` |
| `upstream.maven` | `PROXY_UPSTREAM_MAVEN` | `https://repo1.maven.org/maven2` |
| `upstream.gradle_plugin_portal` | `PROXY_UPSTREAM_GRADLE_PLUGIN_PORTAL` | `https://plugins.gradle.org/m2` |
| `upstream.nuget` | `PROXY_UPSTREAM_NUGET` | `https://api.nuget.org` |
| `upstream.nuget_search` | `PROXY_UPSTREAM_NUGET_SEARCH` | `https://azuresearch-usnc.nuget.org` |
| `upstream.composer` | `PROXY_UPSTREAM_COMPOSER` | `https://packagist.org` |
| `upstream.composer_repository` | `PROXY_UPSTREAM_COMPOSER_REPOSITORY` | `https://repo.packagist.org` |
| `upstream.conan` | `PROXY_UPSTREAM_CONAN` | `https://center.conan.io` |
| `upstream.conda` | `PROXY_UPSTREAM_CONDA` | `https://conda.anaconda.org` |
| `upstream.cran` | `PROXY_UPSTREAM_CRAN` | `https://cloud.r-project.org` |
| `upstream.julia` | `PROXY_UPSTREAM_JULIA` | `https://pkg.julialang.org` |
| `upstream.oci_default` | `PROXY_UPSTREAM_OCI_DEFAULT` | `https://registry-1.docker.io` |
| `upstream.debian` | `PROXY_UPSTREAM_DEBIAN` | `http://deb.debian.org/debian` |
| `upstream.rpm` | `PROXY_UPSTREAM_RPM` | `https://dl.fedoraproject.org/pub/fedora/linux` |

Private, ULA, CGNAT, and loopback addresses are rejected by default. Add each private upstream hostname or IP address to `upstream.allow_private_hosts`. The matching environment variable accepts a comma-separated list. Loopback upstreams also require `upstream.allow_loopback: true`. That setting permits upstream requests and redirects to reach any loopback address.

```yaml
upstream:
  allow_private_hosts:
    - "upstream-proxy.internal"
  pypi: "http://upstream-proxy.internal/pypi"
  pypi_download: "http://upstream-proxy.internal/pypi"
```

For protocols that use separate metadata and download services, configure both values. They may point to the same endpoint when chaining proxies:

```yaml
upstream:
  pypi: "https://upstream-proxy.example.com/pypi"
  pypi_download: "https://upstream-proxy.example.com/pypi"
  nuget: "https://upstream-proxy.example.com/nuget"
  nuget_search: "https://upstream-proxy.example.com/nuget"
  composer: "https://upstream-proxy.example.com/composer"
  composer_repository: "https://upstream-proxy.example.com/composer"
```

`upstream.hex_api` is used for cooldown timestamps and must expose Hex's `/api/packages/{name}` JSON endpoint.

Helm HTTP repositories and additional OCI registries are configured as named maps:

```yaml
upstream:
  # Named HTTP Helm chart repositories, served at /helm/{name}/.
  helm:
    bitnami: "https://charts.bitnami.com/bitnami"

  # Named OCI registries. Select one with the repository prefix
  # upstream/{name}/, e.g. oci://proxy.example.com/upstream/ghcr/owner/chart.
  oci:
    ghcr: "https://ghcr.io"
```

Helm HTTP repositories are read-only. The proxy fetches and rewrites each
repository's `index.yaml` so chart archives are downloaded through the proxy.
Chart archives are retained only when their SHA-256 digest matches the digest
listed in the index. Relative and absolute chart URLs are both supported.

`upstream.oci_default` sets the registry used by unprefixed `/v2` requests,
while `upstream.oci` selects named registries through the `upstream/{name}/`
repository prefix. For example, `oci://proxy.example.com/upstream/ghcr/owner/chart`
uses the `ghcr` registry with `owner/chart` as its repository.
When the proxy uses plain HTTP (for example `localhost:8080`), pass
`--plain-http` to Helm OCI commands.

## Authentication

Configure authentication for private upstream registries. The same authentication-aware client is used for metadata and artifact downloads, and credentials can reference environment variables using `${VAR_NAME}` syntax.

OCI registries that return a Bearer challenge from a `/v2/{repository}/…` endpoint are handled automatically. The proxy discovers the token realm from `WWW-Authenticate`, applies any configured credentials for the token URL, and reuses the scoped token until shortly before it expires.

### Bearer Token

Used by npm, GitHub Package Registry, and many other registries:

```yaml
upstream:
  auth:
    "https://registry.npmjs.org":
      type: bearer
      token: "${NPM_TOKEN}"
    "https://npm.pkg.github.com":
      type: bearer
      token: "${GITHUB_TOKEN}"
```

### Basic Authentication

Used by PyPI, Artifactory, and others:

```yaml
upstream:
  auth:
    "https://pypi.org":
      type: basic
      username: "__token__"
      password: "${PYPI_TOKEN}"
    "https://artifactory.mycompany.com":
      type: basic
      username: "deploy"
      password: "${ARTIFACTORY_PASSWORD}"
```

### Custom Header

For registries that use non-standard authentication headers:

```yaml
upstream:
  auth:
    "https://maven.mycompany.com":
      type: header
      header_name: "X-Auth-Token"
      header_value: "${MAVEN_TOKEN}"
```

### URL Matching

Auth keys must be absolute URLs. Matching compares the scheme, host, effective port, and path-segment prefix, preventing credentials for `registry.example.com` from being sent to a lookalike host such as `registry.example.com.evil.test`. The longest matching scope wins, so you can configure different credentials for different paths:

```yaml
upstream:
  auth:
    # All requests to this registry
    "https://registry.mycompany.com":
      type: bearer
      token: "${REGISTRY_TOKEN}"
    # Override for a specific scope
    "https://registry.mycompany.com/@private":
      type: bearer
      token: "${PRIVATE_TOKEN}"
```

## Gradle Build Cache

The `/gradle` endpoint supports optional safeguards for upload control and cache retention.

```yaml
gradle:
  build_cache:
    read_only: false
    max_upload_size: "100MB"
    max_age: "168h"
    max_size: "20GB"
    sweep_interval: "10m"
```

| Config | Environment | Description |
|--------|-------------|-------------|
| `gradle.build_cache.read_only` | `PROXY_GRADLE_BUILD_CACHE_READ_ONLY` | Disable PUT uploads and keep GET/HEAD read-only |
| `gradle.build_cache.max_upload_size` | `PROXY_GRADLE_BUILD_CACHE_MAX_UPLOAD_SIZE` | Maximum accepted PUT body size (must be > 0) |
| `gradle.build_cache.max_age` | `PROXY_GRADLE_BUILD_CACHE_MAX_AGE` | Delete entries older than this duration (default `168h`, set `0` to disable) |
| `gradle.build_cache.max_size` | `PROXY_GRADLE_BUILD_CACHE_MAX_SIZE` | Total size cap for `_gradle/http-build-cache`, deleting oldest first (`0` disables) |
| `gradle.build_cache.sweep_interval` | `PROXY_GRADLE_BUILD_CACHE_SWEEP_INTERVAL` | Frequency for background eviction sweeps |

`max_age` and `max_size` are independent and can be combined. When both are set, age-based eviction runs first, then size-based eviction trims remaining entries oldest-first.

## Cooldown

The cooldown feature hides package versions published too recently, giving the community time to spot malicious releases before they reach your projects. When a version is within its cooldown period, it's stripped from metadata responses so package managers won't install it.

```yaml
cooldown:
  default: "3d"
  ecosystems:
    npm: "7d"
    cargo: "0"
  packages:
    "pkg:npm/lodash": "0"
    "pkg:npm/@babel/core": "14d"
```

| Config | Environment | Description |
|--------|-------------|-------------|
| `cooldown.default` | `PROXY_COOLDOWN_DEFAULT` | Global default cooldown |
| `cooldown.ecosystems` | - | Per-ecosystem overrides |
| `cooldown.packages` | - | Per-package overrides (keyed by PURL) |

Durations support days (`7d`), hours (`48h`), and minutes (`30m`). Set to `0` to disable.

Package PURL keys are normalized to canonical form before matching, so `pkg:npm/@babel/core` and `pkg:npm/%40babel/core` are equivalent, as are `pkg:pypi/Django` and `pkg:pypi/django`. If both forms configure the same package, the canonical entry wins.

Resolution order: package override, then ecosystem override, then global default. This lets you set a conservative default while exempting trusted packages.

Currently supported for npm, PyPI, pub.dev, Composer, Cargo, NuGet, Conda, RubyGems, and Hex. These ecosystems include publish timestamps in their metadata.

Note: Hex cooldown requires disabling registry signature verification since the proxy re-encodes the protobuf payload without the original signature. Set `HEX_NO_VERIFY_REPO_ORIGIN=1` or configure your repo with `no_verify: true`.

## Metadata Caching

By default the proxy fetches metadata fresh from upstream on every request. Enable `cache_metadata` to store metadata responses in the database and storage backend for offline fallback. When upstream is unreachable, the proxy serves the last cached copy. ETag-based revalidation avoids re-downloading unchanged metadata.

OCI manifests are always cached because cached image blobs cannot be pulled without their manifests. Digest-addressed manifests are immutable and served directly from cache. Tag-addressed manifests follow `metadata_ttl`, revalidate when stale, and fall back to the last cached response when the registry is unavailable.

```yaml
cache_metadata: true
```

Or via environment variable: `PROXY_CACHE_METADATA=true`.

The `proxy mirror` command always enables metadata caching regardless of this setting.

### Metadata TTL

When metadata caching is enabled, `metadata_ttl` controls how long a cached response is considered fresh before revalidating with upstream. During the TTL window, cached metadata is served directly without contacting upstream, reducing latency and upstream load.

```yaml
metadata_ttl: "5m"   # default
```

Or via environment variable: `PROXY_METADATA_TTL=10m`.

Set to `"0"` to always revalidate with upstream (ETag-based conditional requests still avoid re-downloading unchanged content).

When upstream is unreachable and the cached entry is past its TTL, the proxy serves the stale cached copy with a `Warning: 110 - "Response is Stale"` header so clients can tell the data may be outdated.

### Metadata size limit

Upstream metadata responses are buffered in memory before being rewritten and served. `metadata_max_size` caps that buffer to protect against OOM from a misbehaving upstream. Some npm packages with thousands of versions (for example `renovate`) exceed the 100 MB default, so raise this if you see `metadata response exceeds size limit` in the logs.

```yaml
metadata_max_size: "100MB"   # default
```

Or via environment variable: `PROXY_METADATA_MAX_SIZE=250MB`.

## Upstream HTTP timeout

Protocol handlers use a shared HTTP client for upstream requests such as metadata fetches and pass-through file downloads. `http_timeout` sets that client's per-request timeout. Raise it if slow upstreams or large metadata responses cause `context deadline exceeded` errors.

```yaml
http_timeout: "30s"   # default
```

Or via environment variable: `PROXY_HTTP_TIMEOUT=2m`.

Set to `"0"` to disable the timeout entirely (requests then rely only on the server's write timeout).

## Mirror API

The `/api/mirror` endpoints are disabled by default. Enable them to allow starting mirror jobs via HTTP:

```yaml
mirror_api: true
```

Or via environment variable: `PROXY_MIRROR_API=true`.

When disabled, the endpoints are not registered and return 404.

## Mirror Command

The `proxy mirror` command pre-populates the cache from various sources. It accepts the same storage and database flags as `serve`.

| Flag | Default | Description |
|------|---------|-------------|
| `--sbom` | | Path to CycloneDX or SPDX SBOM file |
| `--concurrency` | `4` | Number of parallel downloads |
| `--dry-run` | `false` | Show what would be mirrored without downloading |
| `--config` | | Path to configuration file |
| `--storage-url` | | Storage URL |
| `--database-driver` | | Database driver |
| `--database-path` | | SQLite database file |
| `--database-url` | | PostgreSQL connection URL |

Positional arguments are treated as PURLs:

```bash
proxy mirror pkg:npm/lodash@4.17.21 pkg:cargo/serde@1.0.0
```

## Docker

### SQLite with Local Storage

```bash
docker compose up
```

### PostgreSQL with Local Storage

```bash
docker compose --profile postgres up
```

### PostgreSQL with S3 (MinIO)

```bash
docker compose --profile s3 up
```

## Example Configurations

### Minimal (defaults)

```yaml
listen: ":8080"
```

### Production with PostgreSQL and S3

```yaml
listen: ":8080"
base_url: "https://proxy.example.com"

storage:
  url: "s3://my-cache-bucket"
  max_size: "100GB"

database:
  driver: "postgres"
  url: "postgres://proxy:secret@db.example.com:5432/proxy?sslmode=require"

log:
  level: "info"
  format: "json"
```

### Private npm Registry

```yaml
listen: ":8080"
base_url: "http://localhost:8080"

upstream:
  npm: "https://npm.pkg.github.com"
  auth:
    npm:
      type: bearer
      token: "${GITHUB_TOKEN}"
```
