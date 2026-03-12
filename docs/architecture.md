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
│  │  ...16 ecosystems              /api/*   -> APIHandler     │    │
│  │                                /        -> Web UI         │    │
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
4. Handler rewrites tarball URLs in metadata to point at proxy
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
   - Handler streams file to client

   **Cache Miss:**
   - Resolve download URL using Resolver
   - Fetch artifact from upstream using Fetcher
   - Store artifact in Storage (returns size, hash)
   - Create/update database records (package, version, artifact)
   - Open stored file
   - Return reader to handler
   - Handler streams file to client

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
```

On PostgreSQL, `INTEGER PRIMARY KEY` becomes `SERIAL`, `DATETIME` becomes `TIMESTAMP`, `INTEGER DEFAULT 0` booleans become `BOOLEAN DEFAULT FALSE`, and size/count columns use `BIGINT`.

The `MigrateSchema()` function handles backward compatibility with older git-pkgs databases by adding missing columns via `ALTER TABLE` as needed.

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
- Stores files in nested directories: `{ecosystem}/{name}/{version}/{filename}`
- Atomic writes using temp file + rename
- Computes SHA256 hash during write
- Cleans up empty parent directories on delete

**Path structure:**

```
cache/artifacts/
├── npm/
│   ├── lodash/
│   │   └── 4.17.21/
│   │       └── lodash-4.17.21.tgz
│   └── @babel/
│       └── core/
│           └── 7.23.0/
│               └── core-7.23.0.tgz
└── cargo/
    └── serde/
        └── 1.0.193/
            └── serde-1.0.193.crate
```

### `internal/upstream`

Fetches artifacts from upstream registries.

**Fetcher:**
- HTTP client with configurable timeout (5 min default for large artifacts)
- Exponential backoff retry on 429 (rate limit) and 5xx errors
- Returns streaming reader (doesn't load into memory)
- Configurable user-agent

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
- Rewrites tarball URLs to point at proxy

**CargoHandler:**
- `handleConfig()` - Return registry config
- `handleIndex()` - Proxy sparse index
- `handleDownload()` - Serve cached crate

### `internal/server`

HTTP server setup, web UI, and API handlers.

- Creates and wires together all components
- Mounts protocol handlers at ecosystem-specific paths
- Middleware: request ID, real IP, logging, panic recovery, active request tracking
- Web UI: dashboard, package browser, source browser, version comparison
- Templates are embedded in the binary via `//go:embed`
- Enrichment API for package metadata, vulnerability scanning, and outdated detection
- Health, stats, and Prometheus metrics endpoints

### `internal/metrics`

Prometheus metrics for cache performance, upstream latency, storage operations, and active requests. See the Monitoring section of the README for the full metric list.

### `internal/cooldown`

Version age filtering for supply chain attack mitigation. Configurable at global, ecosystem, and per-package levels. Supported by npm, PyPI, pub.dev, and Composer handlers.

### `internal/enrichment`

Package metadata enrichment. Fetches license, description, homepage, repository URL, and vulnerability data from upstream registries. Powers the `/api/` endpoints and the web UI's package detail pages.

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

## Design Decisions

**Why SQLite?**
- Simple deployment (single file)
- No external dependencies
- Good performance for this workload
- Pure Go driver available (no CGO)

**Why rewrite metadata URLs?**
- Ensures clients fetch artifacts through proxy
- Alternative: Let clients fetch directly, miss cache opportunity

**Why not cache metadata?**
- Simplicity - no invalidation logic needed
- Fresh data - new versions visible immediately
- Metadata is small, upstream fetch is fast

**Why stream artifacts?**
- Memory efficient - don't load large files into RAM
- Better latency - start sending while still receiving

**Why atomic writes?**
- Prevents serving partial files
- Safe concurrent access
- Clean recovery from crashes
