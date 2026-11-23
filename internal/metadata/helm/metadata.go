// Package helm transforms Helm chart repository indexes without storage or
// network access.
package helm

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/proxy/internal/packageurl"
)

const (
	ecosystem = "helm"
	// IndexFilename is the name of a repository's index.
	IndexFilename   = "index.yaml"
	sha256HexLength = 64
)

// CooldownPolicy evaluates version age at the caller's supplied time.
type CooldownPolicy interface {
	Enabled() bool
	Evaluate(ecosystem, packagePURL string, publishedAt, evaluatedAt time.Time) cooldown.Decision
}

// OCIRegistry is a registry whose chart references the proxy serves through
// its /v2 routes, under Prefix ("" for the default registry, else
// /upstream/{name}).
type OCIRegistry struct {
	Host   string
	Prefix string
}

// Options supplies the public URL, repository, policy and OCI registries
// for a rewrite. EvaluatedAt is used as given; the transformation never
// reads the clock.
type Options struct {
	ProxyURL string
	// Repository is the configured repository name the index is served
	// under, and UpstreamURL the repository's upstream URL, which relative
	// chart URLs resolve against.
	Repository  string
	UpstreamURL string
	// OCIRegistries are the registries whose OCI chart references point at
	// the proxy; other references stay as they are.
	OCIRegistries []OCIRegistry
	Cooldown      CooldownPolicy
	EvaluatedAt   time.Time
	// OnMalformed is called synchronously for each release left out because
	// it can't be read.
	OnMalformed func(chart string, err error)
}

// Rewrite points the index's chart URLs at the proxy and drops releases in
// cooldown or that can't be read. It also returns the releases it kept, as
// chart@digest, which together with the index fix its output.
func Rewrite(body []byte, o Options) ([]byte, []string, error) {
	document, entries, err := ParseIndex(body)
	if err != nil {
		return nil, nil, err
	}

	var kept []string
	for i := 0; i < len(entries.Content); i += 2 {
		chartName := entries.Content[i].Value
		releases := entries.Content[i+1]
		if releases.Kind != yaml.SequenceNode {
			return nil, nil, fmt.Errorf("chart %q releases must be a sequence", chartName)
		}

		filtered := make([]*yaml.Node, 0, len(releases.Content))
		for _, release := range releases.Content {
			chart, err := parseRelease(chartName, o.UpstreamURL, release)
			if err != nil {
				if o.OnMalformed != nil {
					o.OnMalformed(chartName, err)
				}
				continue
			}
			if inCooldown(o.Cooldown, chartName, chart.created, o.EvaluatedAt) {
				continue
			}
			for _, download := range chart.downloads {
				if download.OCI {
					download.node.Value = OCIProxyReference(o.ProxyURL, o.OCIRegistries, download.URL)
				} else {
					download.node.Value = ChartProxyURL(o.ProxyURL, o.Repository, chart.digest, download.Filename)
				}
			}
			filtered = append(filtered, release)
			kept = append(kept, chartName+"@"+chart.digest)
		}
		releases.Content = filtered
	}

	out, err := yaml.Marshal(document)
	return out, kept, err
}

// inCooldown reports whether policy holds back a chart release created at
// created, evaluated at evaluatedAt. A release without a creation time is
// let through.
func inCooldown(policy CooldownPolicy, chartName string, created, evaluatedAt time.Time) bool {
	if created.IsZero() || policy == nil || !policy.Enabled() {
		return false
	}
	return !policy.Evaluate(ecosystem, packageurl.MakeString(ecosystem, chartName, ""), created, evaluatedAt).Allowed
}

// ChartProxyURL is where the proxy serves an HTTP chart download of the
// repository.
func ChartProxyURL(proxyURL, repository, digest, filename string) string {
	return fmt.Sprintf("%s/helm/%s/charts/%s/%s", proxyURL,
		url.PathEscape(repository), digest, url.PathEscape(filename))
}

// OCIProxyReference returns an OCI chart reference pointing at the proxy's
// /v2 routes when its registry is one of registries, and reference as it is
// otherwise or when proxyURL has a path, which OCI clients can't use.
func OCIProxyReference(proxyURL string, registries []OCIRegistry, reference string) string {
	u, err := url.Parse(reference)
	if err != nil {
		return reference
	}
	proxy, err := url.Parse(proxyURL)
	// OCI clients place /v2 before the repository path, so a path-prefixed
	// public HTTP base URL cannot be represented by simply prepending its path.
	if err != nil || proxy.Host == "" || strings.Trim(proxy.Path, "/") != "" {
		return reference
	}
	for _, registry := range registries {
		if !strings.EqualFold(u.Host, registry.Host) {
			continue
		}
		if registry.Prefix == "" && strings.HasPrefix(u.Path, "/upstream/") {
			// This prefix is reserved by the named-registry router.
			return reference
		}
		return "oci://" + proxy.Host + registry.Prefix + u.EscapedPath()
	}
	return reference
}

// Lookup holds the HTTP chart downloads an index lists, by chart digest, in
// index order, and each release by chart name and version. IndexDigest is
// left for the caller to record which index it was built from.
type Lookup struct {
	IndexDigest string
	ByDigest    map[string][]Entry
	ByRelease   map[Release]ReleaseDownload
}

// Entry is one release listed under a digest.
type Entry struct {
	Name      string
	Created   time.Time
	Downloads []Download
}

// Release names a chart release.
type Release struct{ Chart, Version string }

// ReleaseDownload is what helm downloads for a release: the first URL the
// index lists for it, checked against its digest.
type ReleaseDownload struct {
	Digest  string
	Created time.Time
	First   Download
}

// Download is one URL a release lists: an HTTP download with its filename,
// or an OCI reference.
type Download struct {
	URL      string
	Filename string
	OCI      bool
}

// BuildLookup reads the charts an index lists, resolving relative URLs
// against upstreamURL. Releases that can't be read are left out.
func BuildLookup(upstreamURL string, body []byte) (*Lookup, error) {
	_, entries, err := ParseIndex(body)
	if err != nil {
		return nil, err
	}

	lookup := &Lookup{ByDigest: make(map[string][]Entry), ByRelease: make(map[Release]ReleaseDownload)}
	for i := 0; i < len(entries.Content); i += 2 {
		chartName := entries.Content[i].Value
		releases := entries.Content[i+1]
		if releases.Kind != yaml.SequenceNode {
			return nil, fmt.Errorf("chart %q releases must be a sequence", chartName)
		}
		for _, release := range releases.Content {
			chart, err := parseRelease(chartName, upstreamURL, release)
			if err != nil {
				continue
			}
			lookup.add(chartName, release, chart)
		}
	}
	return lookup, nil
}

func (l *Lookup) add(chartName string, release *yaml.Node, chart chartRelease) {
	entry := Entry{Name: chartName, Created: chart.created}
	if version := MappingValue(release, "version"); version != nil && version.Value != "" {
		// helm takes the first release listed for a version.
		key := Release{chartName, version.Value}
		if _, listed := l.ByRelease[key]; !listed {
			l.ByRelease[key] = ReleaseDownload{
				Digest:  chart.digest,
				Created: chart.created,
				First:   chart.downloads[0].Download,
			}
		}
	}
	for _, download := range chart.downloads {
		if !download.OCI {
			// Copy without the YAML node, so the lookup doesn't keep the
			// parsed index alive.
			entry.Downloads = append(entry.Downloads, download.Download)
		}
	}
	l.ByDigest[chart.digest] = append(l.ByDigest[chart.digest], entry)
}

// Lists reports whether the index lists an HTTP download of filename with
// the digest.
func (l *Lookup) Lists(digest, filename string) bool {
	for _, chart := range l.ByDigest[digest] {
		for _, download := range chart.Downloads {
			if download.Filename == filename {
				return true
			}
		}
	}
	return false
}

// download is a Download with the index node its URL came from.
type download struct {
	Download
	node *yaml.Node
}

type chartRelease struct {
	created   time.Time
	digest    string
	downloads []download
}

func parseRelease(chartName, upstreamURL string, release *yaml.Node) (chartRelease, error) {
	digestNode := MappingValue(release, "digest")
	urlsNode := MappingValue(release, "urls")
	if digestNode == nil || urlsNode == nil || urlsNode.Kind != yaml.SequenceNode || len(urlsNode.Content) == 0 {
		return chartRelease{}, fmt.Errorf("chart %q has no digest or URLs", chartName)
	}
	digest, ok := NormalizeDigest(digestNode.Value)
	if !ok {
		return chartRelease{}, fmt.Errorf("chart %q has invalid digest", chartName)
	}

	baseURL, err := url.Parse(upstreamURL + "/" + IndexFilename)
	if err != nil {
		return chartRelease{}, fmt.Errorf("parsing Helm repository URL: %w", err)
	}

	chart := chartRelease{digest: digest}
	if createdNode := MappingValue(release, "created"); createdNode != nil && createdNode.Value != "" {
		chart.created, err = time.Parse(time.RFC3339Nano, createdNode.Value)
		if err != nil {
			return chartRelease{}, fmt.Errorf("chart %q has invalid creation time: %w", chartName, err)
		}
	}

	for _, urlNode := range urlsNode.Content {
		d, err := parseDownload(chartName, baseURL, urlNode)
		if err != nil {
			return chartRelease{}, err
		}
		chart.downloads = append(chart.downloads, d)
	}
	return chart, nil
}

func parseDownload(chartName string, baseURL *url.URL, node *yaml.Node) (download, error) {
	if node.Kind != yaml.ScalarNode {
		return download{}, fmt.Errorf("chart %q has invalid URL", chartName)
	}
	reference, err := url.Parse(node.Value)
	if err != nil {
		return download{}, fmt.Errorf("parsing chart %q URL: %w", chartName, err)
	}
	if reference.Scheme == "oci" {
		if reference.Hostname() == "" || strings.Trim(reference.Path, "/") == "" ||
			reference.User != nil || reference.RawQuery != "" || reference.ForceQuery || reference.Fragment != "" ||
			containsPathTraversal(reference.Path) {
			return download{}, fmt.Errorf("chart %q has invalid OCI reference", chartName)
		}
		return download{Download: Download{URL: node.Value, OCI: true}, node: node}, nil
	}
	downloadURL := baseURL.ResolveReference(reference)
	if (downloadURL.Scheme != "http" && downloadURL.Scheme != "https") || downloadURL.Host == "" {
		return download{}, fmt.Errorf("chart %q URL must be HTTP(S) or OCI", chartName)
	}
	filename := path.Base(downloadURL.Path)
	if filename == "." || filename == "/" || filename == "" || !strings.HasSuffix(filename, ".tgz") {
		return download{}, fmt.Errorf("chart %q URL must point to a .tgz file", chartName)
	}
	return download{Download: Download{URL: downloadURL.String(), Filename: filename}, node: node}, nil
}

// containsPathTraversal reports whether path, or its unescaped form, has a
// ".." segment, as the handler's check of the same name does.
func containsPathTraversal(p string) bool {
	if hasDotDotSegment(p) {
		return true
	}
	if decoded, err := url.PathUnescape(p); err == nil && decoded != p {
		return hasDotDotSegment(decoded)
	}
	return false
}

func hasDotDotSegment(p string) bool {
	for segment := range strings.SplitSeq(strings.ReplaceAll(p, "\\", "/"), "/") {
		if segment == ".." {
			return true
		}
	}
	return false
}

// ParseIndex parses an index, returning the document and its entries
// mapping.
func ParseIndex(body []byte) (*yaml.Node, *yaml.Node, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(body, &document); err != nil {
		return nil, nil, fmt.Errorf("parsing Helm index: %w", err)
	}
	entries, err := IndexEntries(&document)
	if err != nil {
		return nil, nil, err
	}
	return &document, entries, nil
}

// IndexEntries returns a parsed index's entries mapping.
func IndexEntries(document *yaml.Node) (*yaml.Node, error) {
	if document == nil {
		return nil, errors.New("helm index is empty")
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("helm index must be a mapping")
	}
	entries := MappingValue(document.Content[0], "entries")
	if entries == nil || entries.Kind != yaml.MappingNode {
		return nil, errors.New("helm index has no entries mapping")
	}
	if len(entries.Content)%2 != 0 {
		return nil, errors.New("helm index entries mapping has an incomplete key-value pair")
	}
	return entries, nil
}

// MappingValue returns the value under key in a YAML mapping, nil when
// mapping isn't one or has no such key.
func MappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// NormalizeDigest returns a chart digest as lowercase hex without its
// sha256: prefix, and whether it is one.
func NormalizeDigest(value string) (string, bool) {
	digest := strings.TrimPrefix(strings.ToLower(value), "sha256:")
	if len(digest) != sha256HexLength {
		return "", false
	}
	for _, char := range digest {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return "", false
		}
	}
	return digest, true
}
