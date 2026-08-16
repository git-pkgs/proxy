package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	helmMetadataEcosystem = "helm"
	helmIndexFilename     = "index.yaml"
	sha256HexLength       = 64
)

// HelmHandler serves read-only HTTP Helm chart repositories. Each configured
// repository is mounted at /helm/{repository}/.
type HelmHandler struct {
	proxy        *Proxy
	proxyURL     string
	repositories map[string]string
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

	body, contentType, err := h.fetchIndex(r, repository, upstreamURL)
	if err != nil {
		h.serveIndexError(w, err)
		return
	}

	rewritten, err := h.rewriteIndex(repository, upstreamURL, body)
	if err != nil {
		h.proxy.Logger.Warn("failed to rewrite Helm index", "repository", repository, "error", err)
		http.Error(w, "invalid Helm repository index", http.StatusBadGateway)
		return
	}

	h.proxy.writeMetadataCachedResponse(w, r, helmMetadataEcosystem, h.indexCacheKey(repository, upstreamURL), rewritten, contentType)
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

	body, _, err := h.fetchIndex(r, repository, upstreamURL)
	if err != nil {
		h.serveIndexError(w, err)
		return
	}

	downloadURL, err := h.findChartDownload(upstreamURL, body, digest, filename)
	if err != nil {
		if errors.Is(err, errHelmChartNotFound) {
			http.NotFound(w, r)
			return
		}
		h.proxy.Logger.Warn("failed to read Helm index", "repository", repository, "error", err)
		http.Error(w, "invalid Helm repository index", http.StatusBadGateway)
		return
	}

	result, err := h.proxy.GetOrFetchArtifactFromURL(
		r.Context(), helmMetadataEcosystem, repository, digest, filename, downloadURL)
	if err != nil {
		h.proxy.serveArtifactError(w, err, "failed to fetch chart")
		return
	}
	if !strings.EqualFold(result.Hash, digest) {
		if result.Reader != nil {
			_ = result.Reader.Close()
		}
		if clearErr := h.proxy.ClearCachedArtifact(r.Context(), helmMetadataEcosystem, repository, digest, filename); clearErr != nil {
			h.proxy.Logger.Warn("failed to clear Helm chart with invalid digest", "error", clearErr)
		}
		http.Error(w, "chart digest verification failed", http.StatusBadGateway)
		return
	}

	if result.ContentType == "" {
		w.Header().Set("Content-Type", "application/gzip")
	}
	ServeArtifact(w, result)
}

func (h *HelmHandler) repositoryForRequest(r *http.Request) (name, upstreamURL string, ok bool) {
	name = r.PathValue("repository")
	upstreamURL, ok = h.repositories[name]
	return name, upstreamURL, ok
}

func (h *HelmHandler) fetchIndex(r *http.Request, repository, upstreamURL string) ([]byte, string, error) {
	return h.proxy.FetchOrCacheMetadata(
		r.Context(),
		helmMetadataEcosystem,
		h.indexCacheKey(repository, upstreamURL),
		upstreamURL+"/"+helmIndexFilename,
		"application/x-yaml, text/yaml;q=0.9, */*;q=0.1",
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
	h.proxy.Logger.Error("failed to fetch Helm index", "error", err)
	http.Error(w, "failed to fetch Helm repository index", http.StatusBadGateway)
}

func (h *HelmHandler) rewriteIndex(repository, upstreamURL string, body []byte) ([]byte, error) {
	document, entries, err := parseHelmIndex(body)
	if err != nil {
		return nil, err
	}

	for i := 0; i < len(entries.Content); i += 2 {
		chartName := entries.Content[i].Value
		releases := entries.Content[i+1]
		if releases.Kind != yaml.SequenceNode {
			return nil, fmt.Errorf("chart %q releases must be a sequence", chartName)
		}

		filtered := make([]*yaml.Node, 0, len(releases.Content))
		for _, release := range releases.Content {
			chart, err := h.parseChartRelease(chartName, upstreamURL, release)
			if err != nil {
				return nil, err
			}
			if h.chartOnCooldown(chartName, chart.created) {
				continue
			}
			for _, download := range chart.downloads {
				download.node.Value = h.chartProxyURL(repository, chart.digest, download.filename)
			}
			filtered = append(filtered, release)
		}
		releases.Content = filtered
	}

	return yaml.Marshal(document)
}

func (h *HelmHandler) findChartDownload(upstreamURL string, body []byte, digest, filename string) (string, error) {
	_, entries, err := parseHelmIndex(body)
	if err != nil {
		return "", err
	}

	for i := 0; i < len(entries.Content); i += 2 {
		chartName := entries.Content[i].Value
		releases := entries.Content[i+1]
		if releases.Kind != yaml.SequenceNode {
			return "", fmt.Errorf("chart %q releases must be a sequence", chartName)
		}
		for _, release := range releases.Content {
			chart, err := h.parseChartRelease(chartName, upstreamURL, release)
			if err != nil {
				return "", err
			}
			if chart.digest != digest || h.chartOnCooldown(chartName, chart.created) {
				continue
			}
			for _, download := range chart.downloads {
				if download.filename == filename {
					return download.url, nil
				}
			}
		}
	}

	return "", errHelmChartNotFound
}

func (h *HelmHandler) chartOnCooldown(chartName string, created time.Time) bool {
	return !created.IsZero() && h.proxy.Cooldown != nil && h.proxy.Cooldown.Enabled() &&
		!h.proxy.Cooldown.IsAllowed(helmMetadataEcosystem, canonicalPackagePURL(helmMetadataEcosystem, chartName), created)
}

type helmChartDownload struct {
	node     *yaml.Node
	url      string
	filename string
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
		if urlNode.Kind != yaml.ScalarNode {
			return helmChartRelease{}, fmt.Errorf("chart %q has invalid URL", chartName)
		}
		reference, err := url.Parse(urlNode.Value)
		if err != nil {
			return helmChartRelease{}, fmt.Errorf("parsing chart %q URL: %w", chartName, err)
		}
		downloadURL := baseURL.ResolveReference(reference)
		if (downloadURL.Scheme != "http" && downloadURL.Scheme != "https") || downloadURL.Host == "" {
			return helmChartRelease{}, fmt.Errorf("chart %q URL must be HTTP(S)", chartName)
		}
		filename := path.Base(downloadURL.Path)
		if filename == "." || filename == "/" || filename == "" || !strings.HasSuffix(filename, ".tgz") {
			return helmChartRelease{}, fmt.Errorf("chart %q URL must point to a .tgz file", chartName)
		}
		chart.downloads = append(chart.downloads, helmChartDownload{
			node:     urlNode,
			url:      downloadURL.String(),
			filename: filename,
		})
	}
	return chart, nil
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
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, nil, errors.New("helm index must be a mapping")
	}
	entries := helmMappingValue(document.Content[0], "entries")
	if entries == nil || entries.Kind != yaml.MappingNode {
		return nil, nil, errors.New("helm index has no entries mapping")
	}
	return &document, entries, nil
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
