package handler

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

var nugetRegistrationPrefixes = []string{
	"/v3/registration5-semver1/",
	"/v3/registration5-gz-semver1/",
	"/v3/registration5-gz-semver2/",
}

const nugetRegistrationPath = "/v3/registration5-gz-semver2/"

func (h *NuGetHandler) cooldownEnabled() bool {
	return h.proxy.Cooldown != nil && h.proxy.Cooldown.Enabled()
}

func (h *NuGetHandler) nugetCooldownApplies(id string) bool {
	return h.cooldownEnabled() && h.proxy.Cooldown.For("nuget", canonicalPackagePURL("nuget", strings.ToLower(id))) > 0
}

// Cache upstream documents, not filtered results, so policy changes and elapsed
// time take effect even while metadata is fresh. Include the upstream in the key.
func (h *NuGetHandler) nugetMetadata(ctx context.Context, path string) (map[string]any, error) {
	target := h.upstreamURL + path
	key := fmt.Sprintf("_cooldown/%x", sha256.Sum256([]byte(target)))
	var document map[string]any
	validate := func(body []byte) error {
		var err error
		document, err = h.decodeNuGetMetadata(body)
		return err
	}
	_, _, err := h.proxy.fetchOrCacheMetadata(ctx, "nuget", key, target, false, validate)
	if err != nil {
		return nil, err
	}
	return document, nil
}

func (h *NuGetHandler) decodeNuGetMetadata(body []byte) (map[string]any, error) {
	// Normally the HTTP transport decodes gzip. Also support compressed cached
	// bytes and clients with transparent decompression disabled, with the same
	// metadata limit applied to the decompressed document.
	if bytes.HasPrefix(body, []byte{0x1f, 0x8b}) {
		reader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		defer func() { _ = reader.Close() }()
		body, err = h.proxy.ReadMetadata(reader)
		if err != nil {
			return nil, err
		}
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, fmt.Errorf("parsing NuGet metadata: %w", err)
	}
	if document == nil {
		return nil, fmt.Errorf("empty NuGet metadata")
	}
	return document, nil
}

// Prefer semver2, but a configured source may advertise only an older hive.
// Retry only advertised aliases on 404; transport/validation errors must not
// silently switch to a hive with less complete metadata. Keep requests on the
// configured upstream, consistent with the service-index route rewriting.
func (h *NuGetHandler) nugetRegistrationMetadata(ctx context.Context, suffix string) (map[string]any, string, error) {
	path := nugetRegistrationPath + suffix
	document, err := h.nugetMetadata(ctx, path)
	if !errors.Is(err, ErrUpstreamNotFound) {
		return document, path, err
	}
	index, indexErr := h.nugetMetadata(ctx, "/v3/index.json")
	if indexErr != nil {
		return nil, path, indexErr
	}
	resources, _ := index["resources"].([]any)
	seen := map[string]bool{nugetRegistrationPath: true}
	for _, resource := range resources {
		entry, _ := resource.(map[string]any)
		service, _ := entry["@type"].(string)
		id, _ := entry["@id"].(string)
		if id == "" || !strings.HasPrefix(service, "RegistrationsBaseUrl") {
			continue
		}
		prefix := strings.TrimPrefix(h.rewriteNuGetURL(id, service), h.proxyURL+"/nuget")
		if !slices.Contains(nugetRegistrationPrefixes, prefix) || seen[prefix] {
			continue
		}
		seen[prefix] = true
		path = prefix + suffix
		document, err = h.nugetMetadata(ctx, path)
		if !errors.Is(err, ErrUpstreamNotFound) {
			return document, path, err
		}
	}
	return nil, path, err
}

func (h *NuGetHandler) nugetMetadataError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrUpstreamNotFound) {
		JSONError(w, http.StatusNotFound, "package metadata not found")
		return
	}
	h.proxy.Logger.Warn("failed to process NuGet metadata", "error", err)
	JSONError(w, http.StatusBadGateway, "failed to process package metadata")
}

func (h *NuGetHandler) handleVersionList(w http.ResponseWriter, r *http.Request) {
	if !h.cooldownEnabled() {
		h.proxyUpstream(w, r)
		return
	}
	id := strings.ToLower(r.PathValue("id"))
	document, err := h.nugetMetadata(r.Context(), "/v3-flatcontainer/"+url.PathEscape(id)+"/index.json")
	if err != nil {
		h.nugetMetadataError(w, err)
		return
	}
	blocked := make(map[string]bool)
	// A globally enabled policy may still exempt this package or ecosystem.
	// Keep metadata caching, but do not require publication data in that case.
	if h.nugetCooldownApplies(id) {
		registration, registrationPath, err := h.nugetRegistrationMetadata(r.Context(), url.PathEscape(id)+"/index.json")
		if err == nil {
			err = h.expandNuGetPages(r.Context(), registration, registrationPath)
		}
		if err != nil {
			h.nugetMetadataError(w, err)
			return
		}
		h.collectNuGetBlockedVersions(registration, id, blocked)
	}
	versions, ok := document["versions"].([]any)
	if !ok {
		h.nugetMetadataError(w, fmt.Errorf("missing NuGet versions"))
		return
	}
	filtered := make([]any, 0, len(versions))
	for _, value := range versions {
		version, ok := value.(string)
		if !ok {
			h.nugetMetadataError(w, fmt.Errorf("invalid NuGet version"))
			return
		}
		if !blocked[nugetVersionKey(version)] {
			filtered = append(filtered, value)
		}
	}
	document["versions"] = filtered
	w.Header().Set(headerContentType, contentTypeJSON)
	_ = json.NewEncoder(w).Encode(document)
}

func nugetVersionKey(version string) string {
	version, _, _ = strings.Cut(version, "+")
	return strings.ToLower(version)
}

func (h *NuGetHandler) nugetDownloadAllowed(ctx context.Context, id, version string) (bool, error) {
	if !h.nugetCooldownApplies(id) {
		return true, nil
	}
	suffix := url.PathEscape(strings.ToLower(id)) + "/" + url.PathEscape(nugetVersionKey(version)) + ".json"
	leaf, _, err := h.nugetRegistrationMetadata(ctx, suffix)
	if err != nil {
		return false, err
	}
	return h.nugetLeafAllowed(leaf, id), nil
}

// A standalone leaf has published at its root; leaves embedded in pages carry
// it in catalogEntry. Missing/invalid timestamps retain the existing permissive
// behavior, but fetch and JSON errors must not bypass the policy.
func (h *NuGetHandler) nugetLeafAllowed(leaf map[string]any, id string) bool {
	if !h.cooldownEnabled() {
		return true
	}
	entry := nugetCatalogEntry(leaf)
	if id == "" {
		id, _ = entry["id"].(string)
	}
	published, _ := entry["published"].(string)
	when, err := time.Parse(time.RFC3339, published)
	if err != nil {
		return true
	}
	return h.proxy.Cooldown.IsAllowed("nuget", canonicalPackagePURL("nuget", strings.ToLower(id)), when)
}

func nugetCatalogEntry(leaf map[string]any) map[string]any {
	if entry, ok := leaf["catalogEntry"].(map[string]any); ok {
		return entry
	}
	return leaf
}

func (h *NuGetHandler) collectNuGetBlockedVersions(document map[string]any, id string, blocked map[string]bool) {
	entry := nugetCatalogEntry(document)
	if version, ok := entry["version"].(string); ok && !h.nugetLeafAllowed(document, id) {
		blocked[nugetVersionKey(version)] = true
	}
	items, _ := document["items"].([]any)
	for _, item := range items {
		if child, ok := item.(map[string]any); ok {
			h.collectNuGetBlockedVersions(child, id, blocked)
		}
	}
}

func (h *NuGetHandler) handleRegistration(w http.ResponseWriter, r *http.Request) {
	if !h.cooldownEnabled() {
		h.proxyUpstream(w, r)
		return
	}
	id := nugetRegistrationID(r.URL.Path)
	applyCooldown := h.nugetCooldownApplies(id)
	document, err := h.nugetMetadata(r.Context(), r.URL.Path)
	if err == nil && applyCooldown {
		err = h.expandNuGetPages(r.Context(), document, r.URL.Path)
	}
	if err != nil {
		h.nugetMetadataError(w, err)
		return
	}
	_, hasItems := document["items"]
	if applyCooldown && !h.filterNuGetRegistration(document, id) && !hasItems {
		JSONError(w, http.StatusNotFound, "version not found")
		return
	}
	h.rewriteNuGetRegistrationLinks(document)
	w.Header().Set(headerContentType, contentTypeJSON)
	_ = json.NewEncoder(w).Encode(document)
}

func nugetRegistrationID(path string) string {
	for _, prefix := range nugetRegistrationPrefixes {
		if rest, ok := strings.CutPrefix(path, prefix); ok {
			id, _, _ := strings.Cut(rest, "/")
			return id
		}
	}
	return ""
}

// Only expand index pages, never recursively follow arbitrary upstream links.
// Pin requests to this configured upstream and the current package's page path.
func (h *NuGetHandler) expandNuGetPages(ctx context.Context, document map[string]any, path string) error {
	if !strings.HasSuffix(path, "/index.json") {
		return nil
	}
	items, ok := document["items"].([]any)
	if !ok {
		return fmt.Errorf("missing registration pages")
	}
	base, err := url.Parse(h.upstreamURL + path)
	if err != nil {
		return err
	}
	pagePrefix := strings.TrimSuffix(base.Path, "index.json") + "page/"
	for _, item := range items {
		page, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("invalid registration page")
		}
		if _, ok := page["items"].([]any); ok {
			continue
		}
		link, _ := page["@id"].(string)
		target, err := base.Parse(link)
		if err != nil || target.Scheme != base.Scheme || target.Host != base.Host ||
			!strings.HasPrefix(target.Path, pagePrefix) || containsPathTraversal(target.Path) || target.RawQuery != "" || target.Fragment != "" {
			return fmt.Errorf("invalid registration page URL: %q", link)
		}
		upstream, _ := url.Parse(h.upstreamURL)
		pageDocument, err := h.nugetMetadata(ctx, strings.TrimPrefix(target.Path, upstream.Path))
		if err != nil {
			return err
		}
		leaves, ok := pageDocument["items"].([]any)
		if !ok {
			return fmt.Errorf("missing registration leaves")
		}
		page["items"] = leaves
	}
	return nil
}

func (h *NuGetHandler) applyCooldownFiltering(body []byte) ([]byte, error) {
	if !h.cooldownEnabled() {
		return body, nil
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, err
	}
	h.filterNuGetRegistration(document, "")
	return json.Marshal(document)
}

func (h *NuGetHandler) filterNuGetRegistration(document map[string]any, id string) bool {
	items, ok := document["items"].([]any)
	if !ok {
		return h.nugetLeafAllowed(document, id)
	}
	filtered := make([]any, 0, len(items))
	for _, item := range items {
		child, ok := item.(map[string]any)
		if ok && h.filterNuGetRegistration(child, id) {
			filtered = append(filtered, child)
		}
	}
	document["items"] = filtered
	document["count"] = len(filtered)
	// Page bounds describe the retained leaves, not versions hidden by cooldown.
	if _, isPage := document["lower"]; isPage && len(filtered) > 0 {
		first, _ := filtered[0].(map[string]any)
		last, _ := filtered[len(filtered)-1].(map[string]any)
		document["lower"] = nugetCatalogEntry(first)["version"]
		document["upper"] = nugetCatalogEntry(last)["version"]
	}
	return len(filtered) > 0
}

func (h *NuGetHandler) rewriteNuGetRegistrationLinks(value any) {
	switch node := value.(type) {
	case map[string]any:
		for key, child := range node {
			if link, ok := child.(string); ok {
				switch key {
				case "@id", "parent", "registration", "packageContent":
					node[key] = h.nugetProxyLink(link)
				}
			} else {
				h.rewriteNuGetRegistrationLinks(child)
			}
		}
	case []any:
		for _, child := range node {
			h.rewriteNuGetRegistrationLinks(child)
		}
	}
}

func (h *NuGetHandler) nugetProxyLink(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return link
	}
	upstream, err := url.Parse(h.upstreamURL)
	if err != nil {
		return link
	}
	path := u.Path
	if u.Host == upstream.Host {
		path = strings.TrimPrefix(path, upstream.Path)
	}
	for _, prefix := range append([]string{"/v3-flatcontainer/"}, nugetRegistrationPrefixes...) {
		if strings.HasPrefix(path, prefix) {
			proxy, err := url.Parse(h.proxyURL + "/nuget" + path)
			if err != nil {
				return link
			}
			proxy.RawQuery = u.RawQuery
			proxy.Fragment = u.Fragment
			return proxy.String()
		}
	}
	return link
}
