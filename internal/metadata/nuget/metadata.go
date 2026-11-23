// Package nuget holds the NuGet V3 metadata transformations: the service
// index rewrite, the registration copy that points links at the proxy and
// leaves out versions in cooldown, and the flat container version list
// filter. It takes its inputs explicitly and doesn't fetch, cache or log, so
// it builds on its own for targets such as WASI.
package nuget

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/proxy/internal/packageurl"
)

// RegistrationPrefixes are the registration hives the proxy serves.
var RegistrationPrefixes = []string{
	"/v3/registration5-semver1/",
	"/v3/registration5-gz-semver1/",
	"/v3/registration5-gz-semver2/",
}

// ArtifactPrefixes are the paths whose links are pointed at the proxy.
var ArtifactPrefixes = append([]string{"/v3-flatcontainer/"}, RegistrationPrefixes...)

// RegistrationPath is the registration hive read first.
const RegistrationPath = "/v3/registration5-gz-semver2/"

// unlistedYear is the year of the publish time nuget.org reports for an
// unlisted version, whenever it was published.
const unlistedYear = 1900

// CooldownPolicy evaluates version age at the caller's supplied time.
type CooldownPolicy interface {
	Enabled() bool
	Evaluate(ecosystem, packagePURL string, publishedAt, evaluatedAt time.Time) cooldown.Decision
}

// Options supplies the URLs and policy for a transformation. EvaluatedAt is
// used as given; the transformations never read the clock.
type Options struct {
	// UpstreamURL is the configured NuGet API upstream and ProxyURL the
	// proxy's public URL, both without a trailing slash.
	UpstreamURL string
	ProxyURL    string
	Cooldown    CooldownPolicy
	EvaluatedAt time.Time
	// OnWithheld is called synchronously for each version cooldown holds
	// back, with the lowercase id and version downloads are stored under.
	OnWithheld func(id, version string, publishedAt time.Time)
}

// ServiceURL is the proxy URL of a NuGet service by its advertised type.
// Service types the proxy does not handle are returned as origURL.
func ServiceURL(proxyURL, origURL, serviceType string) string {
	switch serviceType {
	case "PackageBaseAddress/3.0.0":
		return proxyURL + "/nuget/v3-flatcontainer/"
	case "RegistrationsBaseUrl", "RegistrationsBaseUrl/3.0.0-beta", "RegistrationsBaseUrl/3.0.0-rc":
		return proxyURL + "/nuget/v3/registration5-semver1/"
	case "RegistrationsBaseUrl/3.4.0":
		return proxyURL + "/nuget/v3/registration5-gz-semver1/"
	case "RegistrationsBaseUrl/3.6.0", "RegistrationsBaseUrl/Versioned":
		return proxyURL + "/nuget/v3/registration5-gz-semver2/"
	case "SearchQueryService", "SearchQueryService/3.0.0-rc", "SearchQueryService/3.5.0":
		return proxyURL + "/nuget/query"
	case "SearchAutocompleteService", "SearchAutocompleteService/3.5.0":
		return proxyURL + "/nuget/autocomplete"
	default:
		return origURL
	}
}

// RewriteServiceIndex points the services of a service index the proxy
// handles at it. The service type decides the route, because an upstream
// index may advertise a different host. An index without resources is
// returned as it is.
func RewriteServiceIndex(body []byte, proxyURL string) ([]byte, error) {
	var index map[string]any
	if err := json.Unmarshal(body, &index); err != nil {
		return nil, err
	}
	resources, ok := index["resources"].([]any)
	if !ok {
		return body, nil
	}
	for _, res := range resources {
		rmap, ok := res.(map[string]any)
		if !ok {
			continue
		}
		id, _ := rmap["@id"].(string)
		rtype, _ := rmap["@type"].(string)
		if id != "" {
			rmap["@id"] = ServiceURL(proxyURL, id, rtype)
		}
	}
	return json.Marshal(index)
}

// DecodeDocument reads a NuGet JSON document as a map, refusing null.
func DecodeDocument(body []byte) (map[string]any, error) {
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, fmt.Errorf("parsing NuGet metadata: %w", err)
	}
	if document == nil {
		return nil, errors.New("empty NuGet metadata")
	}
	return document, nil
}

// VersionKey is a version as downloads are stored under it: lowercase,
// without build metadata.
func VersionKey(version string) string {
	version, _, _ = strings.Cut(version, "+")
	return strings.ToLower(version)
}

// UnlistedTime reports whether published is the time nuget.org reports for
// an unlisted version.
func UnlistedTime(published string) bool {
	when, err := time.Parse(time.RFC3339, published)
	return err == nil && when.Year() == unlistedYear
}

// PublishedAllowed reports whether cooldown lets the version of id published
// at published through. A missing or invalid time is let through, as is
// every version without a policy. A version held back is reported to
// OnWithheld.
func (o Options) PublishedAllowed(id, version, published string) bool {
	when, err := time.Parse(time.RFC3339, published)
	if err != nil || o.Cooldown == nil {
		return true
	}
	name := strings.ToLower(id)
	if o.Cooldown.Evaluate("nuget", packageurl.MakeString("nuget", name, ""), when, o.EvaluatedAt).Allowed {
		return true
	}
	if o.OnWithheld != nil {
		o.OnWithheld(name, VersionKey(version), when)
	}
	return false
}

// CollectBlocked adds the versions of document, a registration index, page
// or leaf, that cooldown holds back to blocked, by VersionKey.
func (o Options) CollectBlocked(document *Times, id string, blocked map[string]bool) {
	if version, published := document.Release(); version != "" && !o.PublishedAllowed(id, version, published) {
		blocked[VersionKey(version)] = true
	}
	for i := range document.Items {
		o.CollectBlocked(&document.Items[i], id, blocked)
	}
}

// FilterVersions removes the versions in blocked, by VersionKey, from the
// versions of a flat container version list.
func FilterVersions(document map[string]any, blocked map[string]bool) error {
	versions, ok := document["versions"].([]any)
	if !ok {
		return errors.New("missing NuGet versions")
	}
	filtered := make([]any, 0, len(versions))
	for _, value := range versions {
		version, ok := value.(string)
		if !ok {
			return errors.New("invalid NuGet version")
		}
		if !blocked[VersionKey(version)] {
			filtered = append(filtered, value)
		}
	}
	document["versions"] = filtered
	return nil
}

// RegistrationID is the package id a registration path names.
func RegistrationID(path string) string {
	for _, prefix := range RegistrationPrefixes {
		if rest, ok := strings.CutPrefix(path, prefix); ok {
			id, _, _ := strings.Cut(rest, "/")
			return id
		}
	}
	return ""
}

// RegistrationLeafVersion returns the version a registration leaf's path
// names ({prefix}{id}/{version}.json), or "" for any other path. A leaf as
// nuget.org serves it carries no version of its own.
func RegistrationLeafVersion(path string) string {
	for _, prefix := range RegistrationPrefixes {
		rest, ok := strings.CutPrefix(path, prefix)
		if !ok {
			continue
		}
		_, leaf, _ := strings.Cut(rest, "/")
		version, isJSON := strings.CutSuffix(leaf, ".json")
		if !isJSON || version == "" || version == "index" || strings.Contains(version, "/") {
			return ""
		}
		return version
	}
	return ""
}

// ProxyLink is link pointing at the proxy when it names a NuGet resource,
// on the upstream or with the path of one. Other links are returned as
// they are.
func (o Options) ProxyLink(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return link
	}
	upstream, err := url.Parse(o.UpstreamURL)
	if err != nil {
		return link
	}
	path := u.Path
	if u.Host == upstream.Host {
		path = strings.TrimPrefix(path, upstream.Path)
	}
	for _, prefix := range ArtifactPrefixes {
		if strings.HasPrefix(path, prefix) {
			proxy, err := url.Parse(o.ProxyURL + "/nuget" + path)
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

// CatalogPath returns the path below the upstream of a catalog entry link,
// and whether the link is one into the upstream.
func (o Options) CatalogPath(link string) (string, bool) {
	upstream, err := url.Parse(o.UpstreamURL)
	if err != nil {
		return "", false
	}
	target, err := url.Parse(link)
	if err != nil || target.Scheme != upstream.Scheme || target.Host != upstream.Host ||
		!strings.HasPrefix(target.Path, upstream.Path+"/") || containsPathTraversal(target.Path) ||
		target.RawQuery != "" || target.Fragment != "" {
		return "", false
	}
	return strings.TrimPrefix(target.Path, upstream.Path), true
}

// RegistrationPagePath returns the upstream path of a page that the
// registration index at indexPath links to. Only pages of that index, on
// the upstream, are accepted.
func (o Options) RegistrationPagePath(indexPath, link string) (string, error) {
	base, err := url.Parse(o.UpstreamURL + indexPath)
	if err != nil {
		return "", err
	}
	pagePrefix := strings.TrimSuffix(base.Path, "index.json") + "page/"
	target, err := base.Parse(link)
	if err != nil || target.Scheme != base.Scheme || target.Host != base.Host ||
		!strings.HasPrefix(target.Path, pagePrefix) || containsPathTraversal(target.Path) || target.RawQuery != "" || target.Fragment != "" {
		return "", fmt.Errorf("invalid registration page URL: %q", link)
	}
	upstream, _ := url.Parse(o.UpstreamURL)
	return strings.TrimPrefix(target.Path, upstream.Path), nil
}

// containsPathTraversal reports whether path, or path unescaped, has a ".."
// segment, as the handler's check of the same name does.
func containsPathTraversal(path string) bool {
	if hasDotDotSegment(path) {
		return true
	}
	if decoded, err := url.PathUnescape(path); err == nil && decoded != path {
		return hasDotDotSegment(decoded)
	}
	return false
}

func hasDotDotSegment(path string) bool {
	path = strings.ReplaceAll(path, "\\", "/")
	for segment := range strings.SplitSeq(path, "/") {
		if segment == ".." {
			return true
		}
	}
	return false
}
