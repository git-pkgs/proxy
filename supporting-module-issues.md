# Package repository support issues

These drafts are grouped by the repository where the work belongs. The proxy
issues are already open. The remaining drafts have not been posted.

Helm, Chef, and Vagrant are listed as candidate PURL types. Helm has an open
upstream pull request, while Chef and Vagrant still need upstream type
definitions. Their registry clients should follow the accepted definitions
when exposing package identities through shared APIs.

## `git-pkgs/proxy`

Already open:

- [#261 Add Helm repository proxy support](https://github.com/git-pkgs/proxy/issues/261)
- [#262 Add Alpine APK repository proxy support](https://github.com/git-pkgs/proxy/issues/262)
- [#263 Add Arch Linux package repository proxy support](https://github.com/git-pkgs/proxy/issues/263)
- [#264 Add Chef Supermarket proxy support](https://github.com/git-pkgs/proxy/issues/264)
- [#265 Add Vagrant box repository proxy support](https://github.com/git-pkgs/proxy/issues/265)

## `package-url/purl-spec`

Helm type work is already open as
[#236](https://github.com/package-url/purl-spec/pull/236). The pull request
needs to be updated to the current JSON type-definition format. Do not open a
second Helm proposal.

### Define the Chef PURL type

Chef is listed as a candidate PURL type, but it has no type definition or
tracking issue.

Define how Chef cookbooks map to PURL components. Cover cookbook names,
versions, Supermarket hosts, private Supermarket instances, and source
repositories. Include type tests and examples for the public Supermarket and
a configured private server.

### Define the Vagrant PURL type

Vagrant is listed as a candidate PURL type, but it has no type definition or
tracking issue.

Define how Vagrant boxes map to PURL components. Cover namespaces, box names,
versions, providers, architectures, catalog URLs, and checksums. Include type
tests and examples for Vagrant Cloud and a configured private catalog.

## `git-pkgs/purl`

### Add Helm, Chef, and Vagrant PURL type support

The upstream specification lists Helm, Chef, and Vagrant as candidate types.
Helm has an open type proposal, while Chef and Vagrant still need definitions.

After each definition is accepted upstream, add it to the local type catalog
and implement any required normalization, validation, version handling, and
OSV mappings. Add construction, parsing, normalization, and round-trip tests
using the upstream examples.

## `git-pkgs/manifests`

### Add Helm chart manifest and lockfile support

Add parsers for `Chart.yaml` and `Chart.lock`.

`Chart.yaml` should report the chart name, version, declared dependencies,
repository URLs, and dependency version constraints. `Chart.lock` should
report the resolved dependency set and digest without treating the generated
timestamp as package identity.

Tests should cover aliased repositories, OCI repository URLs, conditions,
tags, and charts with no dependencies.

### Add Chef cookbook manifest support

Add static parsers for Chef `metadata.rb` and `Berksfile` files. Do not execute
Ruby while parsing them.

The metadata parser should report the cookbook name, version, license, and
`depends` declarations. The Berksfile parser should report cookbook sources,
version constraints, and git or path sources when they are static string
literals.

Tests should cover unconstrained dependencies, pessimistic constraints,
custom Supermarket sources, and dynamic Ruby expressions that must be skipped
without failing the whole file.

### Add Vagrant box declarations from Vagrantfile

Add a static `Vagrantfile` parser for box dependencies. Do not execute the
Ruby configuration file.

Recognize literal values assigned to `config.vm.box`, `box_version`,
`box_url`, `box_download_checksum`, and `box_download_checksum_type`. Return
the box as a direct dependency with its version, source URL, and integrity
metadata when available.

Tests should cover namespaced boxes, version constraints, custom catalog URLs,
checksums, and dynamic expressions that cannot be resolved safely.

Alpine `APKBUILD` and Arch `PKGBUILD` parsing already exist, so they do not
need new manifest issues.

## `git-pkgs/registries`

### Add Helm HTTP repository metadata support

Add a registry client for HTTP Helm repositories backed by `index.yaml`.

Map charts and chart releases to the shared package and version types. Include
creation time, digest, deprecation status, home and source links,
dependencies, maintainers, and absolute or relative artifact URLs when those
fields are present.

Require a configured repository URL because Helm has no single default HTTP
repository. Tests should cover multiple chart versions, relative URLs,
deprecated charts, missing optional fields, and custom authenticated
repositories.

This issue depends on an accepted Helm PURL type definition. Track
[purl-spec#236](https://github.com/package-url/purl-spec/pull/236).

### Add OCI registry metadata support

Add a registry client for `pkg:oci` packages. Support tag listing, manifest
retrieval by tag or digest, OCI indexes, annotations, and Bearer authentication
challenges.

Preserve the artifact type and layer media types so callers can distinguish
container images, Helm charts, and other OCI artifacts. Tests should include
the Helm config and chart layer media types as well as ordinary image
manifests.

### Add Alpine APK repository metadata support

Add an `apk` registry client for configured Alpine repositories. There is no
single default repository, so `repository_url` should identify a concrete
release, repository, and architecture location.

Read package names, versions, descriptions, licenses, dependencies, build
times, maintainers, architectures, and checksums from v2 `APKINDEX.tar.gz` and
v3 `Packages.adb` indexes. Keep namespace handling compatible with standard
`pkg:apk` PURLs.

Tests should cover v2 and v3 indexes, multiple architectures, versioned PURLs,
missing optional fields, and authenticated repositories.

### Add Arch Linux repository metadata support

Add an `alpm` registry client for configured pacman repositories. There is no
single default repository, so `repository_url` should identify the repository
and architecture location.

Parse repository database entries into package metadata, versions,
dependencies, licenses, build dates, packagers, architectures, and SHA-256
checksums. Keep namespace handling compatible with standard `pkg:alpm` PURLs.

Tests should cover compressed repository databases, epoch and package-release
versions, architecture qualifiers, missing optional fields, and authenticated
repositories.

### Add Chef Supermarket registry support

Add a Chef Supermarket registry client using `/universe` and the cookbook API.

Return cookbook metadata, available versions, publication times,
dependencies, maintainers, source links, and versioned download locations.
Support the public Supermarket and configured private Supermarket servers.

Tests should cover dependency graphs, version metadata, custom base URLs,
authentication, redirects, and missing cookbooks.

This issue depends on an accepted Chef PURL type definition.

### Add Vagrant box catalog registry support

Add a Vagrant registry client that reads box catalog metadata and supports
Vagrant Cloud catalog responses.

Return box descriptions, versions, providers, architectures, download URLs,
checksums, and release timestamps when available. Support custom catalog URLs
and configured credentials.

Tests should cover multiple providers and architectures, legacy catalog
entries without architecture fields, checksum types, redirects, private
catalogs, and missing boxes.

This issue depends on an accepted Vagrant PURL type definition.

## `git-pkgs/archives`

### Handle real Alpine APK v2 and v3 packages

The archive reader currently routes Alpine `.apk` files to the shared gzip
and tar reader. Signed APK v2 packages contain concatenated gzip streams for
the signature, control metadata, and payload. A standard tar reader stops after
the first tar stream, so it cannot reliably browse or extract the package
payload. APK v3 adds the ADB format.

Add format-aware APK readers that expose package metadata and payload files
without confusing Android APK ZIP files. Preserve the existing decompression
and path safety limits.

Tests should use signed v2 and v3 fixtures and cover listing, extraction,
malformed stream boundaries, decompression limits, and Android APK detection.

Arch `.pkg.tar.zst` support is already tracked by
[archives#23](https://github.com/git-pkgs/archives/issues/23), so do not open a
duplicate zstd issue. Helm charts, Chef cookbooks, and Vagrant boxes use
physical archive formats the module can already detect.

## `git-pkgs/magic`

### Detect Alpine APK v3 ADB data

Add byte-signature detection for Alpine APK v3 ADB indexes and packages. Keep
the result at the physical format level, consistent with the existing gzip,
zstd, tar, and ZIP classifications.

Tests should cover valid ADB prefixes, short input, malformed headers, and
nearby byte sequences that must remain unclassified.

## `git-pkgs/integrity`

### Add verification-only MD5 and SHA-1 checksums

Vagrant box catalogs may publish MD5 or SHA-1 checksums as well as SHA-256,
SHA-384, and SHA-512. Add MD5 and SHA-1 algorithms for checking bytes against
publisher-supplied metadata.

Keep the weak algorithms out of SRI parsing and formatting, and document that
they are provided only for compatibility verification. They must not be
selected for newly generated integrity metadata when a stronger algorithm is
available.

Add reader, hexadecimal parsing, length validation, verification, and failure
tests for both algorithms.

## `git-pkgs/managers`

These command definitions are useful for client integration and broader tool
support, but they do not block the proxy handlers.

### Add apk package manager commands

Add an `apk` manager definition for Alpine packages. Cover install, add,
remove, list, outdated, update, resolve, and path operations where apk exposes
a suitable command.

Tests should verify generated commands, repository arguments, versioned
packages, non-interactive flags, and operations that require elevated
permissions.

### Add pacman package manager commands

Add a `pacman` manager definition for Arch packages. Cover install, add,
remove, list, outdated, update, resolve, and path operations where pacman
exposes a suitable command.

Tests should verify generated commands, repository refresh behavior,
versioned packages, non-interactive flags, and operations that require
elevated permissions.

### Add Berkshelf commands

Add a `berks` manager definition for Chef cookbook dependencies. Cover
install, list, outdated, update, resolve, and vendor operations.

Tests should verify generated commands, frozen or locked installs, custom
Berksfile paths, and JSON or machine-readable output where Berkshelf provides
it.

### Add Vagrant box commands

Add a `vagrant` manager definition for box operations. Cover box add, remove,
list, outdated, update, and path operations where Vagrant exposes a suitable
command.

Tests should verify namespaced boxes, versions, providers, architectures,
custom catalog URLs, checksums, and non-interactive flags.

Helm manager support already exists.

## `git-pkgs/enrichment`

### Expose new package registry clients through enrichment

After the registry clients and PURL conventions land, update `enrichment` to
expose metadata for Helm, Alpine, Arch, Chef, and Vagrant packages through the
direct and hybrid clients.

Add dependency updates and conversion tests for package metadata, versions,
publication times, integrity values, status fields, and custom
`repository_url` values. Alpine and Arch vulnerability PURL translation
already exists and should remain covered.

This issue should wait until the corresponding `registries` work is available.
