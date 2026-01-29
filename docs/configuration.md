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
| `base_url` | `PROXY_BASE_URL` | `-base-url` | `http://localhost:8080` | Public URL for the proxy |

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

## Upstream Registries

Override default upstream registry URLs:

```yaml
upstream:
  npm: "https://registry.npmjs.org"
  cargo: "https://index.crates.io"
  cargo_download: "https://static.crates.io/crates"
```

## Authentication

Configure authentication for private upstream registries. Auth is matched by URL prefix, and credentials can reference environment variables using `${VAR_NAME}` syntax.

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

Auth configs are matched by URL prefix. The longest matching prefix wins, so you can configure different credentials for different paths:

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
