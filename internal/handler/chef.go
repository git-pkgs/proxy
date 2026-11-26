package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/git-pkgs/proxy/internal/metadata/chef"
)

const (
	chefEcosystem = "chef"
	chefAPIPath   = chef.APIPath
	// chefUpstreamKeyLength is how much of the upstream URL's digest scopes
	// metadata cache keys and archive filenames: enough to keep two upstreams
	// apart.
	chefUpstreamKeyLength = 16
)

// chefCookbookName matches Supermarket cookbook names: letters, digits,
// hyphens, underscores and dots, not starting with a dot.
var chefCookbookName = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]*$`)

// ChefHandler serves a read-only Chef Supermarket at /chef. Berkshelf and
// Policyfiles resolve dependencies against /universe and then download each
// cookbook through the cookbook API. Every download location in those
// documents is rewritten to the proxy, so cookbooks are fetched through it and
// cached; dependency constraints and every other field pass through unchanged.
type ChefHandler struct {
	proxy       *Proxy
	proxyURL    string
	upstreamURL string
	// upstreamKey identifies the upstream in cache keys, so pointing
	// upstream.chef at another Supermarket does not serve the previous one's
	// documents or archives.
	upstreamKey string
}

// NewChefHandler creates a Chef Supermarket handler for the Supermarket at
// upstreamURL, such as https://supermarket.chef.io or a private Supermarket.
func NewChefHandler(proxy *Proxy, proxyURL, upstreamURL string) *ChefHandler {
	upstreamURL = strings.TrimSuffix(upstreamURL, "/")
	digest := sha256.Sum256([]byte(upstreamURL))
	return &ChefHandler{
		proxy:       proxy,
		proxyURL:    strings.TrimSuffix(proxyURL, "/"),
		upstreamURL: upstreamURL,
		upstreamKey: hex.EncodeToString(digest[:])[:chefUpstreamKeyLength],
	}
}

// Routes returns the HTTP handler for Chef Supermarket requests.
func (h *ChefHandler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /universe", h.handleUniverse)
	mux.HandleFunc("GET "+chefAPIPath+"/cookbooks/{name}", h.handleCookbook)
	mux.HandleFunc("GET "+chefAPIPath+"/cookbooks/{name}/versions/{version}", h.handleVersion)
	mux.HandleFunc("GET "+chefAPIPath+"/cookbooks/{name}/versions/{version}/download", h.handleDownload)
	return mux
}

func (h *ChefHandler) handleUniverse(w http.ResponseWriter, r *http.Request) {
	h.serveDocument(w, r, "universe", h.upstreamURL+"/universe", func(body []byte) ([]byte, error) {
		// The rewrite is shared by requests waiting on it and cached, so it
		// finishes even if the request that started it goes away.
		withheld, err := h.universeWithheld(context.WithoutCancel(r.Context()), body)
		if err != nil {
			return nil, err
		}
		return h.rewriteUniverse(body, func(name, version string) bool { return withheld[name][version] })
	})
}

func (h *ChefHandler) handleCookbook(w http.ResponseWriter, r *http.Request) {
	name, ok := chefCookbookFromRequest(r)
	if !ok {
		http.Error(w, "invalid cookbook name", http.StatusBadRequest)
		return
	}
	h.serveDocument(w, r, "cookbooks/"+name, h.upstreamCookbookURL(name), func(body []byte) ([]byte, error) {
		withheld, err := h.cookbookWithheld(context.WithoutCancel(r.Context()), name, body)
		if err != nil {
			return nil, err
		}
		return h.rewriteCookbook(name, body, func(_, version string) bool { return withheld[version] })
	})
}

func (h *ChefHandler) handleVersion(w http.ResponseWriter, r *http.Request) {
	name, version, ok := chefVersionFromRequest(r)
	if !ok {
		http.Error(w, "invalid cookbook version", http.StatusBadRequest)
		return
	}
	if h.checkVersionCooldown(w, r, name, version) {
		return
	}
	h.serveDocument(w, r, "cookbooks/"+name+"/versions/"+version, h.upstreamVersionURL(name, version), func(body []byte) ([]byte, error) {
		return h.rewriteVersion(name, version, body)
	})
}

// handleDownload serves a cookbook archive from the cache. An archive not
// cached yet is fetched through the Supermarket download endpoint, following
// the redirect it answers with. A cached archive is served without the
// upstream. Archives are cached per upstream, since two Supermarkets can
// publish different cookbooks under the same name and version.
func (h *ChefHandler) handleDownload(w http.ResponseWriter, r *http.Request) {
	name, version, ok := chefVersionFromRequest(r)
	if !ok {
		http.Error(w, "invalid cookbook version", http.StatusBadRequest)
		return
	}
	if h.checkDownloadCooldown(w, r, name, version) {
		return
	}

	result, err := h.proxy.GetOrFetchArtifactFromURL(
		r.Context(), chefEcosystem, name, version, h.archiveFilename(name, version), h.upstreamVersionURL(name, version)+"/download")
	if err != nil {
		h.proxy.serveArtifactError(w, err, "failed to fetch cookbook")
		return
	}
	if result.Artifact.MediaType == "" {
		w.Header().Set(headerContentType, "application/gzip")
	}
	ServeArtifactRequest(w, r, result)
}

// serveDocument serves a JSON document from the metadata cache, which applies
// the metadata TTL and falls back to the stored copy while the upstream is
// unavailable, rewritten by rewrite.
func (h *ChefHandler) serveDocument(w http.ResponseWriter, r *http.Request, documentKey, upstreamURL string, rewrite func([]byte) ([]byte, error)) {
	cacheKey := h.cacheKey(documentKey)
	if stored, ok := h.proxy.storedRewrite(chefEcosystem, cacheKey, h.proxyURL, cacheKey); ok {
		serveRewrittenMetadata(w, r, stored)
		return
	}

	upstream := h.proxy.fetchMetadataDocument(r.Context(), chefEcosystem, cacheKey, upstreamURL, contentTypeJSON, nil)
	if err := upstream.err; err != nil {
		if errors.Is(err, ErrUpstreamNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if h.proxy.writeUpstreamRefusal(w, err) {
			return
		}
		h.proxy.Logger.Error("Chef Supermarket request failed", "url", upstreamURL, "error", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}

	rewritten, err := h.proxy.cachedRewrite(r.Context(), chefEcosystem, h.proxyURL, cacheKey, upstream.body, upstream.digest,
		func(body []byte) ([]byte, []string, error) {
			out, err := rewrite(body)
			return out, nil, err
		})
	if err != nil {
		if r.Context().Err() != nil {
			return // the client left while waiting on a shared rewrite
		}
		if h.proxy.writeUpstreamRefusal(w, err) {
			return
		}
		// Serving the document unrewritten would send clients straight to
		// the upstream, bypassing the cache.
		h.proxy.Logger.Warn("failed to rewrite Chef Supermarket document", "url", upstreamURL, "error", err)
		http.Error(w, "invalid Chef Supermarket response", http.StatusBadGateway)
		return
	}
	markStale(w, upstream.stale)
	serveRewrittenMetadata(w, r, rewritten)
}

// rewriteUniverse points every version's location_path and download_url at
// the proxy and leaves out the versions withheld reports; see
// chef.RewriteUniverse.
func (h *ChefHandler) rewriteUniverse(body []byte, withheld func(name, version string) bool) ([]byte, error) {
	opts := h.rewriteOptions()
	opts.Withheld = withheld
	return chef.RewriteUniverse(body, opts)
}

// rewriteCookbook points a cookbook's version URLs at the proxy and leaves
// out the versions withheld reports; see chef.RewriteCookbook.
func (h *ChefHandler) rewriteCookbook(name string, body []byte, withheld func(name, version string) bool) ([]byte, error) {
	opts := h.rewriteOptions()
	opts.Withheld = withheld
	return chef.RewriteCookbook(name, body, opts)
}

// rewriteVersion points a cookbook version's cookbook and file URLs at the
// proxy; see chef.RewriteVersion.
func (h *ChefHandler) rewriteVersion(name, version string, body []byte) ([]byte, error) {
	return chef.RewriteVersion(name, version, body, h.rewriteOptions())
}

func (h *ChefHandler) rewriteOptions() chef.Options {
	return chef.Options{ProxyURL: h.proxyURL}
}

func (h *ChefHandler) upstreamCookbookURL(name string) string {
	return h.upstreamURL + chefAPIPath + "/cookbooks/" + url.PathEscape(name)
}

func (h *ChefHandler) upstreamVersionURL(name, version string) string {
	return h.upstreamCookbookURL(name) + "/versions/" + url.PathEscape(version)
}

// cacheKey prefixes a metadata cache key with the upstream's key.
func (h *ChefHandler) cacheKey(documentKey string) string {
	return h.upstreamKey + "/" + documentKey
}

// archiveFilename names a cookbook archive in the cache. The filename is part
// of the artifact's cache identity, so it carries the upstream's key; the
// .tgz extension is kept for browsing the archive.
func (h *ChefHandler) archiveFilename(name, version string) string {
	return name + "-" + version + "-" + h.upstreamKey + ".tgz"
}

func chefCookbookFromRequest(r *http.Request) (string, bool) {
	name := r.PathValue("name")
	if !chefCookbookName.MatchString(name) || containsPathTraversal(name) {
		return "", false
	}
	return name, true
}

func chefVersionFromRequest(r *http.Request) (name, version string, ok bool) {
	name, ok = chefCookbookFromRequest(r)
	if !ok {
		return "", "", false
	}
	version, ok = normalizeChefVersion(r.PathValue("version"))
	return name, version, ok
}

// normalizeChefVersion returns a cookbook version in its dotted form; see
// chef.NormalizeVersion.
func normalizeChefVersion(version string) (string, bool) {
	return chef.NormalizeVersion(version)
}

// encodeChefJSON encodes v without escaping HTML characters; see
// chef.EncodeJSON.
func encodeChefJSON(v any) (json.RawMessage, error) {
	return chef.EncodeJSON(v)
}
