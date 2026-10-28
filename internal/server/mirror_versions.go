package server

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/mirror"
	"github.com/git-pkgs/registries"
)

// NewMirrorVersions lists versions for proxy mirror from the configured
// upstreams, through client, so upstream.auth and the private address rules
// apply as they do when serving.
func NewMirrorVersions(upstream config.UpstreamConfig, client *http.Client) mirror.VersionLister { //nolint:ireturn // the mirror takes any lister
	return mirror.RegistryVersions{
		Client: registries.NewClient(registries.WithHTTPClient(client)),
		URL: func(purlType, name string) (string, error) {
			return mirrorVersionsURL(upstream, purlType, name)
		},
	}
}

// newMirror builds the mirror for mirror API jobs and schedules, which fetch
// and list versions from the configured upstreams.
func (s *Server) newMirror(proxy *handler.Proxy, workers int) *mirror.Mirror {
	m := mirror.New(proxy, s.db, s.storage, s.logger, workers)
	m.Images = s.newContainerHandler(proxy)
	m.Charts = handler.NewHelmHandler(proxy, s.cfg.BaseURL, s.cfg.Upstream.Helm)
	UseMirrorUpstreams(m, s.cfg.Upstream, proxy.HTTPClient)
	return m
}

// UseMirrorUpstreams points m at the configured upstreams: it lists
// versions from them, through client, and fetches Julia packages from
// upstream.julia.
func UseMirrorUpstreams(m *mirror.Mirror, upstream config.UpstreamConfig, client *http.Client) {
	m.Versions = NewMirrorVersions(upstream, client)
	m.JuliaUpstream = upstream.Julia
}

// mirrorVersionsURL returns the registry base URL to list a package's
// versions from: the upstream the proxy serves it from, where that upstream
// has the API the registries client reads. An empty URL means the public
// registry, used only while the upstream is the public one too.
func mirrorVersionsURL(u config.UpstreamConfig, purlType, name string) (string, error) {
	d := config.Default().Upstream
	switch purlType {
	case "npm":
		if routed, ok := handler.RoutedPackageURL(u.NPMRoutes, name); ok {
			return routed, nil
		}
		return upstreamOr(u.NPM, d.NPM), nil
	case "gem":
		return upstreamOr(u.Gem, d.Gem), nil
	case "golang":
		return upstreamOr(u.Go, d.Go), nil
	case "hex":
		return upstreamOr(u.HexAPI, d.HexAPI), nil
	case "pub":
		return upstreamOr(u.Pub, d.Pub), nil
	case "pypi":
		return upstreamOr(u.PyPI, d.PyPI), nil
	case "maven":
		return upstreamOr(u.Maven, d.Maven), nil
	case "nuget":
		return upstreamOr(u.NuGet, d.NuGet) + "/v3", nil
	case "composer":
		if _, ok := handler.RoutedPackageURL(u.ComposerRoutes, name); ok {
			return "", errors.New("mirroring packages in upstream.composer_routes isn't supported")
		}
		return upstreamOr(u.Composer, d.Composer), nil
	case "cran":
		return upstreamOr(u.CRAN, d.CRAN), nil
	case "brew":
		return strings.TrimSuffix(upstreamOr(u.HomebrewAPI, d.HomebrewAPI), "/api"), nil
	case "cargo":
		return publicOnly("upstream.cargo", u.Cargo, d.Cargo)
	case "conda":
		// Files are listed from anaconda.org even for a given version, so a
		// version doesn't help here.
		if upstreamOr(u.Conda, d.Conda) != strings.TrimSuffix(d.Conda, "/") {
			return "", errors.New("mirroring conda packages from a custom upstream.conda isn't supported")
		}
		return "", nil
	case "julia":
		// Packages are identified by uuid and tree hash, the same on every
		// Pkg server serving General, so General's are used whatever the
		// upstream.
		return "", nil
	case "helm":
		// The namespace names the repository; see RegistryVersions.
		repository, _, ok := strings.Cut(name, "/")
		if !ok {
			return "", errors.New("helm charts need the upstream.helm repository as the namespace, such as pkg:helm/{repository}/{chart}")
		}
		repositoryURL, ok := u.Helm[repository]
		if !ok {
			return "", fmt.Errorf("unknown Helm repository %q: it isn't in upstream.helm", repository)
		}
		return strings.TrimSuffix(repositoryURL, "/"), nil
	case "swift":
		return upstreamOr(u.Swift, d.Swift), nil
	default:
		return "", fmt.Errorf("listing %s versions isn't supported; give a version", purlType)
	}
}

func upstreamOr(configured, fallback string) string {
	return strings.TrimSuffix(cmp.Or(configured, fallback), "/")
}

// publicOnly is for upstreams that don't serve the API the registries
// client reads: versions come from the public registry while the upstream
// is the public one, and can't be listed otherwise.
func publicOnly(setting, configured, fallback string) (string, error) {
	if upstreamOr(configured, fallback) != strings.TrimSuffix(fallback, "/") {
		return "", fmt.Errorf("listing versions from a custom %s isn't supported; give a version", setting)
	}
	return "", nil
}
