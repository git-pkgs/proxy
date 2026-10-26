package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/purl"
	"github.com/git-pkgs/registries"
	_ "github.com/git-pkgs/registries/all"
)

// VersionLister lists the versions of a package, for PURLs without one.
type VersionLister interface {
	ListVersions(ctx context.Context, ecosystem, name string) ([]string, error)
}

// RegistryVersions lists versions through the registries clients.
type RegistryVersions struct {
	// Client makes the requests. Nil uses registries.DefaultClient.
	Client *registries.Client
	// URL returns the registry base URL to list a package from, by PURL
	// type. Nil, or an empty URL, uses the public registry.
	URL func(purlType, name string) (string, error)
}

// VersionFinder finds one version of a package, with the files it
// publishes, for ecosystems the mirror fetches from the version's metadata.
type VersionFinder interface {
	FindVersion(ctx context.Context, ecosystem, name, version string) (registries.Version, error)
}

// ListVersions implements VersionLister.
func (r RegistryVersions) ListVersions(ctx context.Context, ecosystem, name string) ([]string, error) {
	versions, err := r.fetchVersions(ctx, ecosystem, name)
	if err != nil {
		return nil, err
	}
	result := make([]string, len(versions))
	for i, v := range versions {
		result[i] = v.Number
	}
	return result, nil
}

// FindVersion implements VersionFinder.
func (r RegistryVersions) FindVersion(ctx context.Context, ecosystem, name, version string) (registries.Version, error) {
	versions, err := r.fetchVersions(ctx, ecosystem, name)
	if err != nil {
		return registries.Version{}, err
	}
	for _, v := range versions {
		if v.Number == version {
			return v, nil
		}
	}
	return registries.Version{}, fmt.Errorf("%w: %s %s@%s", registries.ErrNotFound, ecosystem, name, version)
}

func (r RegistryVersions) fetchVersions(ctx context.Context, ecosystem, name string) ([]registries.Version, error) {
	reg, err := r.registry(ecosystem, name)
	if err != nil {
		return nil, err
	}
	switch purl.EcosystemToPURLType(ecosystem) {
	case "helm":
		// The namespace names the repository, which URL resolves to the
		// index the chart is listed in.
		_, name, _ = strings.Cut(name, "/")
	case "swift":
		var baseURL string
		if baseURL, err = r.baseURL("swift", name); err != nil {
			return nil, err
		}
		if name, err = swiftIdentity(ctx, r.client(), baseURL, name); err != nil {
			return nil, err
		}
	}
	return reg.FetchVersions(ctx, name)
}

// registry returns the client for the registry name is listed in.
func (r RegistryVersions) registry(ecosystem, name string) (registries.Registry, error) { //nolint:ireturn // registries.New returns the interface
	purlType := purl.EcosystemToPURLType(ecosystem)
	baseURL, err := r.baseURL(purlType, name)
	if err != nil {
		return nil, err
	}
	return registries.New(purlType, baseURL, r.client())
}

func (r RegistryVersions) baseURL(purlType, name string) (string, error) {
	if r.URL == nil {
		return "", nil
	}
	return r.URL(purlType, name)
}

func (r RegistryVersions) client() *registries.Client {
	if r.Client == nil {
		return registries.DefaultClient()
	}
	return r.Client
}

// swiftIdentity returns the registry identity, scope.name, of the name of a
// pkg:swift PURL, looking a source repository up on the registry at baseURL.
func swiftIdentity(ctx context.Context, client *registries.Client, baseURL, name string) (string, error) {
	source := swiftSourceURL(name)
	if source == "" {
		scope, pkg, err := swiftScopeAndName(name)
		if err != nil {
			return "", err
		}
		return scope + "." + pkg, nil
	}
	if baseURL == "" {
		baseURL = config.DefaultSwiftUpstream
	}
	var body json.RawMessage
	_, err := client.GetJSONWithHeaders(ctx,
		strings.TrimSuffix(baseURL, "/")+"/identifiers?"+url.Values{"url": {source}}.Encode(),
		http.Header{"Accept": {"application/vnd.swift.registry.v1+json"}}, &body)
	if httpErr := (*registries.HTTPError)(nil); errors.As(err, &httpErr) && httpErr.IsNotFound() {
		return "", fmt.Errorf("the Swift registry has no package for %s", source)
	}
	if err != nil {
		return "", err
	}
	scope, pkg, err := handler.SwiftIdentityOf(body, source)
	if err != nil {
		return "", err
	}
	return scope + "." + pkg, nil
}
