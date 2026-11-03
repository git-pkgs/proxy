package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/git-pkgs/proxy/internal/retention"
)

const (
	helmMetadataEcosystem = "helm"
	helmIndexFilename     = "index.yaml"
	sha256HexLength       = 64
)

// Helm takes no package rules: charts are cached under the configured
// repository's name and the chart digest, not under the chart name, so a key
// like pkg:helm/nginx would name a repository. The helm rule and the default
// cover charts from HTTP repositories; charts pulled over OCI go through the
// container handler and fall under oci.
func init() {
	retention.Default.Register(retention.Spec{Key: "helm"})
}

// HelmHandler serves read-only HTTP Helm chart repositories. Each configured
// repository is mounted at /helm/{repository}/.
type HelmHandler struct {
	proxy         *Proxy
	proxyURL      string
	repositories  map[string]string
	ociRegistries []helmOCIRegistry

	lookupMu sync.Mutex
	// lookups holds the charts of each repository's last parsed index.
	lookups map[string]*helmChartLookup
}

// NewHelmHandler creates a Helm chart repository protocol handler.
func NewHelmHandler(proxy *Proxy, proxyURL string, repositories map[string]string) *HelmHandler {
	h := &HelmHandler{
		proxyURL:     strings.TrimSuffix(proxyURL, "/"),
		repositories: make(map[string]string, len(repositories)),
		proxy:        proxy,
	}
	for name, repositoryURL := range repositories {
		h.repositories[name] = strings.TrimSuffix(repositoryURL, "/")
	}
	return h
}

// Routes returns the HTTP handler for Helm chart repository requests.
func (h *HelmHandler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{repository}/index.yaml", h.handleIndex)
	mux.HandleFunc("GET /{repository}/charts/{digest}/{filename}", h.handleChart)
	return mux
}

func (h *HelmHandler) handleIndex(w http.ResponseWriter, r *http.Request) {
	repository, upstreamURL, ok := h.repositoryForRequest(r)
	if !ok {
		http.NotFound(w, r)
		return
	}

	// Parsing and encoding a large index costs far more than serving it, so
	// rewrites go through the rewrite cache like npm packuments. The cache
	// key holds the repository and upstream URL, both part of cacheKey.
	cacheKey := h.indexCacheKey(repository, upstreamURL)
	if stored, contentType, ok := h.proxy.storedRewriteVariant(helmMetadataEcosystem, cacheKey, h.proxyURL, cacheKey, nil); ok {
		serveRewrittenMetadataAs(w, r, stored, contentType)
		return
	}

	index := h.fetchIndex(r.Context(), repository, upstreamURL)
	if index.err != nil {
		h.serveIndexError(w, index.err)
		return
	}

	rewritten, err := h.proxy.cachedRewrite(r.Context(), helmMetadataEcosystem, h.proxyURL, cacheKey, index.body, index.digest,
		func(body []byte) ([]byte, []string, error) {
			return h.rewriteIndex(repository, upstreamURL, body)
		})
	if err != nil {
		if r.Context().Err() != nil {
			return // the client left while waiting on a shared rewrite
		}
		h.proxy.Logger.Warn("failed to rewrite Helm index", "repository", repository, "error", err)
		http.Error(w, "invalid Helm repository index", http.StatusBadGateway)
		return
	}

	markStale(w, index.stale)
	serveRewrittenMetadataAs(w, r, rewritten, index.contentType)
}

func (h *HelmHandler) handleChart(w http.ResponseWriter, r *http.Request) {
	repository, upstreamURL, ok := h.repositoryForRequest(r)
	if !ok {
		http.NotFound(w, r)
		return
	}

	digest, ok := normalizeHelmDigest(r.PathValue("digest"))
	filename := r.PathValue("filename")
	if !ok || filename == "" || strings.Contains(filename, "/") || containsPathTraversal(filename) {
		http.Error(w, "invalid chart request", http.StatusBadRequest)
		return
	}

	cached, err := h.proxy.GetCachedArtifact(r.Context(), helmMetadataEcosystem, repository, digest, filename)
	if err != nil {
		h.proxy.Logger.Error("failed to check Helm chart cache", "error", err)
		http.Error(w, "failed to check chart cache", http.StatusInternalServerError)
		return
	}
	if cached != nil {
		h.serveChart(w, r, repository, digest, filename, cached)
		return
	}

	index := h.fetchIndex(r.Context(), repository, upstreamURL)
	if index.err != nil {
		h.serveIndexError(w, index.err)
		return
	}

	downloadURL, err := h.findChartDownload(repository, upstreamURL, index, digest, filename)
	if err != nil {
		if errors.Is(err, errHelmChartNotFound) {
			http.NotFound(w, r)
			return
		}
		h.proxy.Logger.Warn("failed to read Helm index", "repository", repository, "error", err)
		http.Error(w, "invalid Helm repository index", http.StatusBadGateway)
		return
	}

	// Verify the chart against the index digest while it is fetched, so a
	// mismatch is never committed to the cache, nor streamed out complete.
	result, err := h.proxy.GetOrFetchArtifactFromURLWithDigest(
		r.Context(), helmMetadataEcosystem, repository, digest, filename, downloadURL, "sha256:"+digest)
	if err != nil {
		h.proxy.serveArtifactError(w, err, "failed to fetch chart")
		return
	}
	h.serveChart(w, r, repository, digest, filename, result)
}

func (h *HelmHandler) serveChart(w http.ResponseWriter, r *http.Request, repository, digest, filename string, result *CacheResult) {
	if !strings.EqualFold(result.Artifact.Digest.Encoded(), digest) {
		if result.Reader != nil {
			_ = result.Reader.Close()
		}
		if clearErr := h.proxy.ClearCachedArtifact(helmMetadataEcosystem, repository, digest, filename); clearErr != nil {
			h.proxy.Logger.Warn("failed to clear Helm chart with invalid digest", "error", clearErr)
		}
		http.Error(w, "chart digest verification failed", http.StatusBadGateway)
		return
	}

	if result.Artifact.MediaType == "" {
		w.Header().Set(headerContentType, "application/gzip")
	}
	ServeArtifactRequest(w, r, result)
}

func (h *HelmHandler) repositoryForRequest(r *http.Request) (name, upstreamURL string, ok bool) {
	name = r.PathValue("repository")
	upstreamURL, ok = h.repositories[name]
	return name, upstreamURL, ok
}

func (h *HelmHandler) fetchIndex(ctx context.Context, repository, upstreamURL string) metadataResult {
	return h.proxy.fetchMetadataDocument(
		ctx,
		helmMetadataEcosystem,
		h.indexCacheKey(repository, upstreamURL),
		upstreamURL+"/"+helmIndexFilename,
		"application/x-yaml, text/yaml;q=0.9, */*;q=0.1",
		nil,
	)
}

func (h *HelmHandler) indexCacheKey(repository, upstreamURL string) string {
	identity := repository + "\x00" + upstreamURL
	digest := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(digest[:])
}

func (h *HelmHandler) serveIndexError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrUpstreamNotFound) {
		http.Error(w, "Helm repository not found", http.StatusNotFound)
		return
	}
	if writeUpstreamHeld(w, err) {
		return
	}
	h.proxy.Logger.Error("failed to fetch Helm index", "error", err)
	http.Error(w, "failed to fetch Helm repository index", http.StatusBadGateway)
}

// rewriteIndex points the index's chart URLs at the proxy and drops releases
// in cooldown. It also returns the releases it kept, by chart name and
// digest, which together with the index fix its output.
func (h *HelmHandler) rewriteIndex(repository, upstreamURL string, body []byte) ([]byte, []string, error) {
	document, entries, err := parseHelmIndex(body)
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
			chart, err := h.parseChartRelease(chartName, upstreamURL, release)
			if err != nil {
				h.proxy.Logger.Warn("omitting malformed Helm chart release",
					"repository", repository, "chart", chartName, "error", err)
				continue
			}
			if h.chartOnCooldown(chartName, chart.created) {
				continue
			}
			for _, download := range chart.downloads {
				if download.oci {
					download.node.Value = h.ociChartProxyURL(download.url)
				} else {
					download.node.Value = h.chartProxyURL(repository, chart.digest, download.filename)
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

// findChartDownload returns the upstream URL of a chart in the index. Parsing
// a large index takes a while, so the charts it lists are kept per
// repository for as long as the stored index stays the same.
func (h *HelmHandler) findChartDownload(repository, upstreamURL string, index metadataResult, digest, filename string) (string, error) {
	charts, err := h.chartLookup(repository, upstreamURL, index)
	if err != nil {
		return "", err
	}
	for _, chart := range charts.byDigest[digest] {
		if h.chartOnCooldown(chart.name, chart.created) {
			continue
		}
		for _, download := range chart.downloads {
			if download.filename == filename {
				return download.url, nil
			}
		}
	}
	return "", errHelmChartNotFound
}

// helmChartLookup holds the HTTP chart downloads an index lists, by chart
// digest, in index order, and each release by chart name and version.
type helmChartLookup struct {
	indexDigest string
	byDigest    map[string][]helmChartEntry
	byRelease   map[helmRelease]helmReleaseDownload
}

type helmChartEntry struct {
	name      string
	created   time.Time
	downloads []helmChartDownload
}

type helmRelease struct{ chart, version string }

// helmReleaseDownload is what helm downloads for a release: the first URL
// the index lists for it, checked against its digest.
type helmReleaseDownload struct {
	digest  string
	created time.Time
	first   helmChartDownload
}

// chartLookup returns the charts the index lists, parsing it only when the
// last lookup built for the repository came from other bytes. An index
// without a digest, read with the metadata cache off, is parsed every time.
func (h *HelmHandler) chartLookup(repository, upstreamURL string, index metadataResult) (*helmChartLookup, error) {
	key := repository + "\x00" + upstreamURL
	if index.digest != "" {
		h.lookupMu.Lock()
		cached := h.lookups[key]
		h.lookupMu.Unlock()
		if cached != nil && cached.indexDigest == index.digest {
			return cached, nil
		}
	}

	lookup, err := h.buildChartLookup(upstreamURL, index.body)
	if err != nil {
		return nil, err
	}
	if index.digest != "" {
		lookup.indexDigest = index.digest
		h.lookupMu.Lock()
		if h.lookups == nil {
			h.lookups = make(map[string]*helmChartLookup)
		}
		h.lookups[key] = lookup
		h.lookupMu.Unlock()
	}
	return lookup, nil
}

func (h *HelmHandler) buildChartLookup(upstreamURL string, body []byte) (*helmChartLookup, error) {
	_, entries, err := parseHelmIndex(body)
	if err != nil {
		return nil, err
	}

	lookup := &helmChartLookup{byDigest: make(map[string][]helmChartEntry), byRelease: make(map[helmRelease]helmReleaseDownload)}
	for i := 0; i < len(entries.Content); i += 2 {
		chartName := entries.Content[i].Value
		releases := entries.Content[i+1]
		if releases.Kind != yaml.SequenceNode {
			return nil, fmt.Errorf("chart %q releases must be a sequence", chartName)
		}
		for _, release := range releases.Content {
			chart, err := h.parseChartRelease(chartName, upstreamURL, release)
			if err != nil {
				continue
			}
			entry := helmChartEntry{name: chartName, created: chart.created}
			if version := helmMappingValue(release, "version"); version != nil && version.Value != "" {
				// helm takes the first release listed for a version.
				key := helmRelease{chartName, version.Value}
				if _, listed := lookup.byRelease[key]; !listed {
					first := chart.downloads[0]
					lookup.byRelease[key] = helmReleaseDownload{
						digest:  chart.digest,
						created: chart.created,
						first:   helmChartDownload{url: first.url, filename: first.filename, oci: first.oci},
					}
				}
			}
			for _, download := range chart.downloads {
				if !download.oci {
					// Drop the YAML node, so the lookup doesn't keep the
					// parsed index alive.
					entry.downloads = append(entry.downloads, helmChartDownload{url: download.url, filename: download.filename})
				}
			}
			lookup.byDigest[chart.digest] = append(lookup.byDigest[chart.digest], entry)
		}
	}
	return lookup, nil
}

func (h *HelmHandler) chartOnCooldown(chartName string, created time.Time) bool {
	return !created.IsZero() && h.proxy.Cooldown != nil && h.proxy.Cooldown.Enabled() &&
		!h.proxy.cooldown().IsAllowed(helmMetadataEcosystem, canonicalPackagePURL(helmMetadataEcosystem, chartName), created)
}

type helmChartDownload struct {
	node     *yaml.Node
	url      string
	filename string
	oci      bool
}

type helmChartRelease struct {
	created   time.Time
	digest    string
	downloads []helmChartDownload
}

var errHelmChartNotFound = errors.New("chart not found in Helm index")

func (h *HelmHandler) parseChartRelease(chartName, upstreamURL string, release *yaml.Node) (helmChartRelease, error) {
	digestNode := helmMappingValue(release, "digest")
	urlsNode := helmMappingValue(release, "urls")
	if digestNode == nil || urlsNode == nil || urlsNode.Kind != yaml.SequenceNode || len(urlsNode.Content) == 0 {
		return helmChartRelease{}, fmt.Errorf("chart %q has no digest or URLs", chartName)
	}
	digest, ok := normalizeHelmDigest(digestNode.Value)
	if !ok {
		return helmChartRelease{}, fmt.Errorf("chart %q has invalid digest", chartName)
	}

	baseURL, err := url.Parse(upstreamURL + "/" + helmIndexFilename)
	if err != nil {
		return helmChartRelease{}, fmt.Errorf("parsing Helm repository URL: %w", err)
	}

	chart := helmChartRelease{digest: digest}
	if createdNode := helmMappingValue(release, "created"); createdNode != nil && createdNode.Value != "" {
		chart.created, err = time.Parse(time.RFC3339Nano, createdNode.Value)
		if err != nil {
			return helmChartRelease{}, fmt.Errorf("chart %q has invalid creation time: %w", chartName, err)
		}
	}

	for _, urlNode := range urlsNode.Content {
		download, err := parseHelmChartDownload(chartName, baseURL, urlNode)
		if err != nil {
			return helmChartRelease{}, err
		}
		chart.downloads = append(chart.downloads, download)
	}
	return chart, nil
}

func parseHelmChartDownload(chartName string, baseURL *url.URL, node *yaml.Node) (helmChartDownload, error) {
	if node.Kind != yaml.ScalarNode {
		return helmChartDownload{}, fmt.Errorf("chart %q has invalid URL", chartName)
	}
	reference, err := url.Parse(node.Value)
	if err != nil {
		return helmChartDownload{}, fmt.Errorf("parsing chart %q URL: %w", chartName, err)
	}
	if reference.Scheme == "oci" {
		if reference.Hostname() == "" || strings.Trim(reference.Path, "/") == "" ||
			reference.User != nil || reference.RawQuery != "" || reference.ForceQuery || reference.Fragment != "" ||
			containsPathTraversal(reference.Path) {
			return helmChartDownload{}, fmt.Errorf("chart %q has invalid OCI reference", chartName)
		}
		return helmChartDownload{node: node, url: node.Value, oci: true}, nil
	}
	downloadURL := baseURL.ResolveReference(reference)
	if (downloadURL.Scheme != "http" && downloadURL.Scheme != "https") || downloadURL.Host == "" {
		return helmChartDownload{}, fmt.Errorf("chart %q URL must be HTTP(S) or OCI", chartName)
	}
	filename := path.Base(downloadURL.Path)
	if filename == "." || filename == "/" || filename == "" || !strings.HasSuffix(filename, ".tgz") {
		return helmChartDownload{}, fmt.Errorf("chart %q URL must point to a .tgz file", chartName)
	}
	return helmChartDownload{node: node, url: downloadURL.String(), filename: filename}, nil
}

func (h *HelmHandler) chartProxyURL(repository, digest, filename string) string {
	return fmt.Sprintf("%s/helm/%s/charts/%s/%s", h.proxyURL,
		url.PathEscape(repository), digest, url.PathEscape(filename))
}

func parseHelmIndex(body []byte) (*yaml.Node, *yaml.Node, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(body, &document); err != nil {
		return nil, nil, fmt.Errorf("parsing Helm index: %w", err)
	}
	entries, err := helmIndexEntries(&document)
	if err != nil {
		return nil, nil, err
	}
	return &document, entries, nil
}

func helmIndexEntries(document *yaml.Node) (*yaml.Node, error) {
	if document == nil {
		return nil, errors.New("helm index is empty")
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("helm index must be a mapping")
	}
	entries := helmMappingValue(document.Content[0], "entries")
	if entries == nil || entries.Kind != yaml.MappingNode {
		return nil, errors.New("helm index has no entries mapping")
	}
	if len(entries.Content)%2 != 0 {
		return nil, errors.New("helm index entries mapping has an incomplete key-value pair")
	}
	return entries, nil
}

func helmMappingValue(mapping *yaml.Node, key string) *yaml.Node {
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

func normalizeHelmDigest(value string) (string, bool) {
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
