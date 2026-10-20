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
| `trust_forwarded_for` | `PROXY_TRUST_FORWARDED_FOR` | - | `false` | Attribute requests to the leftmost `X-Forwarded-For` entry instead of the TCP peer address, in the structured log, the access log and the request-source table on `/ui/analytics`. |
| `ui_request_sources` | `PROXY_UI_REQUEST_SOURCES` | - | `false` | Show the request-source table on `/ui/analytics`, which reports caller addresses, the tool each ran and how much each pulled. |
| `ui_failure_details` | `PROXY_UI_FAILURE_DETAILS` | - | `false` | List the latest failures behind each failure count on `/ui/analytics`: the package, its upstream URL or storage path, and the error. URLs are shown without credentials or query strings. |
| `http_timeout` | `PROXY_HTTP_TIMEOUT` | - | `30s` | Timeout for upstream metadata and pass-through requests. |
| `write_timeout` | `PROXY_WRITE_TIMEOUT` | - | `5m` | Response write deadline, including download and scanning time before the response starts. |
| `artifact_timeout` | `PROXY_ARTIFACT_TIMEOUT` | - | `5m` | Timeout for each upstream artifact download while serving requests; excludes subsequent scanning. Does not limit `proxy mirror`. |

These timeouts accept Go durations such as `30s`, `5m`, or `1h`. An omitted value uses the default; `"0"` disables that timeout. Invalid or negative values are rejected at startup. Environment variables override file values.

For long scans, increase `write_timeout` to cover the download, scan, and response transfer. `scanning.timeout` controls the scan separately; changing it alone does not extend the response deadline. `artifact_timeout` is independent of both. For example:

```yaml
write_timeout: "18m"
artifact_timeout: "10m"
scanning:
  timeout: "16m"
```

These are independent limits, not reserved time slices: a download approaching 10 minutes followed by a 16-minute scan would still exceed an 18-minute response deadline. Any reverse proxy in front of the server also needs sufficient timeout allowance. The upstream transport's existing response-header timeout remains unchanged.

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
| `storage.cache_artifacts` | `PROXY_STORAGE_CACHE_ARTIFACTS` | - | Store fetched artifacts (default: true); `false` streams them from upstream |
| `storage.retention.default` | `PROXY_STORAGE_RETENTION_DEFAULT` | - | Evict artifacts not served for this long (e.g. "30d"); see [Retention](#retention) |
| `storage.retention.sweep_interval` | `PROXY_STORAGE_RETENTION_SWEEP_INTERVAL` | - | How often expired artifacts are looked for (default: "10m", at least "1m") |

`storage.max_size` counts cached artifacts and cached metadata documents (`cache_metadata`). When the total goes over the limit, the least recently served entries of either kind are evicted first; an evicted metadata document is fetched from upstream again on the next request for it. Pushed artifacts are not counted or evicted. An artifact replaced by a refetch stays in storage for at least an hour, or `storage.direct_serve_ttl` if longer, so requests already reading it can finish, and storage use can exceed the limit by what was replaced in that time.

### Serving artifacts without storing them

With `storage.cache_artifacts: false` the proxy streams every artifact download from upstream to the client and stores no artifacts. Metadata is still filtered and cached, so cooldown and the denylist apply as usual, and the database still holds the publish times cooldown needs. Use it when another caching layer, such as an Artifactory remote repository, sits in front of the proxy and caching artifacts twice only costs storage.

```yaml
storage:
  cache_artifacts: false
```

Every download is a fresh upstream fetch, and concurrent requests for the same artifact are not combined. Artifacts with a digest known up front (OCI blobs, Swift archives, Helm charts) are verified while streaming: the response is sent chunked, and on a mismatch the connection is aborted before the response completes so the client never receives a tampered artifact as a good one. The same happens when the upstream connection fails mid-download.

`cache_artifacts: false` cannot be combined with `scanning.enabled`, `storage.direct_serve`, `mirror_api` or `mirror.schedules`, which all need stored artifacts, and the `mirror` command refuses to run with it.

### Retention

Retention evicts cached artifacts that nobody has downloaded for a while. It works alongside `storage.max_size`: retention removes what has gone unused, and the size limit still applies to whatever is left.

The `ecosystems` and `packages` entries below only pass validation once the ecosystems they name support retention; see the table further down.

```yaml
storage:
  retention:
    default: "30d"
    ecosystems:
      npm: "14d"
      maven: "0"
    packages:
      "pkg:npm/lodash": "0"
    sweep_interval: "10m"
```

Each cached file ages on its own, counted from the last time the proxy served it, or from when it was fetched if it has not been served since. A newer release of the same package does not shorten the life of older versions. Durations take a `d` suffix for days, and `"0"` or an empty value means never evict by age. With no retention configured, artifacts stay until `max_size` evicts them. `sweep_interval` must be at least `1m`.

A package rule wins over an ecosystem rule, which wins over `default`. Package rules are keyed by package PURL without a version, written in one of the forms the table below lists for the ecosystem. The ecosystems and package rules can only name ecosystems that support retention in the running version; anything else, including a typo, stops the proxy at startup with an error naming the setting. `default` applies only to supported ecosystems, except `generic`, which requires an explicit rule. Support is added one ecosystem at a time, so a later release can make an existing `default` cover more ecosystems. On startup the proxy logs which ecosystems retention supports (`covered`) and which ones `default` does not reach (`default_not_applied`), including `generic`.

Generic retention is opt-in because release assets may disappear upstream and cannot then be downloaded again after eviction. An existing `default` never starts evicting generic downloads. Set `ecosystems.generic: "30d"` to cover all generic artifacts, or configure only selected GitHub repositories:

```yaml
storage:
  retention:
    default: "30d"  # Other supported ecosystems; never generic.
    packages:
      "pkg:generic/jqlang%2Fjq": "30d"
```

The package rule covers every release and asset in that repository, but each cached file ages separately. Other generic repositories remain exempt from age-based eviction. A package rule of `"0"` also exempts that repository when `ecosystems.generic` is enabled. The independent `storage.max_size` limit still applies.

| Ecosystem key | Retention supported | Package rule form |
|---------------|---------------------|-------------------|
| `alpine` | yes | `pkg:apk/curl` or `pkg:apk/alpine/curl`; case doesn't matter. A rule covers the package in every configured APK repository. Alpine mirrors keep only the current build of each package per release, so an evicted older one can't be fetched again. |
| `cargo` | yes | `pkg:cargo/serde`, the crate name as published; case, `-` and `_` have to match, so `pkg:cargo/inflector` doesn't cover `Inflector`. |
| `composer` | yes | `pkg:composer/symfony/console`; case doesn't matter. A key without a vendor is refused. |
| `conan` | yes | `pkg:conan/zlib`, the recipe name; a rule covers every version, revision and user/channel of it. |
| `conda` | yes | `pkg:conda/conda-forge/numpy`, with the channel as the namespace; the package name's case doesn't matter. A key without a channel is refused. |
| `cran` | yes | `pkg:cran/ggplot2`, the package name as published, case included (`pkg:cran/Rcpp`). A rule covers source and binary builds alike. |
| `deb` | yes | `pkg:deb/curl`; case doesn't matter. A vendor namespace such as `pkg:deb/debian/curl` is accepted and dropped, so a rule covers the package from every configured archive. Debian and Ubuntu archives only keep the current version per suite, so an evicted older one can't be fetched again. |
| `gem` | yes | `pkg:gem/rails`, the gem name as published, case included. |
| `generic` | yes, explicit rules only | `pkg:generic/owner%2Frepo` for a GitHub Release repository such as `jqlang/jq`. The ecosystem rule covers all cached version-pinned generic downloads, including GitHub Release assets and configured `generic_artifacts`. The global `default` never applies; metadata/API responses are not artifact retention candidates. |
| `golang` | yes | `pkg:golang/github.com/stretchr/testify`, the module path as in `go.mod`; case doesn't matter. A major version like `/v2` (or `.v3` on gopkg.in) is a module of its own and needs its own rule. The `!`-escaped form from proxy URLs and the module cache (`github.com/!azure/...`) is refused. |
| `helm` | yes | No package rules: charts are cached per repository and digest, not under the chart name. The `helm` rule and `default` cover charts from HTTP repositories; charts pulled over OCI fall under `oci`. |
| `hex` | yes | `pkg:hex/phoenix`, the package name as published. |
| `julia` | yes | No package rules: a julia PURL needs a `uuid` qualifier, which the cache doesn't store. The `julia` rule and `default` cover package tarballs, artifacts and registry snapshots alike. |
| `maven` | yes | `pkg:maven/com.google.guava/guava`, group and artifact as published, case included. Gradle build cache entries are not covered; they use `gradle.build_cache.max_age`. |
| `npm` | yes | `pkg:npm/lodash`, `pkg:npm/@babel/core`; the name has to match exactly, case included, so `pkg:npm/jsonstream` doesn't cover `JSONStream`. |
| `nuget` | yes | `pkg:nuget/Newtonsoft.Json`; case doesn't matter. |
| `oci` | yes | `pkg:oci/library/nginx`, the repository name of the pull. A named upstream keeps its prefix, also when containerd reaches it through `ns`: `ghcr.io/owner/app` pulled that way is `pkg:oci/upstream/ghcr/owner/app`. Homebrew bottles use Homebrew's image name, with `@` written as `/` and `+` as `x` (`openssl@3` is `pkg:oci/homebrew/core/openssl/3`). Helm charts pulled over OCI are stored like images. Only blobs are evicted; manifests stay in the metadata cache. |
| `pub` | yes | `pkg:pub/http`, the package name as published. |
| `pypi` | yes | `pkg:pypi/requests`; case and single `_`/`-` don't matter. Dots and repeated separators are kept as written, so a project like `zope.interface` can be stored as `zope.interface` and `zope-interface`; add a rule for each. |
| `rpm` | yes | `pkg:rpm/curl`, the name as packaged, case included (`pkg:rpm/NetworkManager`). A vendor namespace such as `pkg:rpm/fedora/curl` is accepted and dropped. Fedora and EPEL update repositories keep only the latest build of a package, so an evicted older one may not be fetchable again. |
| `swift` | yes | `pkg:swift/apple/swift-nio`, with the registry scope as the namespace, or the stored form `pkg:generic/swift-registry/apple.swift-nio`; case doesn't matter. A source repository key like `pkg:swift/github.com/apple/swift-nio` is refused, since it doesn't name a registry identity. |

An evicted artifact is fetched again from upstream on the next request. Packages preloaded with `proxy mirror` age from the time they were mirrored, so an unused mirrored package is evicted like any other. Running `proxy mirror` again over a package that is still cached counts as a download and restarts its clock, so packages a scheduled mirror job keeps listing never age out. A registry that removes old releases (Linux distribution archives in particular) cannot serve an evicted version again, so set those ecosystems' rules with that in mind.

Expired artifacts are not deleted right away. Their records are cleared and the files wait in the same queue a replaced artifact goes through, for at least an hour (or `storage.direct_serve_ttl` if longer), so downloads already in progress can finish. `storage.max_size` counts the space as free from the moment the record is cleared. Once that wait is over, each minute the queue is worked through for up to 30 seconds, pausing until the next minute after a delete fails. One sweep clears at most 10,000 artifacts, so the first sweep over a large, old cache spreads its evictions, and the downloads that bring popular artifacts back, over several intervals.

With several proxies sharing one database and `database.hit_flush_interval` set, a proxy's sweep cannot see downloads another proxy has not written yet. Keep retention durations well above the flush interval.

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

### Hit counts

Every cache hit updates the artifact's hit count and last-access time, which the stats pages and LRU eviction use. Rather than writing each hit as its own transaction, the proxy counts hits in memory and writes them together every `hit_flush_interval`. SQLite allows one writer at a time, and the proxy uses a single SQLite connection, so with a write per hit, concurrent downloads queue behind each other.

```yaml
database:
  hit_flush_interval: "1s"
```

| Config | Environment | Flag | Description |
|--------|-------------|------|-------------|
| `database.hit_flush_interval` | `PROXY_DATABASE_HIT_FLUSH_INTERVAL` | - | How often batched hits are written (default `1s`). `0` writes each hit as it happens. |

Hits not yet written are lost if the process is killed; a normal shutdown writes them.

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

Each client request record carries `remote_addr` (the TCP peer, host and port), `remote_ip` (the address the request is attributed to), `user_agent`, `client` (the tool name derived from the User-Agent), `ecosystem`, and `bytes` (the response body size written to the client). The structured log carries the same `client`, `remote` and `remote_ip` fields.

`remote_ip` follows the top-level `trust_forwarded_for` setting. Enable that only when the proxy sits behind a load balancer or ingress that sets the header: any client can send `X-Forwarded-For`, so behind such a hop it is the only way to see the real caller, but in front of one it lets a caller choose what address it is logged as. It can also choose a fresh one per request, which fills the bounded source table on `/ui/analytics` and evicts the genuine callers from it. `remote_addr` is unaffected and always records the TCP peer.

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
| `upstream.go_sumdbs_offline` | `PROXY_UPSTREAM_GO_SUMDBS_OFFLINE` | `false` |
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
| `upstream.chef` | `PROXY_UPSTREAM_CHEF` | `https://supermarket.chef.io` |
| `upstream.conda` | `PROXY_UPSTREAM_CONDA` | `https://conda.anaconda.org` |
| `upstream.cran` | `PROXY_UPSTREAM_CRAN` | `https://cloud.r-project.org` |
| `upstream.julia` | `PROXY_UPSTREAM_JULIA` | `https://pkg.julialang.org` |
| `upstream.swift` | `PROXY_UPSTREAM_SWIFT` | `https://tuist.dev/api/registry/swift` |
| `upstream.oci_default` | `PROXY_UPSTREAM_OCI_DEFAULT` | `https://registry-1.docker.io` |
| `upstream.debian` | `PROXY_UPSTREAM_DEBIAN` | `http://deb.debian.org/debian` |
| `upstream.rpm` | `PROXY_UPSTREAM_RPM` | `https://dl.fedoraproject.org/pub/fedora/linux` |
| `upstream.homebrew_api` | `PROXY_UPSTREAM_HOMEBREW_API` | `https://formulae.brew.sh/api` |
| `upstream.homebrew_artifact` | `PROXY_UPSTREAM_HOMEBREW_ARTIFACT` | `https://ghcr.io` |

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

`upstream.chef` is the Supermarket served at `/chef/`, for Berkshelf (`source "https://proxy.example.com/chef"`) and Policyfiles (`default_source :supermarket, "https://proxy.example.com/chef"`). The upstream has to serve the Supermarket API: `/universe`, `/api/v1/cookbooks/{name}`, its versions and their downloads. A private Supermarket that needs credentials takes them from `upstream.auth`, scoped to its URL. A download redirect to another host, such as the storage bucket behind Supermarket, does not receive them:

```yaml
upstream:
  chef: "https://supermarket.example.com"
  auth:
    "https://supermarket.example.com":
      type: bearer
      token: "${SUPERMARKET_TOKEN}"
```

### Private package routes

Clients that use the proxy as their only registry can still install private packages. A package route sends every package whose name matches a pattern to a private registry, and never to the default upstream:

```yaml
upstream:
  composer_routes:
    "example/*": "https://packages.example.com/composer"
  npm_routes:
    "@example/*": "https://packages.example.com/npm"
  allow_private_hosts:
    - "packages.example.com"
  auth:
    "https://packages.example.com":
      type: bearer
      token: "${PRIVATE_REGISTRY_TOKEN}"
```

- Patterns use Go's [`path.Match`](https://pkg.go.dev/path#Match) syntax and are matched case-insensitively. `*` does not cross `/`, so `example/*` matches `example/library` but not `example-fork/library`. The longest matching pattern wins.
- A Composer route URL is the repository base that serves `/p2/{vendor}/{package}.json`. A route also covers the package's dev versions in `/p2/{vendor}/{package}~dev.json`; patterns are matched against the package name without `~dev`.
- An npm route URL is the registry base that serves `/{package}`. Tarball URLs in its packuments must be on the same host and below the same path.
- A routed package is not looked up anywhere else. If the private registry does not have it, the client gets a 404. If the registry fails or rejects the credentials, the client gets a 502. A package with the same name on the public registry is never served in its place (dependency confusion).
- Metadata and artifacts of routed packages are cached under keys that include the route's URL. Entries cached from the public registry before a route was added are never served for a routed package.
- With cooldown enabled, the publish time of a routed npm version always comes from the route's metadata. Times recorded for the public package of the same name, or for a route's previous URL, are not used, and routed downloads do not record times of their own.
- Composer metadata of routed packages carries an empty `notification-url`, so Composer does not report their installs to the default upstream's `notify-batch` endpoint.
- Use `upstream.auth` for the registry's credentials. The proxy has no authentication of its own, so every client that can reach it can download routed packages.
- Search (`/composer/search.json`) and the package list (`/composer/packages/list.json`) still come from the default upstream only.
- npm audit and signing-key requests still go to `upstream.npm`. An audit request lists every package in the dependency tree, routed ones included.
- Routes can only be set in the configuration file, not with environment variables.

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

HTTP indexes can also contain `oci://` chart references. References matching the
host and port of a configured `upstream.oci` registry or `upstream.oci_default`
are rewritten through the OCI proxy, with named registries preferred. Unmatched
references are left unchanged, so Helm contacts those registries directly.
Automatic rewriting requires both the OCI upstream URL and `base_url` to be
root URLs without a path prefix. Repository paths, tags, and digests are preserved.

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
down.

Upstreams pointed at these hosts get version-pinned paths built in, whatever
the upstream is named:

| Upstream URL | Cached as artifacts | Package name |
|---|---|---|
| `https://archive.apache.org` | `dist/{project dirs}/{version}/[binaries/]{file}`, where `{version}` starts with a digit | the project dirs, e.g. `maven/maven-3`, `maven/mvnd` |
| `https://nodejs.org` | `dist/v{version}/{file}` | the upstream name |
| `https://dl.google.com/go` | `go{version}.{os}-{arch}.tar.gz`, `.zip` and their `.sha256` | the upstream name |

Other upstreams can declare their own version-pinned paths in
`generic_artifacts`. These are tried before any built-in ones, so a configured
pattern that overlaps a built-in decides the package name. Each pattern is a regular expression that must match the
whole path after `/generic/{name}/`, with `version` and `file` named groups
(single path segments) and an optional `name` group for the package name,
which otherwise is the upstream name. Matching downloads are cached like
release assets and listed under the `generic` ecosystem:

```yaml
upstream:
  generic:
    eclipse: "https://download.eclipse.org"
  generic_artifacts:
    eclipse:
      - 'jdtls/milestones/(?P<version>[^/]+)/(?P<file>[^/]+)'
```

Only list paths whose content never changes for a given version: a match is
never revalidated.

Every other path is served through the metadata cache (`cache_metadata`
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

containerd mirror requests carry the original registry host in an `ns` query
parameter (see the containerd section in the README). The proxy only looks the
host up and never connects to it: `docker.io`, `index.docker.io`,
`registry-1.docker.io` and the host of `upstream.oci_default` select the default
registry, and the host of each `upstream.oci` URL selects that named registry.
Hosts are compared case-insensitively. The scheme's default port (443 for
`https`, 80 for `http`) may be spelled out or left out in the image reference;
any other port must match exactly, so `https://registry.example:80` and
`https://registry.example` are two different registries. An
unknown host returns `404 NAME_UNKNOWN`, so containerd falls back to its next
host. Registry URLs with a path (for example an Artifactory repository path)
are not reachable through their own host in `ns`, only through
`upstream/{name}/` or through `upstream.oci_mirrors`. When two
entries share a host, the proxy logs a warning at startup; the default registry
wins, otherwise the alphabetically first name. Pulls through `ns`,
`upstream/{name}/` and unprefixed requests share the same cache entries.
Requests that combine the `upstream/{name}/` prefix with `ns`, as per-registry
containerd mirrors with `override_path = true` send them, are routed by the
prefix. They are accepted only when `ns` is the upstream's own host, a host
listed for it in `upstream.oci_mirrors`, or Docker Hub (`docker.io` and its
aliases). Docker Hub repository names have two path components and can never
start with `upstream/`, so a Docker Hub mirror behind any prefix stays
reachable. Any other host gets `404 NAME_UNKNOWN`: a containerd `_default`
mirror sends the same request for an image such as
`unconfigured.example/upstream/ghcr/owner/app`, and that pull must not be
answered from the `ghcr` upstream.

When a named upstream mirrors another registry, for example an Artifactory
remote of `ghcr.io`, say so in `upstream.oci_mirrors`:

```yaml
upstream:
  oci:
    ghcr: "https://artifactory.example.com/artifactory/api/docker/ghcr-remote"
  oci_mirrors:
    ghcr: ["ghcr.io"]
```

Each entry lists bare registry hosts as containerd sends them, optionally with
a port. A host can be listed for one upstream only. Those hosts select the
upstream for unprefixed `ns` requests and are accepted as `ns` on its
`upstream/{name}/` prefix. A host that is also the host of a configured
registry URL stays with that registry for unprefixed requests; the proxy logs a
warning at startup.

A host that is accepted on a prefix route gives up image paths of the form
`<host>/upstream/{name}/...` under a `_default` mirror: with `ghcr: ["ghcr.io"]`,
a pull of `ghcr.io/upstream/ghcr/owner/app` is answered with `owner/app` from
the `ghcr` upstream, because containerd sends the same request for it as a
per-registry mirror does. The same applies to the upstream's own host.
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

```yaml
upstream:

  # Named Arch Linux pacman mirrors, served at /arch/{name}/.
  arch:
    archlinux: "https://geo.mirror.pkgbuild.com"
```

Arch Linux repositories are read-only. Requests to `/arch/{name}/…` are passed
to the mirror unchanged, so both the Arch Linux layout
(`/arch/archlinux/core/os/x86_64/core.db`) and the Arch Linux ARM layout
(`/arch/archlinuxarm/aarch64/core/core.db`) work. Databases (`{repo}.db`,
`{repo}.files` and their `.tar.*` forms) are cached with the metadata TTL and
served byte-for-byte unchanged; a database's `.sig` is refreshed together with
the database. Packages and package signatures use the shared artifact cache.
When `upstream.arch` is empty, a single repository named `archlinux` pointing at
`https://geo.mirror.pkgbuild.com` is available; configuring any entry replaces
that default. Arch Linux ARM has no default: pick a regional HTTPS mirror, not
the `mirror.archlinuxarm.org` redirector.

### Go checksum database

```yaml
upstream:

  # Named Go checksum databases, served at /go/sumdb/{name}/.
  go_sumdbs:
    sum.golang.org: "https://sum.golang.org"

  # Keep answering for a database from its cached tree, however old, when
  # the database cannot be reached. For networks where clients have no
  # route to the database of their own. Requires cache_metadata.
  go_sumdbs_offline: false
```

The `go` command asks `GOPROXY` for the checksum database before contacting it
directly. The proxy answers for every name listed here and fetches lookups and
tiles from the database's URL, not from `upstream.go`. Unknown names get 404,
so the `go` command falls back to contacting that database itself. When
`upstream.go_sumdbs` is empty, `sum.golang.org` is served; configuring any
entry replaces that default.

Once the proxy says it serves a database, the `go` command no longer contacts
that database directly. The proxy therefore only says so when it can fetch the
database's latest signed tree, or has a fresh one cached (fetched within
`metadata_ttl`). If it cannot, it answers 404 as for an unknown name, and
clients go to the database themselves, as they would without the proxy. The
check waits five seconds at most, and after a failed check the proxy waits a
minute before it tries the database again.

Set `upstream.go_sumdbs_offline: true` when clients have no route to the
database of their own. The proxy then keeps answering for it from the cached
tree for as long as the outage lasts and serves cached lookups and tiles.
That does not mean every cached module still verifies: the `go` command
checks each lookup against tiles of the signed tree that came with it, the
database attaches a different tree to almost every lookup, and a tile no
client fetched through the proxy before cannot be served during the outage.
Lookups for modules that were never cached fail as well. Builds whose
`go.sum` already lists every module do not ask the database at all and are
not affected. This requires `cache_metadata`.

With `cache_metadata` enabled, lookups, partial hash tiles and the latest
signed tree follow `metadata_ttl` and are served from cache when the database
is unreachable. Complete hash tiles never change and are not revalidated once
cached; a tile of the wrong size is refused and not stored. Data tiles are
always forwarded and never stored: the `go` command does not fetch them, only
mirrors and auditors do.
Without `cache_metadata` the requests are forwarded and nothing is stored.

A 404 from the database keeps its text, which the `go` command prints to
explain why a version is missing. With `cache_metadata` enabled the text is
cut at 4 KiB; without it the response is passed on unchanged.

If `upstream.go` points at a module proxy that serves the checksum database
itself, such as Athens, point the entry there instead, for example
`sum.golang.org: "https://athens.example/sumdb/sum.golang.org"`.

### Debian archives

```yaml
upstream:

  # The main APT archive, served at /debian/.
  debian: "http://deb.debian.org/debian"

  # Additional APT archives, served at /debian/{name}/.
  debian_repositories:
    security: "https://security.debian.org/debian-security"
```

A Debian release is served by more than one archive: security updates live on
a separate host from the main archive, so `upstream.debian` alone cannot serve
a complete suite set. Requests to `/debian/{name}/…` mirror the upstream
layout, e.g. `/debian/security/dists/trixie-security/InRelease`.

`upstream.debian_repositories` is additive. `/debian/pool/…` and
`/debian/dists/…` continue to address `upstream.debian` with unchanged cache
identities, so existing deployments and their warm caches are unaffected. A
repository name shadows the main archive's root path of the same name; `pool`
and `dists` are refused at config load for that reason, and other root paths
an archive may serve (`indices`, `project`, `doc`, `tools`) are not, so avoid
those names unless the shadowing is intended.

This field has no environment variable, as with the other named upstream maps.
`PROXY_UPSTREAM_DEBIAN` still sets `upstream.debian`.

### Maven repositories

```yaml
upstream:

  # The default repository, served at /maven/.
  maven: "https://repo1.maven.org/maven2"

  # Additional Maven repositories, served at /maven/{name}/.
  maven_repositories:
    grails: "https://repo.grails.org/grails/restricted"
```

Builds often need more than one Maven repository. Each entry in
`upstream.maven_repositories` is served at `/maven/{name}/` with the upstream's
layout, e.g. `/maven/grails/org/grails/grails-core/6.0.0/grails-core-6.0.0.pom`,
and is fetched from that upstream alone: unlike the default repository, a named
one doesn't fall back to the Gradle Plugin Portal. Files are cached per
repository, so the same coordinates in two repositories never share a cache
entry.

`upstream.maven_repositories` is additive. `/maven/…` keeps serving
`upstream.maven` with unchanged cache identities. A repository name is matched
before the default repository, so it hides every group ID whose first segment
is the same name there: a repository named `tools` would take
`/maven/tools/jackson/…` away from Maven Central. At config load the proxy
refuses names of two characters or less, common top-level domains such as `com`
and `io`, and every first segment of a group ID on Maven Central, the Gradle
Plugin Portal and Google's Maven repository (about 1,200 names, such as `javax`,
`junit`, `software` or `gradle`). Names are compared case-sensitively, as paths
are, and may only use letters, digits, `.`, `_` and `-`. Other repositories can
have group IDs the list doesn't know, so pick names that can't be the start of
a group ID, such as `grails` or `company-releases`.

A repository's files are cached under its name and a hash of its upstream URL,
so pointing a repository at a different URL stops serving what the old one
sent.

This field has no environment variable, as with the other named upstream maps.
`PROXY_UPSTREAM_MAVEN` still sets `upstream.maven`.

## Authentication

Configure authentication for private upstream registries. The same authentication-aware client is used for metadata and artifact downloads, and credentials can reference environment variables using `${VAR_NAME}` syntax.

OCI registries that return a Bearer challenge from a `/v2/{repository}/…` endpoint are handled automatically. The proxy discovers the token realm from `WWW-Authenticate`, applies any configured credentials for the token URL, and reuses the scoped token until shortly before it expires.

A token fetched with credentials configured for the token URL is only fetched for, and sent to, a registry that is trusted with it: one with configured credentials of its own, or one on the same site (registrable domain) as the token service, as `registry-1.docker.io` is with `auth.docker.io`. Any other host that answers with a challenge naming that token service gets its 401 back, so it cannot obtain a token minted from your credentials. Tokens from a token service with no configured credentials are anonymous and are fetched for any registry, as before. If your registry and its token service are on different sites, configure credentials for the registry URL as well.

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
    read_only: true
    max_upload_size: "100MB"
    max_age: "168h"
    max_size: "20GB"
    sweep_interval: "10m"
```

| Config | Environment | Description |
|--------|-------------|-------------|
| `gradle.build_cache.read_only` | `PROXY_GRADLE_BUILD_CACHE_READ_ONLY` | Reject PUT uploads and serve GET/HEAD only (default `true`; the variable takes `true`/`false` or `1`/`0`, any other value keeps uploads off and logs a warning) |
| `gradle.build_cache.max_upload_size` | `PROXY_GRADLE_BUILD_CACHE_MAX_UPLOAD_SIZE` | Maximum accepted PUT body size (must be > 0) |
| `gradle.build_cache.max_age` | `PROXY_GRADLE_BUILD_CACHE_MAX_AGE` | Delete entries older than this duration (default `168h`, also accepts days such as `7d`; set `0` to disable) |
| `gradle.build_cache.max_size` | `PROXY_GRADLE_BUILD_CACHE_MAX_SIZE` | Total size cap for `_gradle/http-build-cache`, deleting oldest first (`0` disables) |
| `gradle.build_cache.sweep_interval` | `PROXY_GRADLE_BUILD_CACHE_SWEEP_INTERVAL` | Frequency for background eviction sweeps |

Uploads are off by default, so a client that can reach the proxy cannot overwrite cache entries that end up in other people's builds. Set `read_only: false` (or `PROXY_GRADLE_BUILD_CACHE_READ_ONLY=false`) on the instance your CI jobs upload to, and let only CI push (`isPush = System.getenv("CI") != null`). Before this default changed, uploads were accepted without configuration; setups that upload need the setting now.

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
  package_patterns:
    "pkg:npm/@example/*": "0"
```

| Config | Environment | Description |
|--------|-------------|-------------|
| `cooldown.default` | `PROXY_COOLDOWN_DEFAULT` | Global default cooldown |
| `cooldown.ecosystems` | - | Per-ecosystem overrides |
| `cooldown.packages` | - | Per-package overrides (keyed by PURL) |
| `cooldown.package_patterns` | - | Per-package glob overrides (keyed by PURL glob) |

Durations support days (`7d`), hours (`48h`), and minutes (`30m`). Set to `0` to disable.

Package PURL keys are normalized to canonical form before matching, so `pkg:npm/@babel/core` and `pkg:npm/%40babel/core` are equivalent, as are `pkg:pypi/Django` and `pkg:pypi/django`. If both forms configure the same package, the canonical entry wins.

`package_patterns` matches canonical, versionless PURLs using `*` for zero or more characters and `?` for one character. Neither wildcard crosses `/` separators. Character classes (`[...]`) and backslash escapes are rejected at startup. For example, `"pkg:npm/@example/*"` matches packages under the `@example` npm scope. Patterns accept `@` as an alias for `%40`; other characters must use their canonical PURL form. Equivalent patterns with different durations are rejected at startup. Equal durations, such as `1d` and `24h`, are accepted.

Exact `packages` entries take precedence over patterns. When several patterns match, longer patterns win after excluding `*` and `?` from the length. Ties use lexical order of the normalized patterns.

Resolution order: exact package override, then package pattern, then ecosystem override, then global default. This lets you set a conservative default while exempting trusted package families.

Currently supported for npm, PyPI, pub.dev, Composer, Cargo, NuGet, Conda, RubyGems, and Hex. These ecosystems include publish timestamps in their metadata.

Note: Hex cooldown requires disabling registry signature verification since the proxy re-encodes the protobuf payload without the original signature. Set `HEX_NO_VERIFY_REPO_ORIGIN=1` or configure your repo with `no_verify: true`.

## Version denylist

Block exact package versions independently of cooldown and artifact scanning:

```yaml
denylist:
  packages:
    - "pkg:pypi/requests@2.31.0"
    - "pkg:cargo/some-crate@1.2.3"
    - "pkg:npm/%40scope/example@4.5.6"
```

Entries must be versioned PURLs without qualifiers or subpaths. Package names
are canonicalized (including PyPI case/separator normalization and npm scopes);
versions match exactly, not by range or glob. Invalid entries prevent startup.
An empty list disables the policy. Restart the proxy after changing the list.

Denied versions are removed from npm packuments, Cargo sparse indexes, and PyPI
simple HTML/JSON and release metadata, including responses served from cached
metadata during an upstream outage. No publication timestamp or enabled cooldown
is required. npm tags pointing at denied versions are removed; `latest` is moved
to the highest remaining stable version when available. Other tags are not
retargeted. PyPI version-specific JSON endpoints return 404 for denied versions.

The shared artifact download pipeline returns 403 for denied package/version
identities before reading storage, issuing a signed redirect, or fetching bytes.
This also applies to mirror downloads and recognized Debian package downloads.
Existing cached artifacts are retained, so removing a denial restores access.
Already-issued storage URLs and clients' local caches cannot be revoked by this
configuration.

Metadata filtering currently covers npm, PyPI, and Cargo. Other ecosystems'
metadata is unchanged; in particular, APT indexes cannot be rewritten without
regenerating their checksums and signatures. Pass-through resources without a
resolved package/version identity are not covered. This is an explicit operator
policy, not automatic CVE detection or cache re-scanning. Exact pins or dependency
constraints may still make an install fail when no allowed version can satisfy it.

### APT version pinning

For Debian and Ubuntu clients, combine the proxy's download denylist with
client-side APT preferences. The proxy blocks recognized denied `.deb` downloads
with 403, but does not remove versions from APT indexes. Without a client-side
pin, APT can select a denied version and then fail when downloading it.

On each client, add a record to `/etc/apt/preferences.d/proxy-denylist.pref`:

```text
Package: somepkg
Pin: version 1.2.3-1
Pin-Priority: -1
```

Replace `somepkg` with the binary package name and `1.2.3-1` with its full APT
version, including the epoch if present. A negative priority excludes that
version from normal candidate selection. For multi-architecture clients, use
`Package: somepkg:any` to cover all architectures. Separate additional records
with blank lines. See [apt_preferences(5)](https://manpages.debian.org/stable/apt/apt_preferences.5.en.html).

Check the effective priorities and simulate installation before deploying the
preferences broadly:

```bash
apt-cache policy somepkg
apt-get --simulate install somepkg
```

Confirm that the denied version has priority `-1` and is not selected. Existing
specific pins can take precedence, so check for conflicts. APT can select
another candidate only if one is available and satisfies dependency constraints;
a Debian suite commonly offers only one version. Pinning does not remove an
already-installed package or automatically downgrade it.

These preferences are managed on clients, not distributed by the proxy. Keep
the corresponding proxy denylist entries as a separate download guard.

APT indexes are left unchanged to preserve upstream authentication: `Release`
metadata contains index checksums and is signed by the repository. Filtering
would require regenerating metadata and signing it with a key clients trust,
making the proxy a repository authority rather than a transparent cache. Do not
disable signature verification to work around this limitation. See
[apt-secure(8)](https://manpages.debian.org/stable/apt/apt-secure.8.en.html).

If centrally curated APT indexes are required, use a repository manager such as
[aptly](https://www.aptly.info/) or [reprepro](https://salsa.debian.org/debian/reprepro)
as the upstream behind the proxy. Filtered APT repository generation and signing
are outside the proxy's current scope.

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

Because scanners pull the staged bytes from the running server, the `proxy mirror` command cannot scan what it fetches. While scanning is enabled it refuses every package and container image instead of caching them unscanned. Mirror through the `/api/mirror` endpoints of the running server, whose jobs are scanned like any other download.

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

Composer archive downloads look up their upstream URL in the same cached metadata, so a cached archive is served without asking Packagist again and stays installable while it is down. Only a version or file the cached metadata does not list yet is looked up upstream.

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

Upstream metadata responses are buffered in memory before being rewritten and served. `metadata_max_size` caps that buffer to protect against OOM from a misbehaving upstream. Some npm packages with thousands of versions (for example `renovate`) exceed the 100 MB default, so raise this if you see `metadata response exceeds size limit` in the logs. Conda `repodata.json` with cooldown enabled is filtered as it streams and isn't held in memory, so this limit doesn't apply to it.

```yaml
metadata_max_size: "100MB"   # default
```

Or via environment variable: `PROXY_METADATA_MAX_SIZE=250MB`.

### Artifact size limit

Artifacts have no size limit by default. `artifact_max_size` refuses any artifact larger than the limit, so a client cannot fill storage or tie up the proxy by requesting huge downloads. An artifact whose upstream `Content-Length` is over the limit is refused before its body is read; one without a declared size fails once the download passes the limit. Either way nothing is cached and the client gets `502 Bad Gateway`, except when artifacts are streamed: one without a declared size has already started sending when it passes the limit, so the client sees the download cut off. The limit applies to `serve`, `mirror` and streamed artifacts.

```yaml
artifact_max_size: "2GB"   # default: unlimited ("" or "0")
```

Or via environment variable: `PROXY_ARTIFACT_MAX_SIZE=2GB`.

### Rewritten metadata cache

The npm and Composer handlers rewrite every metadata document they serve, so that download URLs point at the proxy. That means decoding the whole document and encoding it again, and Composer's minified documents are also expanded, which together cost milliseconds per typical package and far more for very large ones. The proxy keeps rewritten documents in memory, so each distinct upstream document is rewritten once and requests arriving while it is being rewritten wait for that rewrite. New bytes from upstream are rewritten again. With version cooldown enabled the cache is bypassed, because cooldown filtering depends on the current time.

Each cached document is also kept gzip-compressed, when that makes it smaller, and served with `Content-Encoding: gzip` to clients that accept it. A large packument is compressed once rather than on every request, by the proxy or by a reverse proxy in front of it. The compressed copy counts against the cache size and typically adds about an eighth to each entry. Documents rewritten outside the cache are served uncompressed.

```yaml
metadata_rewrite_cache_size: "256MB"   # default; "0" rewrites on every request
```

Or via environment variable: `PROXY_METADATA_REWRITE_CACHE_SIZE=1GB`.

## Upstream HTTP timeout

Protocol handlers use a shared HTTP client for upstream requests such as metadata fetches and pass-through file downloads. `http_timeout` sets that client's per-request timeout. Raise it if slow upstreams or large metadata responses cause `context deadline exceeded` errors.

```yaml
http_timeout: "30s"   # default
```

Or via environment variable: `PROXY_HTTP_TIMEOUT=2m`.

Set to `"0"` to disable the timeout entirely (requests then rely only on the server's write timeout). Independently of this setting, the shared transport gives up on an upstream that has not sent response headers within 60 seconds.

## Hosted Packages

Operators can push a package archive into the proxy under a name and version. The proxy serves it in place of the upstream archive with the same version, or in addition to the upstream's versions. Use it to ship a patched build of a dependency without running a separate registry.

The `/api/hosted` endpoints are disabled by default. Because a pushed version replaces the upstream one for every client, enabling them requires a token:

```yaml
hosted:
  enabled: true
  token: "${PROXY_HOSTED_TOKEN}"
  max_upload_size: "100MB"
  npm_scopes: ["@acme"]
  composer_vendors: ["acme"]
```

| Config | Environment | Description |
|--------|-------------|-------------|
| `hosted.enabled` | `PROXY_HOSTED_ENABLED` | Register the `/api/hosted` endpoints (default `false`) |
| `hosted.token` | `PROXY_HOSTED_TOKEN` | Bearer token required to push and delete; required when enabled, supports `${VAR}` |
| `hosted.max_upload_size` | `PROXY_HOSTED_MAX_UPLOAD_SIZE` | Maximum size of one pushed archive (default `100MB`) |
| `hosted.npm_scopes` | `PROXY_HOSTED_NPM_SCOPES` | npm scopes served from pushed packages only (comma-separated in the environment) |
| `hosted.composer_vendors` | `PROXY_HOSTED_COMPOSER_VENDORS` | Composer vendors served from pushed packages only |

`hosted.enabled` cannot be combined with `storage.cache_artifacts: false`.

### Pushing a patched package

1. Build the archive with the original package name and the version you push in its manifest (`package.json` or `composer.json`), e.g. `request-2.88.3.tgz`.
2. Push it:

   ```bash
   export PROXY_HOSTED_TOKEN=...
   proxy hosted push -url https://proxy.example.com npm request 2.88.3 ./request-2.88.3.tgz
   ```

   or with curl:

   ```bash
   curl -X PUT --data-binary @request-2.88.3.tgz \
     -H "Authorization: Bearer $PROXY_HOSTED_TOKEN" \
     https://proxy.example.com/api/hosted/npm/request/2.88.3
   ```

3. Install it as usual, e.g. `npm install request@2.88.3`. The version is listed in the package metadata, so ranges like `^2.88.0` resolve to it too.
4. Check what is pushed: `proxy hosted list npm request` or `GET /api/hosted/npm/request`.
5. Remove it again: `proxy hosted delete npm request 2.88.3` or `DELETE /api/hosted/npm/request/2.88.3`. If the upstream has the version, clients get the upstream archive again.

### Supported ecosystems

Download handlers look archives up by the filename clients request, so a push is stored under that name:

| Ecosystem | Name | Filename |
|-----------|------|----------|
| `npm` | `{name}` or `@{scope}/{name}` | `{name}-{version}.tgz`, without the scope |
| `composer` | `{vendor}/{package}` | `{package}-{version}.zip` |

Other ecosystems are not supported yet.

The archive must contain the package manifest: `package.json` in the top directory of an npm tarball, or `composer.json` at the root or in the single top directory of a Composer zip. Its name, and its version when it has one, must match the push. Otherwise the push is refused with 400.

### Package metadata

Pushed versions are listed in the package metadata clients resolve against, with the dependencies from their manifest:

- A pushed version takes the place of an upstream version with the same number.
- Pushed versions are added next to the upstream ones and are never held back by cooldown. npm `dist-tags` stay as the upstream has them.
- A package the upstream doesn't know is served from its pushed versions alone. Its npm `latest` tag is the highest pushed version.
- Pushed Composer dev versions (`dev-*`, `*-dev`) are listed in the `~dev` metadata file.
- npm entries carry the archive's sha512 as `dist.integrity`. Composer entries carry its sha256 as `dist.reference` and leave `shasum` empty.

Metadata of packages with pushed versions is rewritten on every request instead of being cached, so a push or delete shows up on the next request.

### Hosted-only namespaces

A package the upstream doesn't know is served from its pushed versions, but the proxy still asks the upstream about it on every request. If someone later publishes the same name there, their versions are listed next to yours, and a range such as `^1.0.0` can resolve to one of them (dependency confusion).

For private packages:

1. Prefer a namespace you own on the public registry: an npm scope registered on npmjs.com, or a Composer vendor with packages on Packagist. Nobody else can publish under it.
2. List the namespace in `hosted.npm_scopes` or `hosted.composer_vendors`. Its packages are served from pushed versions alone: the upstream is never asked about them, and anything not pushed answers 404.

Other packages, including ones in other scopes and vendors, are proxied as usual.

### Storage and eviction

Pushed archives are stored like cached ones but marked as local. The cache size limit and storage retention never remove them; only a delete does. Scanning, when enabled, checks pushed archives like fetched ones.

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

`platforms` limits which platforms of container images are mirrored, like the command's `--platform` flag:

```bash
curl -X POST http://localhost:8080/api/mirror \
  -H "Content-Type: application/json" \
  -d '{"purls":["pkg:docker/library/alpine@3.20"],"platforms":["linux/amd64"]}'
```

## Cache Delete API

Deleting cached artifacts is disabled by default. Enable it to register `DELETE /api/cache` and show Delete buttons on the version page of the web UI:

```yaml
cache_delete: true
```

Or via environment variable: `PROXY_CACHE_DELETE=true`.

The endpoint has no authentication. Before enabling it, restrict `/api/cache` and `/ui` to trusted users at your ingress or reverse proxy. The UI sends its deletes to `/ui/api/cache`, so whatever protects `/ui` protects those too.

Delete every cached file of a version, or only the one named by `filename`:

```bash
curl -X DELETE http://localhost:8080/api/cache/maven/com.google.guava:guava/32.1.3-jre
curl -X DELETE 'http://localhost:8080/api/cache/maven/com.google.guava:guava/32.1.3-jre?filename=guava-32.1.3-jre.jar'
```

The name is the package name the UI shows, for example `group:artifact` for Maven. The response lists the deleted files. The next request for a deleted file fetches it from the upstream again. The stored bytes are removed after the same delay as a replaced artifact, so downloads already in progress can finish.

Pushed archives are not touched: a whole-version delete skips them, and naming one answers 409. Delete those through the [hosted API](#hosted-packages).

## Mirror Command

The `proxy mirror` command pre-populates the cache from various sources. It accepts the same storage and database flags as `serve`.

| Flag | Default | Description |
|------|---------|-------------|
| `--sbom` | | Path to CycloneDX or SPDX SBOM file |
| `--concurrency` | `4` | Number of parallel downloads |
| `--dry-run` | `false` | Show what would be mirrored without downloading |
| `--platform` | | Comma-separated `os/arch[/variant]` list limiting which platforms of container images are mirrored |
| `--config` | | Path to configuration file |
| `--storage-url` | | Storage URL |
| `--database-driver` | | Database driver |
| `--database-path` | | SQLite database file |
| `--database-url` | | PostgreSQL connection URL |

Positional arguments are treated as PURLs:

```bash
proxy mirror pkg:npm/lodash@4.17.21 pkg:cargo/serde@1.0.0
```

### Container images

`pkg:docker` and `pkg:oci` PURLs mirror a container image the way a pull does: the manifest or index, the platform manifests, then config and layer blobs. Everything is stored under the keys a pull through `/v2` reads, so a later `docker pull` or containerd pull is served from cache, also when the upstream is unreachable.

```bash
proxy mirror --platform linux/amd64,linux/arm64 pkg:docker/library/alpine@3.20
proxy mirror 'pkg:docker/containerd/alpine@3.14.0?repository_url=ghcr.io'
proxy mirror 'pkg:oci/debian@sha256:...?repository_url=docker.io/library/debian&tag=bookworm'
```

- The version is a tag or a digest. Unversioned image PURLs fail; mirroring all tags is not supported.
- Without `repository_url` the name is routed like a pull without containerd's `ns` parameter: the default OCI registry, repository prefix routes and `upstream/{name}/`. Single-segment Docker Hub names get the `library/` prefix.
- `repository_url` is resolved like containerd's `ns` parameter, so it has to name the default registry, Docker Hub, a configured `upstream.oci` host or a host listed in `upstream.oci_mirrors`. Other hosts are refused.
- `pkg:oci` needs `repository_url`. With `tag=` the tag is mirrored as well; if it now points to another digest, both are mirrored and a warning is logged. `arch=` limits the platforms when `--platform` is not given.
- Without `--platform` every platform of an index is mirrored, including Windows images, which can be large. Attestation manifests of the selected platforms are mirrored too, because Docker fetches them on every pull.
- Non-distributable (foreign) layers are skipped.
- While `scanning.enabled` is set, the command refuses images, because scanners fetch staged layers from the running server. Use the mirror API instead.

### Scheduled refresh

Tags such as `alpine:3.20` move. The server can re-mirror them on its own, so the cache follows the tag even when nobody pulls it or the proxy is offline later:

```yaml
mirror:
  schedules:
    - images: [pkg:docker/library/alpine@3.20, pkg:docker/library/nginx@stable]
      interval: 6h
      platforms: [linux/amd64]
```

| Setting | Env | Description |
|---------|-----|-------------|
| `mirror.schedules[].images` | - | `pkg:docker` or `pkg:oci` PURLs with a tag, resolved like the `mirror` command |
| `mirror.schedules[].interval` | - | How often the images are mirrored, at least `1m` |
| `mirror.schedules[].platforms` | - | `os/arch[/variant]` entries limiting the platforms; empty mirrors all |

Each run checks the tag with a HEAD request. If the tag still points to the cached digest, nothing else is fetched. If it moved, the new manifests and blobs are fetched first and the tag is switched only when all of them are cached, so a failed run leaves the previous image pullable. Images of one schedule are mirrored one after the other.

- The first run starts within ten minutes of startup, later runs every `interval` ±10 %, so restarts and several instances don't hit the registry at once. Each instance runs the schedules on its own.
- Digest-pinned PURLs never move and are rejected. A `pkg:oci` digest with `tag=` is accepted; the tag is refreshed and a warning is logged when it no longer matches the digest.
- The previous digest stays cached, so pulls by digest keep working, until eviction or retention removes it.
- Docker Hub does not count HEAD requests against its pull limit, but every changed manifest is a pull. Set `platforms` and `upstream.auth` for `registry-1.docker.io` to stay under the anonymous limit.
- Mirrored layers are scanned when `scanning.enabled` is set. A blocked layer fails the run and keeps the previous image.
- Schedules are read at startup; changes need a restart. They do not need `mirror_api`.

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
