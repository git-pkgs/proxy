# Contributing to git-pkgs proxy

Thank you for your interest in contributing. This document covers how to get started, the code structure, and guidelines for submitting changes.

## Development Setup

```bash
# Clone the repository
git clone https://github.com/git-pkgs/proxy.git
cd proxy

# Install dependencies
go mod download

# Run tests
go test ./...

# Build
go build -o proxy ./cmd/proxy

# Run locally
./proxy
```

## Project Structure

```
proxy/
├── cmd/
│   └── proxy/
│       └── main.go          # CLI entry point
├── internal/
│   ├── config/              # Configuration loading
│   │   └── config.go
│   ├── database/            # SQLite cache metadata
│   │   ├── database.go      # DB connection, open/create
│   │   ├── schema.go        # Table definitions
│   │   ├── types.go         # Go structs for tables
│   │   └── queries.go       # CRUD operations
│   ├── storage/             # Artifact file storage
│   │   ├── storage.go       # Storage interface
│   │   └── filesystem.go    # Local filesystem impl
│   ├── upstream/            # Upstream registry clients
│   │   ├── fetcher.go       # HTTP artifact fetching
│   │   └── resolver.go      # Download URL resolution
│   ├── handler/             # Protocol handlers
│   │   ├── handler.go       # Base proxy + cache logic
│   │   ├── npm.go           # npm protocol
│   │   └── cargo.go         # Cargo protocol
│   └── server/              # HTTP server
│       └── server.go        # Router, middleware
├── docs/
│   └── architecture.md      # Internal architecture
├── README.md                # End user documentation
├── CONTRIBUTING.md          # This file
├── go.mod
└── go.sum
```

## Architecture Overview

The proxy is built from several independent packages:

### `internal/database`

SQLite-based cache metadata. Tracks packages, versions, and artifacts. Uses `modernc.org/sqlite` for a pure-Go SQLite driver (no CGO).

Key types:
- `Package` - Package metadata (purl, ecosystem, name)
- `Version` - Version metadata (version number, integrity hash)
- `Artifact` - Cached file info (storage path, size, hash, hit count)

### `internal/storage`

Artifact file storage abstraction. Currently implements local filesystem storage. Designed to allow future backends (S3, GCS).

Interface:
```go
type Storage interface {
    Store(ctx, path string, r io.Reader) (size int64, hash string, err error)
    Open(ctx, path string) (io.ReadCloser, error)
    Exists(ctx, path string) (bool, error)
    Delete(ctx, path string) error
    Size(ctx, path string) (int64, error)
    UsedSpace(ctx context.Context) (int64, error)
}
```

### `internal/upstream`

Fetches artifacts from upstream registries. Two components:

- `Fetcher` - HTTP client with retries, streams artifacts
- `Resolver` - Determines download URLs for each ecosystem

### `internal/handler`

HTTP protocol handlers. Each registry protocol has its own handler:

- `NPMHandler` - npm registry protocol
- `CargoHandler` - Cargo sparse index protocol

Handlers use a shared `Proxy` type that coordinates caching.

### `internal/server`

HTTP server setup, router configuration, middleware.

### `internal/config`

Configuration loading from files, environment variables, and flags.

## Adding a New Registry

To add support for a new package registry:

1. **Add URL resolution** in `internal/upstream/resolver.go`:

```go
case "newregistry":
    url = fmt.Sprintf("https://registry.example.com/%s/%s/download", name, version)
    filename = fmt.Sprintf("%s-%s.tar.gz", name, version)
```

2. **Create handler** in `internal/handler/newregistry.go`:

```go
type NewRegistryHandler struct {
    proxy    *Proxy
    proxyURL string
}

func NewNewRegistryHandler(proxy *Proxy, proxyURL string) *NewRegistryHandler {
    return &NewRegistryHandler{proxy: proxy, proxyURL: proxyURL}
}

func (h *NewRegistryHandler) Routes() http.Handler {
    mux := http.NewServeMux()
    mux.HandleFunc("GET /{name}/{version}", h.handleDownload)
    return mux
}
```

3. **Mount handler** in `internal/server/server.go`:

```go
newHandler := handler.NewNewRegistryHandler(proxy, s.cfg.BaseURL)
mux.Handle("/newregistry/", http.StripPrefix("/newregistry", newHandler.Routes()))
```

4. **Add tests** in `internal/handler/newregistry_test.go`

5. **Update documentation** in README.md

## Code Style

- Follow standard Go conventions
- Run `go fmt` before committing
- Run `go vet` to catch common issues
- Keep functions short and focused
- Write tests for new functionality
- Document exported types and functions

## Testing

Run all tests:

```bash
go test ./...
```

Run with verbose output:

```bash
go test -v ./...
```

Run specific package:

```bash
go test -v ./internal/handler/
```

Run with coverage:

```bash
go test -cover ./...
```

## Pull Request Process

1. Fork the repository
2. Create a branch for your feature (`git checkout -b feature/my-feature`)
3. Make your changes
4. Add tests for new functionality
5. Run `go test ./...` and ensure all tests pass
6. Run `go fmt ./...` and `go vet ./...`
7. Commit with a clear message
8. Push to your fork
9. Open a pull request

### Commit Messages

Write clear, concise commit messages:

```
Add PyPI registry support

- Add URL resolution for PyPI packages
- Create PyPI protocol handler
- Add tests for wheel and sdist downloads
```

## Questions?

Open an issue on GitHub if you have questions or need help getting started.
