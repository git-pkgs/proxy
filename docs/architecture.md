# Architecture

This document describes the internal architecture of the git-pkgs proxy.

## Overview

The proxy is a caching HTTP server that sits between package manager clients and upstream registries. It intercepts requests, checks a local cache, and either serves cached content or fetches from upstream.

```
┌─────────────────────────────────────────────────────────────────┐
│                         HTTP Server                              │
│  ┌─────────────────────────────────────────────────────────┐    │
│  │                    Router (ServeMux)                     │    │
│  │  /npm/*  -> NPMHandler                                   │    │
│  │  /cargo/* -> CargoHandler                                │    │
│  │  /health -> healthHandler                                │    │
│  │  /stats  -> statsHandler                                 │    │
│  └─────────────────────────────────────────────────────────┘    │
│                              │                                   │
│                              ▼                                   │
│  ┌─────────────────────────────────────────────────────────┐    │
│  │                         Proxy                            │    │
│  │  - GetOrFetchArtifact()                                  │    │
│  │  - Coordinates DB, Storage, Fetcher                      │    │
│  └─────────────────────────────────────────────────────────┘    │
│         │                    │                    │              │
│         ▼                    ▼                    ▼              │
│  ┌───────────┐       ┌─────────────┐      ┌─────────────┐       │
│  │  Database │       │   Storage   │      │   Upstream  │       │
│  │  (SQLite) │       │ (Filesystem)│      │  (Fetcher)  │       │
│  └───────────┘       └─────────────┘      └─────────────┘       │
└─────────────────────────────────────────────────────────────────┘
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

SQLite database for cache metadata. Uses `modernc.org/sqlite` (pure Go, no CGO).

**Tables:**

```sql
packages (
    id, purl, ecosystem, name, namespace, latest_version,
    license, description, homepage, repository_url, upstream_url,
    metadata_fetched_at, created_at, updated_at
)

versions (
    id, purl, package_id, version, license, integrity,
    published_at, yanked, metadata_fetched_at, created_at, updated_at
)

artifacts (
    id, version_id, filename, upstream_url, storage_path,
    content_hash, size, content_type, fetched_at,
    hit_count, last_accessed_at, created_at, updated_at
)
```

**Key operations:**
- `GetPackageByPURL()` - Look up package by PURL
- `GetVersionByPURL()` - Look up version by PURL
- `GetArtifact()` - Look up artifact by version + filename
- `UpsertPackage/Version/Artifact()` - Insert or update records
- `RecordArtifactHit()` - Increment hit counter, update access time
- `GetLeastRecentlyUsedArtifacts()` - For cache eviction

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

HTTP server setup.

- Creates and wires together all components
- Mounts handlers at appropriate paths
- Adds logging middleware
- Health and stats endpoints

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
