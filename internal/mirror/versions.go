package mirror

import (
	"context"

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

// ListVersions implements VersionLister.
func (r RegistryVersions) ListVersions(ctx context.Context, ecosystem, name string) ([]string, error) {
	purlType := purl.EcosystemToPURLType(ecosystem)
	var baseURL string
	if r.URL != nil {
		var err error
		if baseURL, err = r.URL(purlType, name); err != nil {
			return nil, err
		}
	}
	client := r.Client
	if client == nil {
		client = registries.DefaultClient()
	}
	reg, err := registries.New(purlType, baseURL, client)
	if err != nil {
		return nil, err
	}
	versions, err := reg.FetchVersions(ctx, name)
	if err != nil {
		return nil, err
	}
	result := make([]string, len(versions))
	for i, v := range versions {
		result[i] = v.Number
	}
	return result, nil
}
