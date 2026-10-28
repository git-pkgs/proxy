package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// MirrorRelease caches release version of scope.name so that a client can
// resolve and fetch the release without the upstream.
func (h *SwiftHandler) MirrorRelease(ctx context.Context, scope, name, version string) (*CacheResult, error) {
	if !validSwiftPackageReference(scope, name, version) {
		return nil, fmt.Errorf("invalid Swift registry release %s.%s@%s", scope, name, version)
	}
	scope, name = canonicalSwiftPackage(scope, name)
	key, err := swiftSourceArchiveKey(scope, name, version)
	if err != nil {
		return nil, err
	}
	if h.documentsCached() {
		if err := h.mirrorReleaseList(ctx, scope, name); err != nil {
			return nil, fmt.Errorf("fetching release list: %w", err)
		}
		if err := h.mirrorManifests(ctx, scope, name, version); err != nil {
			return nil, fmt.Errorf("fetching manifest: %w", err)
		}
	}
	info, err := h.fetchArchiveInfo(ctx, scope, name, version)
	if err != nil {
		return nil, fmt.Errorf("fetching release metadata: %w", err)
	}
	headers := http.Header{"Accept": {swiftAcceptArchive}}
	return h.fetchSourceArchive(ctx, key, version, h.buildUpstreamURL(scope, name, version+".zip", "", ""), headers, info)
}

// maxSwiftReleasePages bounds the release list pages a mirror follows.
const maxSwiftReleasePages = 100

// mirrorReleaseList caches every page of the release list, under the keys
// the release list route reads for the page links it rewrites.
func (h *SwiftHandler) mirrorReleaseList(ctx context.Context, scope, name string) error {
	query := ""
	seen := map[string]bool{}
	for page := 0; page < maxSwiftReleasePages && !seen[query]; page++ {
		seen[query] = true
		upstreamURL := h.buildUpstreamURL(scope, name, "", "", query)
		doc, err := h.fetchDocument(ctx, swiftReleasesCacheKey(scope, name, query), upstreamURL, swiftAcceptJSON)
		if err != nil {
			return err
		}
		next := swiftLinkQueries(doc.link, "next", upstreamURL)
		if len(next) == 0 {
			return nil
		}
		query = next[0]
	}
	return nil
}

// mirrorManifests caches Package.swift of a release and the version-specific
// manifests its Link header lists, under the keys the manifest route reads
// for the alternate links it rewrites.
func (h *SwiftHandler) mirrorManifests(ctx context.Context, scope, name, version string) error {
	upstreamURL := h.buildUpstreamURL(scope, name, version, "Package.swift", "")
	doc, err := h.fetchDocument(ctx, swiftManifestCacheKey(scope, name, version, ""), upstreamURL, swiftAcceptManifest)
	if err != nil {
		return err
	}
	for _, query := range swiftLinkQueries(doc.link, "alternate", upstreamURL) {
		alternateURL := h.buildUpstreamURL(scope, name, version, "Package.swift", query)
		if _, err := h.fetchDocument(ctx, swiftManifestCacheKey(scope, name, version, query), alternateURL, swiftAcceptManifest); err != nil {
			return fmt.Errorf("%s: %w", query, err)
		}
	}
	return nil
}

// swiftLinkQueries returns the queries of the links with relation rel in
// link, a Link header, that point at requestURL's own path with a query,
// such as the next page of a release list or a version-specific manifest.
func swiftLinkQueries(link, rel, requestURL string) []string {
	base, err := url.Parse(requestURL)
	if err != nil {
		return nil
	}
	var queries []string
	for _, value := range strings.Split(link, ",") {
		target, params, ok := strings.Cut(strings.TrimSpace(value), ";")
		if !ok || !strings.HasPrefix(target, "<") || !strings.HasSuffix(target, ">") || !swiftLinkHasRel(params, rel) {
			continue
		}
		ref, err := url.Parse(strings.TrimSpace(target[1 : len(target)-1]))
		if err != nil {
			continue
		}
		resolved := base.ResolveReference(ref)
		if resolved.Host != base.Host || resolved.EscapedPath() != base.EscapedPath() || resolved.RawQuery == "" {
			continue
		}
		queries = append(queries, resolved.RawQuery)
	}
	return queries
}

func swiftLinkHasRel(params, rel string) bool {
	for _, param := range strings.Split(params, ";") {
		key, value, ok := strings.Cut(strings.TrimSpace(param), "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "rel") {
			continue
		}
		for _, r := range strings.Fields(strings.Trim(strings.TrimSpace(value), `"`)) {
			if strings.EqualFold(r, rel) {
				return true
			}
		}
	}
	return false
}

// ResolveIdentity returns the registry scope and name the upstream lists for
// the source repository at sourceURL. The answer is cached as the identifiers
// route caches it.
func (h *SwiftHandler) ResolveIdentity(ctx context.Context, sourceURL string) (scope, name string, _ error) {
	query := url.Values{"url": {sourceURL}}.Encode()
	body, _, err := h.proxy.FetchOrCacheMetadata(ctx, "swift", swiftMetadataCacheKey("identifiers", query),
		h.upstreamURL+"/identifiers?"+query, swiftAcceptJSON)
	if errors.Is(err, ErrUpstreamNotFound) {
		return "", "", fmt.Errorf("the Swift registry has no package for %s", sourceURL)
	}
	if err != nil {
		return "", "", err
	}
	return SwiftIdentityOf(body, sourceURL)
}

// SwiftIdentityOf returns the scope and name of the first identity in body,
// a registry's answer to an identifiers lookup for sourceURL.
func SwiftIdentityOf(body []byte, sourceURL string) (scope, name string, _ error) {
	var answer struct {
		Identifiers []string `json:"identifiers"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return "", "", fmt.Errorf("parsing the Swift registry's identifiers for %s: %w", sourceURL, err)
	}
	if len(answer.Identifiers) == 0 {
		return "", "", fmt.Errorf("the Swift registry has no package for %s", sourceURL)
	}
	scope, name, ok := strings.Cut(answer.Identifiers[0], ".")
	if !ok || scope == "" || name == "" {
		return "", "", fmt.Errorf("the Swift registry answered %s with the invalid identity %q", sourceURL, answer.Identifiers[0])
	}
	return scope, name, nil
}
