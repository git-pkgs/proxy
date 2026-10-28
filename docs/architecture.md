# Architecture

This document describes the internal architecture of the git-pkgs proxy.

## Overview

The proxy is a caching HTTP server that sits between package manager clients and upstream registries. It intercepts requests, checks a local cache, and either serves cached content or fetches from upstream.

```
┌──────────────────────────────────────────────────────────────────┐
│                          HTTP Server                              │
│  ┌──────────────────────────────────────────────────────────┐    │
│  │                     Router (Chi)                          │    │
│  │  /npm/*     -> NPMHandler      /health  -> healthHandler  │    │
│  │  /cargo/*   -> CargoHandler    /stats   -> statsHandler   │    │
│  │  /gem/*     -> GemHandler      /metrics -> prometheus     │    │
│  │  ...17 ecosystems              /api/*   -> APIHandler     │    │
│  │                                /ui/*    -> Web UI         │    │
│  └──────────────────────────────────────────────────────────┘    │
│         │                    │                    │               │
│         ▼                    ▼                    ▼               │
│  ┌───────────┐       ┌─────────────┐      ┌─────────────┐       │
│  │ Database  │       │   Storage   │      │   Upstream  │       │
│  │ SQLite or │       │ Filesystem  │      │  Registries │       │
│  │ Postgres  │       │  or S3      │      │  (Fetcher)  │       │
│  └───────────┘       └─────────────┘      └─────────────┘       │
└──────────────────────────────────────────────────────────────────┘
```

## Request Flow

### Metadata Request (npm example)

1. Client requests `GET /npm/lodash`
2. NPMHandler receives request
3. Handler fetches metadata from upstream `registry.npmjs.org/lodash`
4. Handler rewrites tarball URLs in metadata to point at proxy, and filters versions withheld by cooldown or the denylist (`internal/metadata/npm`)
5. Handler returns modified metadata to client

Metadata is not cached - always fetched fresh. This ensures clients see new versions immediately.

### Artifact Download (npm example)

1. Client requests `GET /npm/lodash/-/lodash-4.17.21.tgz`
2. NPMHandler extracts package name and version from URL
3. Handler calls `Proxy.GetOrFetchArtifact()`
4. Proxy checks database for cached artifact:

   **Cache Hit:**
   - Look up artifact record in database
   - Open file from storage
   - Record hit (increment counter, update last_accessed_at)
   - Return reader to handler
   - Handler streams file to client, checking it against the recorded
     SHA-256 `content_hash` (the registry's own integrity was checked when the
     file was stored); a mismatch aborts the response and clears the record

   **Cache Miss:**
   - Resolve download URL using Resolver
   - Fetch artifact from upstream using Fetcher
   - Copy the body to Storage and, for a client GET without a `Range`
     header, to the client at once (`storeWhileServing`); the client's copy
     is checked against the upstream's digest as it is read, and a mismatch
     aborts it. A block-mode scanner, `HEAD` and range requests, and
     requests joining a fetch already running wait for the stored file.
   - Store artifact in Storage (returns size, hash)
   - Create/update database records (package, version, artifact)
   - Requests that waited open the stored file and stream it to their client

Artifact handlers honor single byte ranges when the storage reader supports
efficient seeking (currently the local filesystem reader). They advertise
`Accept-Ranges: bytes`, return `206` or `416` as appropriate, and respect
`If-Range`. Malformed and multi-range requests fall back to the full response.
Non-seekable storage readers do not advertise range support, and direct-storage
redirects rely on the destination's capabilities. Range responses cannot verify
the full artifact digest because they read only part of the artifact.

```
┌────────┐  GET /npm/lodash/-/lodash-4.17.21.tgz  ┌─────────────┐
│ Client │ ──────────────────────────────────────▶│ NPMHandler  │
└────────┘                                        └──────┬──────┘
                                                         │
                                                         ▼
                                               ┌─────────────────┐
                                               │ Proxy           │
                                               │ GetOrFetch      │
                                               └────────┬────────┘
                                                        │
                                    ┌───────────────────┼───────────────────┐
                                    │                   │                   │
                                    ▼                   ▼                   ▼
                             ┌───────────┐       ┌───────────┐       ┌───────────┐
                             │ Database  │       │  Storage  │       │ Upstream  │
                             │ (lookup)  │       │  (read)   │       │ (fetch)   │
                             └───────────┘       └───────────┘       └───────────┘
```

## Package Structure

### `internal/database`

SQLite or PostgreSQL database for cache metadata. SQLite uses `modernc.org/sqlite` (pure Go, no CGO). PostgreSQL uses `lib/pq`.

For SQLite, `DB` holds two pools: one connection for writes and a pool of read-only (`query_only`) connections for reads. `Get`, `Select`, `Query` and `QueryRow` go to the reader; `Exec`, transactions and everything else go to the writer, so a read inside a transaction sees the transaction's writes. A write sent to the reader fails instead of running. WAL mode lets the readers run while the writer commits. PostgreSQL and in-memory SQLite use one pool for both.

The schema is compatible with [git-pkgs](https://github.com/git-pkgs) databases. The proxy adds the `artifacts` and `vulnerabilities` tables on top of the shared `packages` and `versions` tables, so both tools can point at the same database.

**Tables:**

```sql
packages (
    id          INTEGER PRIMARY KEY,  -- SERIAL on Postgres
    purl        TEXT NOT NULL,        -- unique, e.g. pkg:npm/lodash
    ecosystem   TEXT NOT NULL,
    name        TEXT NOT NULL,
    latest_version  TEXT,
    license         TEXT,
    description     TEXT,
    homepage        TEXT,
    repository_url  TEXT,
    registry_url    TEXT,
    supplier_name   TEXT,
    supplier_type   TEXT,
    source          TEXT,
    enriched_at     DATETIME,
    vulns_synced_at DATETIME,
    created_at      DATETIME,
    updated_at      DATETIME
)
-- indexes: purl (unique), (ecosystem, name)

versions (
    id           INTEGER PRIMARY KEY,
    purl         TEXT NOT NULL,       -- unique, e.g. pkg:npm/lodash@4.17.21
    package_purl TEXT NOT NULL,       -- FK to packages.purl
    license      TEXT,
    published_at DATETIME,
    integrity    TEXT,                -- subresource integrity hash
    yanked       INTEGER DEFAULT 0,  -- BOOLEAN on Postgres
    source       TEXT,
    enriched_at  DATETIME,
    created_at   DATETIME,
    updated_at   DATETIME
)
-- indexes: purl (unique), package_purl

artifacts (
    id             INTEGER PRIMARY KEY,
    version_purl   TEXT NOT NULL,
    filename       TEXT NOT NULL,
    upstream_url   TEXT NOT NULL,
    storage_path   TEXT,              -- null until cached
    content_hash   TEXT,              -- SHA-256
    size           INTEGER,           -- BIGINT on Postgres
    content_type   TEXT,
    fetched_at     DATETIME,
    hit_count      INTEGER DEFAULT 0, -- BIGINT on Postgres
    last_accessed_at DATETIME,
    created_at     DATETIME,
    updated_at     DATETIME
)
-- indexes: (version_purl, filename) unique, storage_path, last_accessed_at

pending_deletes (
    path          TEXT NOT NULL PRIMARY KEY, -- storage path no record points at
    queued_at     DATETIME NOT NULL
)

vulnerabilities (
    id            INTEGER PRIMARY KEY,
    vuln_id       TEXT NOT NULL,      -- e.g. CVE-2021-1234
    ecosystem     TEXT NOT NULL,
    package_name  TEXT NOT NULL,
    severity      TEXT,
    summary       TEXT,
    fixed_version TEXT,
    cvss_score    REAL,
    "references"  TEXT,               -- JSON array
    fetched_at    DATETIME,
    created_at    DATETIME,
    updated_at    DATETIME
)
-- indexes: (vuln_id, ecosystem, package_name) unique, (ecosystem, package_name)

metadata_cache (
    id            INTEGER PRIMARY KEY,
    ecosystem     TEXT NOT NULL,
    name          TEXT NOT NULL,
    storage_path  TEXT NOT NULL,
    etag          TEXT,
    content_type  TEXT,
    content_encoding TEXT,           -- replayed on serve so signed bytes stay verbatim
    size          INTEGER,           -- BIGINT on Postgres
    fetched_at    DATETIME,
    created_at    DATETIME,
    updated_at    DATETIME
)
-- indexes: (ecosystem, name) unique

metadata_dates (
    ecosystem     TEXT NOT NULL,
    proxy_url     TEXT NOT NULL,      -- each proxy URL rewrites to its own download URLs
    name          TEXT NOT NULL,
    etag          TEXT NOT NULL,      -- the rewritten representation being dated
    last_modified INTEGER NOT NULL,   -- Unix seconds; BIGINT on Postgres
    PRIMARY KEY (ecosystem, proxy_url, name)
)
```

`metadata_dates` holds the `Last-Modified` served with rewritten metadata, such as npm packuments, Composer documents, PyPI simple pages and Helm indexes. A package's date is the second it started serving its current representation, and a new representation is only dated in a later second. Keeping it in the database means replicas sharing one serve the same date, and never give one representation's date to another, so `If-Modified-Since` cannot return 304 for a body the client does not have.

On PostgreSQL, `INTEGER PRIMARY KEY` becomes `SERIAL`, `DATETIME` becomes `TIMESTAMP`, `INTEGER DEFAULT 0` booleans become `BOOLEAN DEFAULT FALSE`, and size/count columns use `BIGINT`.

The `MigrateSchema()` function handles backward compatibility with older git-pkgs databases by running named migrations that add missing columns and tables. See [migrations.md](migrations.md) for how to add new schema changes.

**Key operations:**
- `GetPackageByPURL()` - Look up package by PURL
- `GetVersionByPURL()` - Look up version by PURL
- `GetArtifact()` - Look up artifact by version + filename
- `UpsertPackage/Version/Artifact()` - Insert or update records
- `RecordArtifactHit()` - Increment hit counter, update access time
- `GetLeastRecentlyUsedArtifacts()` - For cache eviction
- `SearchPackages()` - Full-text search across cached packages

### `internal/storage`

File storage abstraction. Current implementation uses local filesystem.

**Interface:**

```go
type Storage interface {
    Store(ctx, path, reader) (size, hash, error)
    Open(ctx, path) (io.ReadCloser, error)
    Exists(ctx, path) (bool, error)
    Delete(ctx, path) error
    Size(ctx, path) (int64, error)
    UsedSpace(ctx) (int64, error)
}
```

**Filesystem implementation:**
- Stores files in nested directories: `{ecosystem}/{name}/{version}/{fetch id}/{filename}`, a new id per fetch, so fetches of one artifact never overwrite or delete each other's object (artifacts cached before this sit at `{ecosystem}/{name}/{version}/{filename}`)
- Atomic writes using temp file + rename
- Computes SHA256 hash during write
- Removes a fetch's directory once its object is deleted

**Path structure:**

```
cache/artifacts/
├── npm/
│   ├── lodash/
│   │   └── 4.17.21/
│   │       └── 3f9a0c1d2e4b5a67/
│   │           └── lodash-4.17.21.tgz
│   └── @babel/
│       └── core/
│           └── 7.23.0/
│               └── 8c2e41f09a7d3b15/
│                   └── core-7.23.0.tgz
└── cargo/
    └── serde/
        └── 1.0.193/
            └── d05b7e9c14a2f863/
                └── serde-1.0.193.crate
```

### `internal/upstream`

Fetches artifacts from upstream registries.

**Fetcher:**
- HTTP client with configurable timeout (5 min default for large artifacts)
- Exponential backoff retry on 429 (rate limit) and 5xx errors
- Returns streaming reader (doesn't load into memory)
- Configurable user-agent
- Shares an authentication-aware transport with metadata requests so URL-scoped credentials apply consistently
- Discovers and caches scoped OCI Bearer tokens from registry challenges

**Resolver:**
- Determines download URL for a package/version
- Handles ecosystem-specific URL patterns:
  - npm: `https://registry.npmjs.org/{name}/-/{shortname}-{version}.tgz`
  - cargo: `https://static.crates.io/crates/{name}/{name}-{version}.crate`
  - etc.

### `internal/handler`

HTTP protocol handlers for each registry type.

**Proxy (shared):**
- `GetOrFetchArtifact()` - Main cache logic
- Coordinates database, storage, and fetcher
- Handles cache hit/miss flow

**NPMHandler:**
- `handlePackageMetadata()` - Proxy + rewrite metadata
- `handleDownload()` - Serve cached artifact
- Fetches, caches and serves packuments, and caches their rewrites; the rewrite itself is `internal/metadata/npm`

**CargoHandler:**
- `handleConfig()` - Return registry config
- `handleIndex()` - Proxy sparse index
- `handleDownload()` - Serve cached crate

**SwiftHandler:**
- Proxies the Swift Package Registry v1 read endpoints
- Rewrites release URLs and caches source archives
- With `cache_metadata`, caches release metadata, release lists and manifests (with their `Link` header) and serves them when the upstream fails

**GitHandler:**
- Serves `/git/{name}/{repo}` from `internal/gitcache`, read-only over smart HTTP
- Validates repository paths and the `repos` allowlist, and maps cache errors to statuses
- Serves the Git LFS API under `{repo}.git/info/lfs/`: forwards download batches upstream and points each object at `objects/{oid}/{size}`, which is served from the artifact cache (ecosystem `git`, name `{name}/{repo}`, version the object ID); a miss uses the download URL kept from the batch or asks the upstream batch API for a fresh one, recorded without its signed query, copies the object from another repository when its content is already stored, and checks the object against its sha256 ID; a batch the upstream can't answer is answered from the cache

### `internal/metadata/npm`

The npm metadata rewrite, separated from the handler so it can run where the proxy's infrastructure can't, such as WASI. It takes a packument's bytes and everything else as explicit inputs (proxy URL, cooldown policy, the time cooldown is evaluated at, denied versions, pushed versions) and returns the rewritten document with the versions it kept. It filters versions by cooldown and the denylist, moves `latest` off a withheld version, lists pushed versions in place of upstream ones, and points each tarball URL at the proxy, copying everything else byte for byte. It does no I/O and never reads the clock; callbacks report what it did, which the handler logs. It imports no database, storage, metrics, HTTP or logging packages, which a test checks.

### `internal/jsonscan`

Reads and rewrites members of a JSON document in place, by byte offsets, without decoding the whole document. Shared by the npm and Composer metadata rewrites.

### `internal/gitcache`

Local bare clones of upstream Git repositories, outside the artifact cache and the database, at `{git.cache_dir}/{name}-{URL hash}/{repo}.git`. The hash of the upstream's URL means repointing an upstream starts a fresh cache; at startup only the configured upstreams' directories are loaded, and those of their old URLs removed.

- Each request for refs checks the upstream's branches and tags with protocol v2 `ls-refs` and fetches only when they changed; concurrent requests share one refresh
- Clients are served by `git upload-pack --stateless-rpc`
- Spawned `git` reaches the upstream only through a loopback relay that forwards with the proxy's upstream client, so address checks, `upstream.auth` and the access log apply
- Commits a client asks for by ID that are on no cached branch or tag are fetched and kept under hidden `refs/proxy/wanted/{time}/{id}` refs until a branch or tag includes them or for 7 days; off for upstreams with credentials
- Evicts least recently used repositories after each fetch once `git.max_size` is passed, and drops a repository that can no longer be refreshed within `git.max_repo_size` so it is cloned again; never one in use. A commit fetched by ID that does not fit is refused instead
- Every fetched pack is kept as a pack (`transfer.unpackLimit=1`), so a transfer cut off at the size limit leaves nothing behind

### `internal/server`

HTTP server setup, web UI, and API handlers.

- Creates and wires together all components
- Mounts protocol handlers at ecosystem-specific paths
- Middleware: request ID, real IP, logging, panic recovery, active request tracking
- Web UI under `/ui`: dashboard, package browser, source browser, version comparison
- Templates are embedded in the binary via `//go:embed`
- Enrichment API for package metadata, vulnerability scanning, and outdated detection
- Health, stats, and Prometheus metrics endpoints. `/health` runs an active write → size-check → read → verify → delete probe against the storage backend and returns a structured JSON response (`HealthResponse`) with `"ok"` / `"error"` status per subsystem. Probe results are cached (default 30 s, configurable via `health.storage_probe_interval`) to avoid overwhelming remote backends. The response also carries a `circuit_breakers` map reporting each upstream's artifact-fetch breaker as `"open"` or `"closed"`, keyed by the host fetched from (or an opaque placeholder where the fetch URL has no host to read); the same state is published as the `proxy_circuit_breaker_state` gauge on each `/metrics` scrape. An open breaker leaves the overall status `"ok"` — it describes an upstream, not this proxy.

### `internal/memlimit`

Sets Go's soft memory limit to 80% of the cgroup memory limit at startup on Linux, unless `GOMEMLIMIT` is set. See "Memory limit" in the configuration docs.

### `internal/metrics`

Prometheus metrics for cache performance, upstream latency, storage operations, and active requests. See the Monitoring section of the README for the full metric list.

### Cooldown

Version age filtering for supply chain attack mitigation, provided by [github.com/git-pkgs/cooldown](https://github.com/git-pkgs/cooldown). Configurable at global, ecosystem, and per-package levels. Supported by npm, PyPI, pub.dev, and Composer handlers.

### `internal/enrichment`

Package metadata enrichment, built on [github.com/git-pkgs/enrichment](https://github.com/git-pkgs/enrichment). Fetches license, description, homepage, repository URL, and vulnerability data from upstream registries. Powers the `/api/` endpoints and the web UI's package detail pages.

### `internal/mirror`

Selective package mirroring for pre-populating the proxy cache. Supports multiple input sources: individual PURLs (versioned or unversioned), CycloneDX/SPDX SBOM files, and full registry enumeration. Uses a bounded worker pool backed by `errgroup` to download artifacts in parallel, reusing `handler.Proxy.GetOrFetchArtifact()` for the actual fetch-and-cache work. Container images (`pkg:docker`, `pkg:oci`) go through `handler.ContainerHandler.MirrorImage()` instead, which walks index, platform manifests and blobs and stores them under the keys a pull reads.

The package also provides a `MetadataCache` for storing raw upstream metadata blobs so the proxy can serve metadata responses offline. The `JobStore` manages async mirror jobs exposed via the `/api/mirror` endpoints.

### `internal/config`

Configuration loading.

- Supports YAML and JSON files
- Environment variable overrides (PROXY_ prefix)
- Command line flag overrides
- Validation

## Extending the Proxy

### Adding a New Registry

1. Add URL resolution in `upstream/resolver.go`
2. Create handler in `handler/newregistry.go`
3. Mount in `server/server.go`
4. Add tests

### Adding a New Storage Backend

1. Implement `storage.Storage` interface
2. Add configuration options in `config/config.go`
3. Add initialization in `server/server.go`

### Cache Eviction

The database tracks `hit_count` and `last_accessed_at` for LRU eviction. Query with:

```go
db.GetLeastRecentlyUsedArtifacts(limit)
```

Eviction can be implemented as:
1. Background goroutine checking `GetTotalCacheSize()`
2. When over limit, get LRU artifacts
3. Delete from storage and clear database records

An object a record stops pointing at, because a refetch replaced it or its entry was discarded, is not deleted at once: a request that read the record may still be opening it. Its path goes into `pending_deletes`, and a background loop deletes it after a grace period of at least an hour, or `direct_serve_ttl` if longer, so signed URLs to it stay valid. This runs whether or not `max_size` is set.

## Design Decisions

**Why SQLite?**
- Simple deployment (single file)
- No external dependencies
- Good performance for this workload
- Pure Go driver available (no CGO)

**Why rewrite metadata URLs?**
- Ensures clients fetch artifacts through proxy
- Alternative: Let clients fetch directly, miss cache opportunity

**Why not cache metadata (by default)?**
- Simplicity - no invalidation logic needed
- Fresh data - new versions visible immediately
- Metadata is small, upstream fetch is fast
- Set `cache_metadata: true` or use the mirror command to enable metadata caching for offline use via the `metadata_cache` table
- OCI manifests and tag lists are exceptions: they are cached automatically so previously fetched images remain pullable and tag resolution works when the registry or token service is unavailable

**Why stream artifacts?**
- Memory efficient - don't load large files into RAM
- Better latency - start sending while still receiving

**Why atomic writes?**
- Prevents serving partial files
- Safe concurrent access
- Clean recovery from crashes
