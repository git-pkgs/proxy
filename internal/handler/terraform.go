package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/git-pkgs/purl"
	"golang.org/x/sync/errgroup"
)

const (
	terraformEcosystem = "terraform"
	// terraformDownloadConcurrency limits the download documents fetched at
	// once to list a version's archives, one per platform.
	terraformDownloadConcurrency = 4
	// terraformUpstreamKeyLength is how much of a registry URL's digest
	// scopes metadata cache keys: enough to keep two registries apart.
	terraformUpstreamKeyLength = 16
)

var (
	// terraformName matches provider namespaces and types.
	terraformName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
	// terraformVersion matches semantic versions, with prerelease and build
	// suffixes.
	terraformVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]+)?$`)
	// terraformArchive matches the archive names the proxy lists in a version
	// document: {os}_{arch}.zip.
	terraformArchive = regexp.MustCompile(`^([a-z0-9]+)_([a-z0-9]+)\.zip$`)
	terraformShasum  = regexp.MustCompile(`^[0-9a-f]{64}$`)

	errTerraformDocument = errors.New("unexpected Terraform registry document")
)

// DefaultTerraformRegistries are the provider registries mirrored when none
// are configured.
var DefaultTerraformRegistries = map[string]string{
	"registry.terraform.io": "https://registry.terraform.io",
	"registry.opentofu.org": "https://registry.opentofu.org",
}

// TerraformHandler serves the Provider Network Mirror Protocol at
// /terraform/providers/, for Terraform and OpenTofu. The mirror path names
// the registry hostname of each provider; only configured hostnames are
// fetched. Version lists and archive locations come from the registry's
// Provider Registry Protocol, and archives are cached and checked against the
// registry's shasum.
type TerraformHandler struct {
	proxy *Proxy
	// registries maps hostnames to registry base URLs.
	registries map[string]string
}

// NewTerraformHandler creates a provider mirror for registries, which maps
// registry hostnames to base URLs. With none, DefaultTerraformRegistries are
// mirrored.
func NewTerraformHandler(proxy *Proxy, registries map[string]string) *TerraformHandler {
	if len(registries) == 0 {
		registries = DefaultTerraformRegistries
	}
	h := &TerraformHandler{proxy: proxy, registries: make(map[string]string, len(registries))}
	for hostname, baseURL := range registries {
		h.registries[strings.ToLower(hostname)] = strings.TrimSuffix(baseURL, "/")
	}
	return h
}

// Routes returns the HTTP handler for provider mirror requests.
func (h *TerraformHandler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /providers/{hostname}/{namespace}/{type}/{file}", h.handleProviderDocument)
	mux.HandleFunc("GET /providers/{hostname}/{namespace}/{type}/{version}/{archive}", h.handleArchive)
	return mux
}

// terraformProvider is a provider on one registry.
type terraformProvider struct {
	hostname, namespace, name string
	// baseURL is the registry's base URL.
	baseURL string
}

// providerFromRequest returns the provider a request names. ok is false when
// it has already answered: 404 for a hostname with no registry configured,
// 400 for an invalid namespace or type.
func (h *TerraformHandler) providerFromRequest(w http.ResponseWriter, r *http.Request) (terraformProvider, bool) {
	hostname := strings.ToLower(r.PathValue("hostname"))
	baseURL, ok := h.registries[hostname]
	if !ok {
		http.Error(w, "no registry configured for this hostname", http.StatusNotFound)
		return terraformProvider{}, false
	}
	namespace, name := r.PathValue("namespace"), r.PathValue("type")
	if !terraformName.MatchString(namespace) || !terraformName.MatchString(name) {
		http.Error(w, "invalid provider address", http.StatusBadRequest)
		return terraformProvider{}, false
	}
	return terraformProvider{hostname: hostname, namespace: namespace, name: name, baseURL: baseURL}, true
}

// handleProviderDocument serves index.json, the provider's versions, and
// {version}.json, the archives of one version.
func (h *TerraformHandler) handleProviderDocument(w http.ResponseWriter, r *http.Request) {
	provider, ok := h.providerFromRequest(w, r)
	if !ok {
		return
	}
	file := r.PathValue("file")
	if file == "index.json" {
		h.serveIndex(w, r, provider)
		return
	}
	version, ok := strings.CutSuffix(file, ".json")
	if !ok || !terraformVersion.MatchString(version) {
		http.NotFound(w, r)
		return
	}
	h.serveVersion(w, r, provider, version)
}

// terraformVersions is the Provider Registry Protocol's version list.
type terraformVersions struct {
	Versions []struct {
		Version   string `json:"version"`
		Platforms []struct {
			OS   string `json:"os"`
			Arch string `json:"arch"`
		} `json:"platforms"`
	} `json:"versions"`
}

// terraformDownload is the Provider Registry Protocol's download document for
// one platform of a version.
type terraformDownload struct {
	DownloadURL string `json:"download_url"`
	Shasum      string `json:"shasum"`
}

type terraformArchiveEntry struct {
	URL    string   `json:"url"`
	Hashes []string `json:"hashes,omitempty"`
}

func (h *TerraformHandler) serveIndex(w http.ResponseWriter, r *http.Request, provider terraformProvider) {
	versions, stale, err := h.versions(r.Context(), provider)
	if err != nil {
		h.writeError(w, r, provider, err)
		return
	}
	index := struct {
		Versions map[string]struct{} `json:"versions"`
	}{Versions: make(map[string]struct{}, len(versions.Versions))}
	for _, v := range versions.Versions {
		if terraformVersion.MatchString(v.Version) {
			index.Versions[v.Version] = struct{}{}
		}
	}
	markStale(w, stale)
	writeTerraformJSON(w, index)
}

// serveVersion lists a version's archives, one per platform the registry
// lists for it. The archive URLs are relative to the version document, so
// they resolve to the proxy's {version}/{os}_{arch}.zip.
func (h *TerraformHandler) serveVersion(w http.ResponseWriter, r *http.Request, provider terraformProvider, version string) {
	versions, stale, err := h.versions(r.Context(), provider)
	if err != nil {
		h.writeError(w, r, provider, err)
		return
	}
	var platforms []string
	for _, v := range versions.Versions {
		if v.Version != version {
			continue
		}
		for _, p := range v.Platforms {
			platform := p.OS + "_" + p.Arch
			if terraformArchive.MatchString(platform + ".zip") {
				platforms = append(platforms, platform)
			}
		}
	}
	if platforms == nil {
		http.NotFound(w, r)
		return
	}

	var mu sync.Mutex
	archives := make(map[string]terraformArchiveEntry, len(platforms))
	g, ctx := errgroup.WithContext(r.Context())
	g.SetLimit(terraformDownloadConcurrency)
	for _, platform := range platforms {
		g.Go(func() error {
			download, downloadStale, err := h.download(ctx, provider, version, platform)
			if errors.Is(err, ErrUpstreamNotFound) {
				return nil // listed without an archive; leave it out
			}
			if err != nil {
				return err
			}
			entry := terraformArchiveEntry{URL: version + "/" + platform + ".zip"}
			if download.Shasum != "" {
				entry.Hashes = []string{"zh:" + download.Shasum}
			}
			mu.Lock()
			archives[platform] = entry
			stale = stale || downloadStale
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		h.writeError(w, r, provider, err)
		return
	}
	markStale(w, stale)
	writeTerraformJSON(w, struct {
		Archives map[string]terraformArchiveEntry `json:"archives"`
	}{archives})
}

// handleArchive serves a provider archive from the cache. One not cached yet
// is fetched from the location in the registry's download document and
// checked against its shasum before it is served or cached.
func (h *TerraformHandler) handleArchive(w http.ResponseWriter, r *http.Request) {
	provider, ok := h.providerFromRequest(w, r)
	if !ok {
		return
	}
	version := r.PathValue("version")
	if !terraformVersion.MatchString(version) {
		http.Error(w, "invalid provider version", http.StatusBadRequest)
		return
	}
	match := terraformArchive.FindStringSubmatch(r.PathValue("archive"))
	if match == nil {
		http.NotFound(w, r)
		return
	}
	goos, arch := match[1], match[2]

	name := provider.address()
	pkgPURL, versionPURL := provider.purls(version)
	filename := provider.archiveFilename(version, goos, arch)
	if h.proxy.versionDenied(terraformEcosystem, name, version) || h.proxy.Denylist.Denied(versionPURL) {
		h.proxy.serveArtifactError(w, fmt.Errorf("%w: %s", ErrVersionDenied, versionPURL), "failed to fetch provider")
		return
	}
	cached, err := h.proxy.checkCache(r.Context(), pkgPURL, versionPURL, filename)
	if err != nil {
		h.proxy.serveArtifactError(w, err, "failed to fetch provider")
		return
	}
	if cached == nil {
		download, _, err := h.download(r.Context(), provider, version, goos+"_"+arch)
		if err != nil {
			h.writeError(w, r, provider, err)
			return
		}
		cached, err = h.proxy.getOrFetchArtifactFromURLWithCachePURLs(
			r.Context(), terraformEcosystem, name, version, filename, pkgPURL, versionPURL,
			download.DownloadURL, nil, download.Shasum, "")
		if err != nil {
			h.proxy.serveArtifactError(w, err, "failed to fetch provider")
			return
		}
	}
	if cached.Artifact.MediaType == "" {
		w.Header().Set(headerContentType, "application/zip")
	}
	ServeArtifactRequest(w, r, cached)
}

// versions fetches the provider's version list through the metadata cache.
func (h *TerraformHandler) versions(ctx context.Context, provider terraformProvider) (terraformVersions, bool, error) {
	providersURL, err := h.providersURL(ctx, provider)
	if err != nil {
		return terraformVersions{}, false, err
	}
	var versions terraformVersions
	stale, err := h.fetchDocument(ctx, provider, "versions", providersURL+provider.namespace+"/"+provider.name+"/versions", &versions)
	return versions, stale, err
}

// download fetches the download document of one platform of a version
// through the metadata cache.
func (h *TerraformHandler) download(ctx context.Context, provider terraformProvider, version, platform string) (terraformDownload, bool, error) {
	providersURL, err := h.providersURL(ctx, provider)
	if err != nil {
		return terraformDownload{}, false, err
	}
	goos, arch, _ := strings.Cut(platform, "_")
	documentKey := version + "/download/" + goos + "/" + arch
	var download terraformDownload
	stale, err := h.fetchDocument(ctx, provider, documentKey,
		providersURL+provider.namespace+"/"+provider.name+"/"+url.PathEscape(version)+"/download/"+goos+"/"+arch, &download)
	if err != nil {
		return terraformDownload{}, false, err
	}
	download.Shasum = strings.ToLower(download.Shasum)
	if !terraformShasum.MatchString(download.Shasum) || download.DownloadURL == "" {
		return terraformDownload{}, false, fmt.Errorf("%w: download %s %s", errTerraformDocument, version, platform)
	}
	return download, stale, nil
}

// providersURL returns the registry's providers.v1 base URL from its service
// discovery document, ending in a slash.
func (h *TerraformHandler) providersURL(ctx context.Context, provider terraformProvider) (string, error) {
	var discovery struct {
		Providers string `json:"providers.v1"`
	}
	discoveryURL := provider.baseURL + "/.well-known/terraform.json"
	if _, err := h.fetchRegistryDocument(ctx, provider, "discovery", discoveryURL, &discovery); err != nil {
		return "", err
	}
	if discovery.Providers == "" {
		return "", fmt.Errorf("%w: %s has no providers.v1 service", errTerraformDocument, provider.hostname)
	}
	base, _ := url.Parse(discoveryURL)
	ref, err := url.Parse(discovery.Providers)
	if err != nil || (ref.Scheme != "" && ref.Scheme != "https" && ref.Scheme != "http") {
		return "", fmt.Errorf("%w: providers.v1 %q", errTerraformDocument, discovery.Providers)
	}
	resolved := base.ResolveReference(ref).String()
	if !strings.HasSuffix(resolved, "/") {
		resolved += "/"
	}
	return resolved, nil
}

// fetchDocument fetches a JSON document of the provider through the metadata
// cache and decodes it into v. It reports whether the stored copy was served
// because the registry was unavailable.
func (h *TerraformHandler) fetchDocument(ctx context.Context, provider terraformProvider, documentKey, upstreamURL string, v any) (bool, error) {
	return h.fetchRegistryDocument(ctx, provider, "providers/"+provider.namespace+"/"+provider.name+"/"+documentKey, upstreamURL, v)
}

func (h *TerraformHandler) fetchRegistryDocument(ctx context.Context, provider terraformProvider, documentKey, upstreamURL string, v any) (bool, error) {
	res := h.proxy.fetchMetadataDocument(ctx, terraformEcosystem, provider.cacheKey(documentKey), upstreamURL, contentTypeJSON, nil)
	if res.err != nil {
		return false, res.err
	}
	if err := json.Unmarshal(res.body, v); err != nil {
		return false, fmt.Errorf("%w: %s: %w", errTerraformDocument, upstreamURL, err)
	}
	return res.stale, nil
}

func (h *TerraformHandler) writeError(w http.ResponseWriter, r *http.Request, provider terraformProvider, err error) {
	switch {
	case r.Context().Err() != nil:
		return // the client left
	case errors.Is(err, ErrUpstreamNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case writeUpstreamHeld(w, err):
	default:
		h.proxy.Logger.Error("Terraform registry request failed", "registry", provider.hostname, "provider", provider.address(), "error", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
	}
}

// address is the provider's source address, such as
// registry.terraform.io/hashicorp/aws.
func (p terraformProvider) address() string {
	return p.hostname + "/" + p.namespace + "/" + p.name
}

// purls returns the package and version PURLs a provider's archives are
// cached under. There is no PURL type for providers, so they are generic
// packages named by the provider's source address, which includes the
// registry hostname.
func (p terraformProvider) purls(version string) (pkgPURL, versionPURL string) {
	pkg := purl.New("generic", "terraform-provider/"+p.hostname+"/"+p.namespace, p.name, "", nil)
	return pkg.String(), pkg.WithVersion(version).String()
}

// upstreamKey identifies the registry's base URL in cache keys, so pointing
// a hostname at another registry does not serve the previous one's documents
// or archives.
func (p terraformProvider) upstreamKey() string {
	digest := sha256.Sum256([]byte(p.baseURL))
	return hex.EncodeToString(digest[:])[:terraformUpstreamKeyLength]
}

// cacheKey scopes a metadata cache key to the registry's hostname and base
// URL.
func (p terraformProvider) cacheKey(documentKey string) string {
	return p.hostname + "-" + p.upstreamKey() + "/" + documentKey
}

// archiveFilename names a provider archive in the cache. The filename is
// part of the artifact's cache identity, so it carries the registry's key.
func (p terraformProvider) archiveFilename(version, goos, arch string) string {
	return fmt.Sprintf("terraform-provider-%s_%s_%s_%s-%s.zip", p.name, version, goos, arch, p.upstreamKey())
}

func writeTerraformJSON(w http.ResponseWriter, v any) {
	body, err := encodeChefJSON(v)
	if err != nil {
		http.Error(w, "failed to encode response", http.StatusInternalServerError)
		return
	}
	w.Header().Set(headerContentType, contentTypeJSON)
	_, _ = w.Write(body)
}
