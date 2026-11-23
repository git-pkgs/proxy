// Package oci holds the OCI distribution metadata transformations the
// container handler applies: pagination Link rewrites, manifest Accept
// negotiation and the references a manifest names. It takes its inputs
// explicitly and has no dependency on the proxy's storage, database or HTTP
// client, so it builds on its own, for WASI among others.
package oci

import (
	"net/url"
	"regexp"
	"strings"
)

// NamespaceQueryParam is the query parameter that selects a registry by
// namespace on the proxy's /v2 routes.
const NamespaceQueryParam = "ns"

var linkTargetPattern = regexp.MustCompile(`<([^>]*)>`)

// LinkOptions are the URLs a Link rewrite needs: the proxy's public URL, the
// upstream registry the Link came from, the path of the request being
// answered (below /v2), and the ns value it used, if any.
type LinkOptions struct {
	ProxyURL    string
	RegistryURL string
	RequestPath string
	Namespace   string
}

// RewriteLink points the targets of a tag list or referrers Link header that
// lead back into the upstream registry's API at the same endpoint on the
// proxy, keeping their query and the ns route the client is on. Targets on
// other hosts or outside /v2/ are left as they are, and so is the whole
// header when a URL doesn't parse.
func RewriteLink(link string, o LinkOptions) string {
	if link == "" {
		return ""
	}
	upstreamURL, err := url.Parse(o.RegistryURL)
	if err != nil {
		return link
	}
	proxyURL, err := url.Parse(o.ProxyURL)
	if err != nil {
		return link
	}

	return linkTargetPattern.ReplaceAllStringFunc(link, func(target string) string {
		linkURL, err := url.Parse(target[1 : len(target)-1])
		if err != nil {
			return target
		}
		if linkURL.IsAbs() {
			if linkURL.Scheme != upstreamURL.Scheme || linkURL.Host != upstreamURL.Host {
				return target
			}
		} else if linkURL.Host != "" || (linkURL.Path != "" && !strings.HasPrefix(linkURL.Path, "/v2/")) {
			return target
		}
		// Relative registry API links resolve against the current tag-list
		// endpoint. Rebuild them below so named-registry selectors are kept.
		linkURL.Scheme = proxyURL.Scheme
		linkURL.Host = proxyURL.Host
		linkURL.User = proxyURL.User
		linkURL.Path = strings.TrimSuffix(proxyURL.Path, "/") + "/v2" + o.RequestPath
		linkURL.RawPath = ""
		if o.Namespace != "" {
			// Keep follow-up pages on the ns route the client is using.
			query := linkURL.Query()
			query.Set(NamespaceQueryParam, o.Namespace)
			linkURL.RawQuery = query.Encode()
		}
		return "<" + linkURL.String() + ">"
	})
}
