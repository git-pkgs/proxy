package handler

import (
	"net/url"
	"slices"
	"strings"

	"github.com/git-pkgs/proxy/internal/metadata/helm"
)

// helmOCIRegistry is a registry whose chart references the index rewrite
// points at the /v2 routes.
type helmOCIRegistry = helm.OCIRegistry

// NewHelmHandlerWithOCIRegistries also rewrites OCI references for configured
// registries through the existing /v2 routes. Unmatched references stay intact.
func NewHelmHandlerWithOCIRegistries(proxy *Proxy, proxyURL string, repositories map[string]string, defaultRegistry string, registries map[string]string) *HelmHandler {
	h := NewHelmHandler(proxy, proxyURL, repositories)
	// Prefer named routes; use a stable order when aliases share a registry.
	names := make([]string, 0, len(registries))
	for name := range registries {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		h.addOCIRegistry(registries[name], "/upstream/"+url.PathEscape(name))
	}
	h.addOCIRegistry(configuredUpstreamURL(defaultRegistry, dockerHubRegistry), "")
	return h
}

func (h *HelmHandler) addOCIRegistry(rawURL, prefix string) {
	u, err := url.Parse(rawURL)
	// A path-prefixed upstream is an API mount, not an OCI repository prefix;
	// its mapping cannot be inferred from a chart's registry authority alone.
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return
	}
	h.ociRegistries = append(h.ociRegistries, helmOCIRegistry{Host: u.Host, Prefix: prefix})
}

func (h *HelmHandler) ociChartProxyURL(reference string) string {
	return helm.OCIProxyReference(h.proxyURL, h.ociRegistries, reference)
}
