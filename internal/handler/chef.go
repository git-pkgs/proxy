package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
)

const (
	chefEcosystem = "chef"
	chefMountPath = "/chef"
	chefAPIPath   = "/api/v1"
	// chefUpstreamKeyLength is how much of the upstream URL's digest scopes
	// metadata cache keys and archive filenames: enough to keep two upstreams
	// apart.
	chefUpstreamKeyLength = 16
)

var (
	// chefCookbookName matches Supermarket cookbook names: letters, digits,
	// hyphens, underscores and dots, not starting with a dot.
	chefCookbookName = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]*$`)
	// chefVersion matches Chef cookbook versions, which have two or three
	// numeric parts.
	chefVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)?$`)

	errChefDocument = errors.New("unexpected Chef Supermarket document")
)

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
	h.serveDocument(w, r, "universe", h.upstreamURL+"/universe", h.rewriteUniverse)
}

func (h *ChefHandler) handleCookbook(w http.ResponseWriter, r *http.Request) {
	name, ok := chefCookbookFromRequest(r)
	if !ok {
		http.Error(w, "invalid cookbook name", http.StatusBadRequest)
		return
	}
	h.serveDocument(w, r, "cookbooks/"+name, h.upstreamCookbookURL(name), func(body []byte) ([]byte, error) {
		return h.rewriteCookbook(name, body)
	})
}

func (h *ChefHandler) handleVersion(w http.ResponseWriter, r *http.Request) {
	name, version, ok := chefVersionFromRequest(r)
	if !ok {
		http.Error(w, "invalid cookbook version", http.StatusBadRequest)
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

	upstream := h.proxy.fetchMetadataDocument(r.Context(), chefEcosystem, cacheKey, upstreamURL, contentTypeJSON)
	if err := upstream.err; err != nil {
		if errors.Is(err, ErrUpstreamNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if writeUpstreamHeld(w, err) {
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
		// Serving the document unrewritten would send clients straight to
		// the upstream, bypassing the cache.
		h.proxy.Logger.Warn("failed to rewrite Chef Supermarket document", "url", upstreamURL, "error", err)
		http.Error(w, "invalid Chef Supermarket response", http.StatusBadGateway)
		return
	}
	serveRewrittenMetadata(w, r, rewritten)
}

// rewriteUniverse points every version's location_path and download_url at
// the proxy. The universe maps cookbook names to versions to their location
// and dependencies; dependencies and any other field are copied as they are.
func (h *ChefHandler) rewriteUniverse(body []byte) ([]byte, error) {
	var universe map[string]map[string]map[string]json.RawMessage
	if err := json.Unmarshal(body, &universe); err != nil {
		return nil, fmt.Errorf("%w: universe: %w", errChefDocument, err)
	}

	locationPath, err := jsonString(h.apiURL())
	if err != nil {
		return nil, err
	}
	for name, versions := range universe {
		for version, entry := range versions {
			if entry == nil {
				continue
			}
			downloadURL, err := jsonString(h.downloadURL(name, version))
			if err != nil {
				return nil, err
			}
			entry["location_path"] = locationPath
			entry["download_url"] = downloadURL
		}
	}
	return encodeChefJSON(universe)
}

// rewriteCookbook points a cookbook's version URLs at the proxy.
func (h *ChefHandler) rewriteCookbook(name string, body []byte) ([]byte, error) {
	var cookbook map[string]json.RawMessage
	if err := json.Unmarshal(body, &cookbook); err != nil {
		return nil, fmt.Errorf("%w: cookbook %s: %w", errChefDocument, name, err)
	}

	if raw, ok := cookbook["latest_version"]; ok {
		rewritten, err := h.rewriteVersionURL(name, raw)
		if err != nil {
			return nil, err
		}
		cookbook["latest_version"] = rewritten
	}
	if raw, ok := cookbook["versions"]; ok {
		var versions []json.RawMessage
		if err := json.Unmarshal(raw, &versions); err != nil {
			return nil, fmt.Errorf("%w: cookbook %s versions: %w", errChefDocument, name, err)
		}
		for i, versionURL := range versions {
			rewritten, err := h.rewriteVersionURL(name, versionURL)
			if err != nil {
				return nil, err
			}
			versions[i] = rewritten
		}
		encoded, err := encodeChefJSON(versions)
		if err != nil {
			return nil, err
		}
		cookbook["versions"] = encoded
	}
	return encodeChefJSON(cookbook)
}

// rewriteVersion points a cookbook version's cookbook and file URLs at the
// proxy.
func (h *ChefHandler) rewriteVersion(name, version string, body []byte) ([]byte, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, fmt.Errorf("%w: cookbook %s version %s: %w", errChefDocument, name, version, err)
	}

	if _, ok := document["cookbook"]; ok {
		cookbookURL, err := jsonString(h.cookbookURL(name))
		if err != nil {
			return nil, err
		}
		document["cookbook"] = cookbookURL
	}
	if _, ok := document["file"]; ok {
		fileURL, err := jsonString(h.downloadURL(name, version))
		if err != nil {
			return nil, err
		}
		document["file"] = fileURL
	}
	return encodeChefJSON(document)
}

// rewriteVersionURL rewrites an upstream cookbook version URL, a JSON string
// ending in /versions/{version}, to the proxy. Anything else, such as null,
// is returned unchanged.
func (h *ChefHandler) rewriteVersionURL(name string, raw json.RawMessage) (json.RawMessage, error) {
	var versionURL string
	if err := json.Unmarshal(raw, &versionURL); err != nil {
		return raw, nil //nolint:nilerr // not a string, so not a URL to rewrite
	}
	parsed, err := url.Parse(versionURL)
	if err != nil {
		return raw, nil //nolint:nilerr // not a URL, so nothing to rewrite
	}
	version, ok := normalizeChefVersion(path.Base(parsed.Path))
	if !ok {
		return raw, nil
	}
	return jsonString(h.versionURL(name, version))
}

func (h *ChefHandler) apiURL() string {
	return h.proxyURL + chefMountPath + chefAPIPath
}

func (h *ChefHandler) cookbookURL(name string) string {
	return h.apiURL() + "/cookbooks/" + url.PathEscape(name)
}

func (h *ChefHandler) versionURL(name, version string) string {
	return h.cookbookURL(name) + "/versions/" + url.PathEscape(version)
}

func (h *ChefHandler) downloadURL(name, version string) string {
	return h.versionURL(name, version) + "/download"
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

// normalizeChefVersion accepts a version in either form Supermarket serves
// (1.2.3, or 1_2_3 as Berkshelf writes it in URLs) and returns the dotted
// form.
func normalizeChefVersion(version string) (string, bool) {
	version = strings.ReplaceAll(version, "_", ".")
	if !chefVersion.MatchString(version) {
		return "", false
	}
	return version, true
}

func jsonString(s string) (json.RawMessage, error) {
	return encodeChefJSON(s)
}

// encodeChefJSON encodes v without escaping HTML characters, so dependency
// constraints such as ">= 1.0" keep their upstream spelling.
func encodeChefJSON(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
