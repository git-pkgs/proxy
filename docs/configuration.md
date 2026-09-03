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
| `upstream.npm_full_metadata` | `PROXY_UPSTREAM_NPM_FULL_METADATA` | `false` |
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
| `upstream.swift` | `PROXY_UPSTREAM_SWIFT` | `https://tuist.dev/api/registry/swift` |
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

Generic HTTP upstreams proxy plain downloads from fixed base URLs:

```yaml
upstream:
  # Named HTTP upstreams, served at /generic/{name}/. The rest of the
  # request path and the query string are appended to the upstream URL.
  generic:
    github: "https://github.com"
    github-api: "https://api.github.com"
  auth:
    # Optional: raise the GitHub API rate limit. Scoped to this host only,
    # so the token is never sent to the object store GitHub redirects to.
    "https://api.github.com":
      type: bearer
      token: "${GITHUB_TOKEN}"
```

Only configured upstreams are reachable, so this is not an open HTTP proxy.
Paths shaped like `{owner}/{repo}/releases/download/{tag}/{asset}` are
version-pinned GitHub release assets: they are stored in the artifact cache
and served from it without revalidation, including while the upstream is
down. Every other path is served through the metadata cache (`cache_metadata`
must be enabled for offline fallback): fresh within `metadata_ttl`, then
revalidated with the upstream's `ETag`/`Last-Modified`, and served stale with
a `Warning: 110` header when the upstream fails, refuses or rate-limits the
request. Metadata responses are buffered up to `metadata_max_size`, so keep
large mutable downloads (`releases/latest/download/...`) off this route.

This is the cache behind [mise](https://mise.jdx.dev)'s aqua backend; see the
mise section in the README for the client-side `url_replacements`.

`upstream.oci_default` sets the registry used by unprefixed `/v2` requests,
while `upstream.oci` selects named registries through the `upstream/{name}/`
repository prefix. For example, `oci://proxy.example.com/upstream/ghcr/owner/chart`
uses the `ghcr` registry with `owner/chart` as its repository.
When the proxy uses plain HTTP (for example `localhost:8080`), pass
`--plain-http` to Helm OCI commands.

```yaml
upstream:

  # Named Alpine APK repositories, served at /apk/{name}/.
  apk:
    alpine: "https://dl-cdn.alpinelinux.org/alpine"
```

Alpine APK repositories are read-only. Requests to `/apk/{name}/…` mirror the
upstream layout, e.g. `/apk/alpine/v3.22/main/x86_64/APKINDEX.tar.gz`. Indexes
(v2 `APKINDEX.tar.gz`, v3 `Packages.adb`) and detached signatures are cached
with the metadata TTL and served byte-for-byte unchanged so apk signature
verification keeps working; `.apk` packages use the shared artifact cache.
When `upstream.apk` is empty, a single repository named `alpine` pointing at
the official mirror is available; configuring any entry replaces that default.

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

### AWS ECR

Private ECR registries issue authorization tokens that expire after 12 hours. The `ecr` auth type calls `ecr:GetAuthorizationToken` on demand, caches the result, and refreshes it shortly before expiry, so no static credential appears in the config file:

```yaml
upstream:
  oci:
    ecr: "https://123456789012.dkr.ecr.eu-west-1.amazonaws.com"
  auth:
    "https://123456789012.dkr.ecr.eu-west-1.amazonaws.com":
      type: ecr
```

AWS credentials are resolved by the SDK's default chain, which covers EKS IAM Roles for Service Accounts (IRSA), EC2/ECS instance profiles, `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` environment variables, and `~/.aws/credentials`. The IAM identity needs the `ecr:GetAuthorizationToken` action plus the usual `ecr:BatchGetImage` / `ecr:GetDownloadUrlForLayer` permissions on the target repositories. The region is inferred from private ECR IPv4, dual-stack, and FIPS hostnames. For other endpoint formats, set `region` explicitly or configure a default region for the SDK.

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

## Artifact Scanning

Cooldown only ever looks at a version's *publish timestamp* — it never inspects the actual bytes of an artifact. Artifact scanning runs after a fetched artifact is staged into storage but before it becomes visible from cache, so an external scanner (trivy, ClamAV, Wiz, or any custom service) can block a bad verdict from ever reaching a client.

```yaml
scanning:
  enabled: true
  fail_open: false
  timeout: 30s
  signing_key: ${PROXY_SCANNING_SIGNING_KEY}
  fetch_base_url: http://proxy.internal:8080
  scanners:
    - name: clamav
      url: http://clamav-adapter:8080/scan
      mode: block
    - name: trivy
      url: http://trivy-adapter:8081/scan
      mode: monitor
      ecosystems: [npm, pypi]
```

| Config | Environment | Description |
|--------|-------------|-------------|
| `scanning.enabled` | `PROXY_SCANNING_ENABLED` | Turn on the scan gate. When false (default), artifacts are cached exactly as if scanning didn't exist |
| `scanning.fail_open` | `PROXY_SCANNING_FAIL_OPEN` | Treat scanner errors/timeouts as allow instead of block. Default is fail-closed |
| `scanning.timeout` | `PROXY_SCANNING_TIMEOUT` | Per-scan-call timeout, Go duration syntax (default `30s`) |
| `scanning.signing_key` | `PROXY_SCANNING_SIGNING_KEY` | Signs pull requests to the internal scan-fetch route. Required whenever `enabled` is true |
| `scanning.fetch_base_url` | `PROXY_SCANNING_FETCH_BASE_URL` | Address scanners use to reach this proxy to pull staged artifacts. Defaults to `base_url` |
| `scanning.scanners` | - | List of external scanning services (YAML only) |
| `scanning.scanners[].name` | - | Identifies this scanner in logs and metrics |
| `scanning.scanners[].url` | - | Endpoint the proxy POSTs scan notifications to |
| `scanning.scanners[].mode` | - | `block` (default) or `monitor` |
| `scanning.scanners[].ecosystems` | - | Restricts this scanner to specific ecosystems (e.g. `npm`, `pypi`). Empty means all ecosystems |
| `scanning.scanners[].headers` | - | Extra HTTP headers sent with every scan request (e.g. for authenticating to the scanner service). Values support `${VAR_NAME}` expansion |

### How caching defers to a scan verdict

The proxy never uploads artifact bytes to a scanner. When an artifact is fetched from upstream, it's stored to the configured storage backend first, exactly as without scanning. If scanning is enabled for the artifact's ecosystem, the proxy then notifies each applicable scanner with package metadata and a short-lived, HMAC-signed URL pointing at the internal `/_internal/scan-fetch` route; each scanner GETs that URL itself to pull the exact bytes staged in storage and runs its own scan against them.

Scanners configured for the same ecosystem all run concurrently, never sequentially. The moment any `block`-mode scanner reports a not-allowed verdict (or errors, unless `fail_open` is set), the proxy cancels the in-flight calls to the other scanners and deletes the staged artifact — it's never committed to the cache database, so it was never visible to a client. If nothing blocks, the proxy waits for every `block`-mode scanner to finish before caching the artifact and serving it. A `monitor`-mode scanner's findings are logged and never gate the wait or the caching decision, even when it reports not-allowed.

A blocked download surfaces to the client as `403 Forbidden` with the scanner's reason, across every ecosystem handler.

### Scanner HTTP contract

Any external service that implements this contract can act as a scanner — a trivy wrapper, a clamav-rest bridge, a Wiz connector, or an in-house service. The proxy POSTs a notify request to `scanning.scanners[].url` and waits for a JSON verdict.

**Request**

| Field | Type | Description |
|-------|------|-------------|
| `ecosystem` | string | e.g. `npm`, `pypi`, `cargo` |
| `name` | string | Package name |
| `version` | string | Package version |
| `filename` | string | Artifact filename |
| `purl` | string | Package URL (PURL) identifying this exact version |
| `content_type` | string | Artifact content type |
| `size` | integer | Artifact size in bytes |
| `fetch_url` | string | Short-lived signed URL; GET this to retrieve the exact staged bytes |

```json
{
  "ecosystem": "npm", "name": "left-pad", "version": "1.0.0",
  "filename": "left-pad-1.0.0.tgz", "purl": "pkg:npm/left-pad@1.0.0",
  "content_type": "application/octet-stream", "size": 1234,
  "fetch_url": "https://proxy.internal/_internal/scan-fetch?path=...&exp=...&sig=..."
}
```

**Response**

| Field | Type | Description |
|-------|------|-------------|
| `allowed` | boolean | Whether the artifact may be cached and served |
| `reason` | string | Human-readable reason, surfaced to the client when `allowed` is false |
| `findings` | array | Optional list of `{"severity", "title", "description"}` objects |

```json
{
  "allowed": false,
  "reason": "malware detected",
  "findings": [
    {"severity": "critical", "title": "Trojan.GenericKD", "description": "..."}
  ]
}
```

The scanner must respond within `scanning.timeout` (default `30s`); a timeout is treated the same as a `block` verdict unless `fail_open` is set.

### The `/_internal/scan-fetch` route

`fetch_url` points at an internal route, `/_internal/scan-fetch`, that streams a staged object straight from the proxy's storage backend via a short-lived HMAC-signed token (`path`, `exp`, `sig` query parameters). This works identically across every storage backend — local filesystem, S3, GCS, Azure — since it never depends on a backend-specific presigned URL, only on the one storage operation every backend already implements.

This route is not part of the public API. It's meant only for scanners to pull artifacts they've been notified about, and should be restricted to internal-network access at the ingress/network-policy layer — the HMAC scoping (one object, a short TTL) limits what a leaked token can do, but isn't a substitute for network restriction. Its query parameters are also documented in the generated [OpenAPI spec](../README.md#openapi-swagger).

The route only exists when scanning is actually configured: it's not mounted at all unless at least one scanner is enabled and `scanning.signing_key` is set, and it also refuses every request with `404` if either condition somehow isn't met at request time. There is no way to reach it, even with a forged token, when scanning is disabled.

## Metadata Caching

By default the proxy fetches metadata fresh from upstream on every request. Enable `cache_metadata` to store metadata responses in the database and storage backend for offline fallback. When upstream is unreachable, the proxy serves the last cached copy. ETag-based revalidation avoids re-downloading unchanged metadata.

OCI manifests and tag lists are always cached because cached image blobs cannot be pulled without their manifests and offline clients may need tag resolution. Digest-addressed manifests are immutable and served directly from cache. Tag-addressed manifests and tag lists follow `metadata_ttl`, revalidate when stale, and fall back to the last cached response when the registry is unavailable.

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

Start a mirror job with either PURLs or an inline CycloneDX or SPDX JSON document:

```bash
curl -X POST http://localhost:8080/api/mirror \
  -H "Content-Type: application/json" \
  -d '{"sbom":{"bomFormat":"CycloneDX","components":[{"purl":"pkg:npm/lodash@4.17.21"}]}}'
```

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
