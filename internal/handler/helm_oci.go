package handler

import (
	"net/url"
	"slices"
	"strings"
)

type helmOCIRegistry struct {
	host   string
	prefix string
}

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
	h.ociRegistries = append(h.ociRegistries, helmOCIRegistry{host: u.Host, prefix: prefix})
}

func (h *HelmHandler) ociChartProxyURL(reference string) string {
	u, err := url.Parse(reference)
	if err != nil {
		return reference
	}
	proxy, err := url.Parse(h.proxyURL)
	// OCI clients place /v2 before the repository path, so a path-prefixed
	// public HTTP base URL cannot be represented by simply prepending its path.
	if err != nil || proxy.Host == "" || strings.Trim(proxy.Path, "/") != "" {
		return reference
	}
	for _, registry := range h.ociRegistries {
		if !strings.EqualFold(u.Host, registry.host) {
			continue
		}
		if registry.prefix == "" && strings.HasPrefix(u.Path, "/upstream/") {
			// This prefix is reserved by the named-registry router.
			return reference
		}
		return "oci://" + proxy.Host + registry.prefix + u.EscapedPath()
	}
	return reference
}
