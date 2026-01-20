package upstream

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrUnsupportedEcosystem = errors.New("unsupported ecosystem")
	ErrNoDownloadURL        = errors.New("no download URL available")
)

// Registry provides package metadata and URL information.
// This interface is compatible with github.com/git-pkgs/registries.Registry
type Registry interface {
	Ecosystem() string
	FetchVersions(ctx context.Context, name string) ([]Version, error)
	URLs() URLBuilder
}

// Version represents a package version with metadata.
type Version struct {
	Number    string
	Integrity string
	Metadata  map[string]any
}

// URLBuilder constructs URLs for a registry.
type URLBuilder interface {
	Download(name, version string) string
}

// Resolver determines download URLs for package artifacts.
type Resolver struct {
	registries map[string]Registry
}

// NewResolver creates a new URL resolver.
func NewResolver() *Resolver {
	return &Resolver{
		registries: make(map[string]Registry),
	}
}

// RegisterRegistry adds a registry for URL resolution.
func (r *Resolver) RegisterRegistry(reg Registry) {
	r.registries[reg.Ecosystem()] = reg
}

// ArtifactInfo contains information about a downloadable artifact.
type ArtifactInfo struct {
	URL       string
	Filename  string
	Integrity string // sha256-... or sha512-...
}

// Resolve returns the download URL and filename for a package artifact.
func (r *Resolver) Resolve(ctx context.Context, ecosystem, name, version string) (*ArtifactInfo, error) {
	reg, ok := r.registries[ecosystem]
	if !ok {
		return r.resolveWithoutRegistry(ecosystem, name, version)
	}

	// Try the simple URL builder first
	if url := reg.URLs().Download(name, version); url != "" {
		return &ArtifactInfo{
			URL:      url,
			Filename: filenameFromURL(url),
		}, nil
	}

	// For ecosystems like PyPI, we need to fetch metadata to get the URL
	return r.resolveFromMetadata(ctx, reg, name, version)
}

// resolveWithoutRegistry handles ecosystems with predictable URLs
// when no registry client is configured.
func (r *Resolver) resolveWithoutRegistry(ecosystem, name, version string) (*ArtifactInfo, error) {
	var url, filename string

	switch ecosystem {
	case "npm":
		shortName := name
		if idx := strings.LastIndex(name, "/"); idx >= 0 {
			shortName = name[idx+1:]
		}
		url = fmt.Sprintf("https://registry.npmjs.org/%s/-/%s-%s.tgz", name, shortName, version)
		filename = fmt.Sprintf("%s-%s.tgz", shortName, version)

	case "cargo":
		url = fmt.Sprintf("https://static.crates.io/crates/%s/%s-%s.crate", name, name, version)
		filename = fmt.Sprintf("%s-%s.crate", name, version)

	case "gem":
		url = fmt.Sprintf("https://rubygems.org/downloads/%s-%s.gem", name, version)
		filename = fmt.Sprintf("%s-%s.gem", name, version)

	case "golang":
		encoded := encodeGoModule(name)
		url = fmt.Sprintf("https://proxy.golang.org/%s/@v/%s.zip", encoded, version)
		filename = fmt.Sprintf("%s@%s.zip", lastPathComponent(name), version)

	case "hex":
		url = fmt.Sprintf("https://repo.hex.pm/tarballs/%s-%s.tar", name, version)
		filename = fmt.Sprintf("%s-%s.tar", name, version)

	case "pub":
		url = fmt.Sprintf("https://pub.dev/packages/%s/versions/%s.tar.gz", name, version)
		filename = fmt.Sprintf("%s-%s.tar.gz", name, version)

	case "maven":
		// Maven name format is "group:artifact", e.g., "com.google.guava:guava"
		parts := strings.SplitN(name, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid maven name format, expected group:artifact")
		}
		group := strings.ReplaceAll(parts[0], ".", "/")
		artifact := parts[1]
		url = fmt.Sprintf("https://repo1.maven.org/maven2/%s/%s/%s/%s-%s.jar", group, artifact, version, artifact, version)
		filename = fmt.Sprintf("%s-%s.jar", artifact, version)

	case "nuget":
		// NuGet package IDs are case-insensitive, use lowercase
		lowername := strings.ToLower(name)
		url = fmt.Sprintf("https://api.nuget.org/v3-flatcontainer/%s/%s/%s.%s.nupkg", lowername, version, lowername, version)
		filename = fmt.Sprintf("%s.%s.nupkg", lowername, version)

	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedEcosystem, ecosystem)
	}

	return &ArtifactInfo{
		URL:      url,
		Filename: filename,
	}, nil
}

// resolveFromMetadata fetches version metadata to find download URL.
func (r *Resolver) resolveFromMetadata(ctx context.Context, reg Registry, name, version string) (*ArtifactInfo, error) {
	versions, err := reg.FetchVersions(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("fetching versions: %w", err)
	}

	for _, v := range versions {
		if v.Number != version {
			continue
		}

		// Look for download URL in metadata
		if v.Metadata != nil {
			if url, ok := v.Metadata["download_url"].(string); ok && url != "" {
				return &ArtifactInfo{
					URL:       url,
					Filename:  filenameFromURL(url),
					Integrity: v.Integrity,
				}, nil
			}
			if url, ok := v.Metadata["tarball"].(string); ok && url != "" {
				return &ArtifactInfo{
					URL:       url,
					Filename:  filenameFromURL(url),
					Integrity: v.Integrity,
				}, nil
			}
		}

		return nil, ErrNoDownloadURL
	}

	return nil, ErrNotFound
}

func filenameFromURL(url string) string {
	if idx := strings.LastIndex(url, "/"); idx >= 0 {
		return url[idx+1:]
	}
	return url
}

func lastPathComponent(path string) string {
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		return path[idx+1:]
	}
	return path
}

// encodeGoModule encodes a module path per goproxy protocol.
// Capital letters become "!" followed by lowercase.
func encodeGoModule(path string) string {
	var b strings.Builder
	for _, r := range path {
		if r >= 'A' && r <= 'Z' {
			b.WriteRune('!')
			b.WriteRune(r + 32)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
