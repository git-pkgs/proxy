package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/git-pkgs/proxy/internal/metadata/helm"
	"github.com/git-pkgs/proxy/internal/retention"
)

const (
	helmMetadataEcosystem = "helm"
	helmIndexFilename     = helm.IndexFilename
	helmIndexAccept       = "application/x-yaml, text/yaml;q=0.9, */*;q=0.1"
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

	var charts *helmChartLookup
	if h.proxy.cooldownEnabled() {
		var err error
		charts, err = h.downloadChartLookup(r, repository, upstreamURL, digest, filename)
		if err != nil {
			cached, cacheErr := h.proxy.GetCachedArtifact(r.Context(), helmMetadataEcosystem, repository, digest, filename)
			if cacheErr == nil && cached != nil {
				h.serveChart(w, r, repository, digest, filename, cached)
			} else {
				h.proxy.writeCooldownLookupError(w, helmMetadataEcosystem, repository, err)
			}
			return
		}
		if h.withholdChart(w, charts, digest, filename) {
			return
		}
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

	var downloadURL string
	if charts == nil {
		index := h.fetchIndex(r.Context(), repository, upstreamURL)
		if index.err != nil {
			h.serveIndexError(w, index.err)
			return
		}
		downloadURL, err = h.findChartDownload(repository, upstreamURL, index, digest, filename)
	} else {
		downloadURL, err = h.findChartURL(charts, digest, filename)
	}
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
		helmIndexAccept,
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
	if h.proxy.writeUpstreamRefusal(w, err) {
		return
	}
	h.proxy.Logger.Error("failed to fetch Helm index", "error", err)
	http.Error(w, "failed to fetch Helm repository index", http.StatusBadGateway)
}

// rewriteIndex points the index's chart URLs at the proxy and drops releases
// in cooldown. It also returns the releases it kept, by chart name and
// digest, which together with the index fix its output; see helm.Rewrite.
func (h *HelmHandler) rewriteIndex(repository, upstreamURL string, body []byte) ([]byte, []string, error) {
	return helm.Rewrite(body, helm.Options{
		ProxyURL:      h.proxyURL,
		Repository:    repository,
		UpstreamURL:   upstreamURL,
		OCIRegistries: h.ociRegistries,
		Cooldown:      h.proxy.cooldown(),
		EvaluatedAt:   time.Now(),
		OnMalformed: func(chart string, err error) {
			h.proxy.Logger.Warn("omitting malformed Helm chart release",
				"repository", repository, "chart", chart, "error", err)
		},
	})
}

// findChartDownload returns the upstream URL of a chart in the index. Parsing
// a large index takes a while, so the charts it lists are kept per
// repository for as long as the stored index stays the same.
func (h *HelmHandler) findChartDownload(repository, upstreamURL string, index metadataResult, digest, filename string) (string, error) {
	charts, err := h.chartLookup(repository, upstreamURL, index)
	if err != nil {
		return "", err
	}
	return h.findChartURL(charts, digest, filename)
}

func (h *HelmHandler) findChartURL(charts *helmChartLookup, digest, filename string) (string, error) {
	for _, chart := range charts.ByDigest[digest] {
		if h.chartOnCooldown(chart.Name, chart.Created) {
			continue
		}
		for _, download := range chart.Downloads {
			if download.Filename == filename {
				return download.URL, nil
			}
		}
	}
	return "", errHelmChartNotFound
}

// helmChartLookup holds the HTTP chart downloads an index lists (see
// helm.Lookup), and helmRelease names a release in it.
type (
	helmChartLookup = helm.Lookup
	helmRelease     = helm.Release
)

// chartLookup returns the charts the index lists, parsing it only when the
// last lookup built for the repository came from other bytes. An index
// without a digest, read with the metadata cache off, is parsed every time.
func (h *HelmHandler) chartLookup(repository, upstreamURL string, index metadataResult) (*helmChartLookup, error) {
	key := repository + "\x00" + upstreamURL
	if index.digest != "" {
		h.lookupMu.Lock()
		cached := h.lookups[key]
		h.lookupMu.Unlock()
		if cached != nil && cached.IndexDigest == index.digest {
			return cached, nil
		}
	}

	lookup, err := h.buildChartLookup(upstreamURL, index.body)
	if err != nil {
		return nil, err
	}
	if index.digest != "" {
		lookup.IndexDigest = index.digest
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
	return helm.BuildLookup(upstreamURL, body)
}

// downloadChartLookup returns the charts the index lists, for a download
// checked against cooldown. An index that doesn't list the download can
// predate the chart, and is fetched again (listingPublishIndex).
func (h *HelmHandler) downloadChartLookup(r *http.Request, repository, upstreamURL, digest, filename string) (*helmChartLookup, error) {
	cacheKey := h.indexCacheKey(repository, upstreamURL)
	return listingPublishIndex(h.proxy, "helm\x00"+cacheKey, func() metadataResult {
		return h.fetchIndex(r.Context(), repository, upstreamURL)
	}, func() metadataResult {
		return h.proxy.refetchLacking(r.Context(), helmMetadataEcosystem, cacheKey,
			upstreamURL+"/"+helmIndexFilename, helmIndexAccept, "", nil)
	}, func(res metadataResult) (*helmChartLookup, error) {
		if res.err != nil {
			return nil, res.err
		}
		return h.chartLookup(repository, upstreamURL, res)
	}, func(charts *helmChartLookup) bool {
		return charts.Lists(digest, filename)
	})
}

func (h *HelmHandler) withholdChart(w http.ResponseWriter, charts *helmChartLookup, digest, filename string) bool {
	withheld := false
	for _, chart := range charts.ByDigest[digest] {
		for _, download := range chart.Downloads {
			if download.Filename != filename {
				continue
			}
			if !h.chartOnCooldown(chart.Name, chart.Created) {
				return false
			}
			withheld = true
		}
	}
	if withheld {
		http.Error(w, ErrVersionNotFound.Error(), http.StatusNotFound)
	}
	return withheld
}

func (h *HelmHandler) chartOnCooldown(chartName string, created time.Time) bool {
	return !created.IsZero() && h.proxy.Cooldown != nil && h.proxy.Cooldown.Enabled() &&
		!h.proxy.cooldown().IsAllowed(helmMetadataEcosystem, canonicalPackagePURL(helmMetadataEcosystem, chartName), created)
}

var errHelmChartNotFound = errors.New("chart not found in Helm index")

func (h *HelmHandler) chartProxyURL(repository, digest, filename string) string {
	return helm.ChartProxyURL(h.proxyURL, repository, digest, filename)
}

func parseHelmIndex(body []byte) (*yaml.Node, *yaml.Node, error) {
	return helm.ParseIndex(body)
}

func helmIndexEntries(document *yaml.Node) (*yaml.Node, error) {
	return helm.IndexEntries(document)
}

func helmMappingValue(mapping *yaml.Node, key string) *yaml.Node {
	return helm.MappingValue(mapping, key)
}

func normalizeHelmDigest(value string) (string, bool) {
	return helm.NormalizeDigest(value)
}
