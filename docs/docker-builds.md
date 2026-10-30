# Using proxy in Docker builds

Docker builds can share proxy's package cache even when their builders and local
package caches are new. Configure the package manager inside the build to use
proxy. Cached archives still need to be downloaded from proxy and installed;
native extensions still need to be compiled.

## Run a persistent proxy

Run proxy outside the image build and keep its database and artifact storage
between builds. For a proxy running on your development machine:

```sh
PROXY_CACHE_METADATA=true proxy serve \
  -listen :8080 \
  -base-url http://host.docker.internal:8080 \
  -storage-url "file://$PWD/proxy-cache/artifacts" \
  -database-path ./proxy-cache/proxy.db
```

Docker Desktop provides `host.docker.internal` for reaching the host. On Linux
Docker Engine, add the host mapping when building:

```sh
docker build \
  --add-host host.docker.internal=host-gateway \
  --build-arg PROXY_URL=http://host.docker.internal:8080 \
  -t app .
```

`localhost` inside a build refers to the build container. Both the build's proxy
URL and proxy's `base_url` must be reachable from that container, since some
registry responses contain download URLs derived from `base_url`. The proxy
must listen on an interface reachable from the builder, with access restricted
to the machines that should use it.

For CI or a remote builder, use a persistent service reachable from the builder,
such as `https://proxy.example.com`, and set proxy's `base_url` to that address.
Use the same service across jobs and pass its URL with `--build-arg PROXY_URL=...`.
Creating an empty proxy for every job loses the shared cache unless its database
and storage persist. If proxy runs in a container, mount persistent storage for
both. See [configuration](configuration.md) for storage and deployment options.

Metadata caching is optional, but avoids repeated metadata fetches within
`metadata_ttl`. Artifact caching is enabled by default. Proxy's configured
cooldown, denylist and scanning policies also apply to Docker builds.

## npm

In your Node build stage, copy the manifests before application source so an
unchanged dependency set can reuse Docker's installation layer:

```dockerfile
ARG PROXY_URL
COPY package.json package-lock.json ./
RUN npm ci --no-audit --no-fund --registry="${PROXY_URL:?PROXY_URL build arg required}/npm/"
COPY . .
```

Pass `PROXY_URL` without a trailing slash. With Docker Desktop:

```sh
docker build \
  --build-arg PROXY_URL=http://host.docker.internal:8080 \
  -t app .
```

Keep `node_modules` out of the build context so `COPY . .` does not overwrite
the installed dependencies. Lockfiles with custom registry or download URLs
may still direct requests to those hosts; check proxy's access log when using
private registries or non-registry dependencies. `--no-audit` skips the audit
request, which proxy would otherwise forward upstream.

## Ruby / Bundler

Keep `https://rubygems.org` as the source in your Gemfile and configure its mirror
inside the Ruby build stage. Install any system packages needed to compile
native extensions before this step:

```dockerfile
ARG PROXY_URL
COPY Gemfile Gemfile.lock ./
RUN bundle config set --local mirror.https://rubygems.org "${PROXY_URL:?PROXY_URL build arg required}/gem" \
    && bundle install \
    && bundle config unset --local mirror.https://rubygems.org
COPY . .
```

Use the same build command as the npm example. The mirror setting is removed
after installation so it is not retained in the image, but the URL still
appears in `docker history` because Docker records the build args used by `RUN`
lines. Keep credentials out of `PROXY_URL`. Exclude host Bundler
configuration and installed gems from the build context. Copy any local path
dependencies required by the Gemfile before `bundle install`.

The RubyGems mirror covers gems fetched from that source. Git dependencies and
other gem sources need their own configuration. Without
`mirror.https://rubygems.org.fallback_timeout`, `bundle install` fails if proxy
is unreachable. Setting it lets Bundler fall back to rubygems.org, so this setup
does not enforce network isolation either way.

## Layer caches and package caches

Keep existing Docker caching optimizations. If Docker reuses the installation
layer, the package manager does not run and proxy receives no requests. A
BuildKit cache mount can also preserve downloaded packages when that layer must
be rebuilt. For example, the npm step can use:

```dockerfile
RUN --mount=type=cache,target=/root/.npm \
    npm ci --no-audit --no-fund --registry="${PROXY_URL:?PROXY_URL build arg required}/npm/"
```

Cache mounts are stored by the builder and are excluded from ordinary
`--cache-to` exports. Proxy can supply packages when a fresh builder has neither
the installation layer nor the package-manager cache. See Docker's
[cache guidance](https://docs.docker.com/build/cache/optimize/) and
[cache-mount reference](https://docs.docker.com/reference/dockerfile/#run---mounttypecache).

To check the configuration, enable proxy's JSONL log with `-access-log` and
rebuild with `docker build --no-cache`. Look for requests to `/npm/` or `/gem/`,
then repeat and check `proxy_cache_hits_total` on `/metrics`. Cache mounts still
retain their contents with `--no-cache`; omit them when testing proxy cache
hits. Upstream metadata revalidation can still occur.

## Base-image pulls

Package-manager configuration affects commands such as `npm ci` and
`bundle install`. To cache images referenced by `FROM`, configure a registry
mirror in the builder as well. For a proxy served at
`https://proxy.example.com`, put this in `buildkitd.toml`:

```toml
[registry."docker.io"]
  mirrors = ["proxy.example.com"]
```

Create a Buildx builder with that configuration and select it explicitly:

```sh
docker buildx create --name proxy-builder \
  --driver docker-container \
  --buildkitd-config ./buildkitd.toml

docker buildx build --builder proxy-builder \
  --build-arg PROXY_URL=https://proxy.example.com \
  --load -t app .
```

For local Docker Desktop testing over HTTP, the configuration is:

```toml
[registry."docker.io"]
  mirrors = ["host.docker.internal:8080"]

[registry."host.docker.internal:8080"]
  http = true
```

The registry mirror address must be reachable from the BuildKit daemon itself.
The build's `--add-host` flag only adds a mapping inside build containers. A
separate Buildx builder has its own registry configuration; setting Docker
Engine's `registry-mirrors` does not configure that builder. See Docker's
[BuildKit configuration guide](https://docs.docker.com/build/buildkit/configure/).

Proxy's OCI endpoint is pull-only. BuildKit registry-cache exports with
`--cache-to type=registry` need a separate writable registry. Other cache export
backends can be used alongside proxy.
