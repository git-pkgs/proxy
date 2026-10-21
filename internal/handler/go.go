package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/retention"
	"github.com/git-pkgs/registries/fetch"
)

const (
	goUpstream      = "https://proxy.golang.org"
	asciiCaseOffset = 32 // difference between lowercase and uppercase ASCII letters
)

// Module zips are stored under pkg:golang/{module path}, which the PURL
// rules lowercase, so a package rule covers a module whatever case either
// is written in. A major version suffix (/v2) is part of the module path
// and so a module of its own. A key in the escaped form proxy URLs and the
// module cache use (github.com/!azure) does not survive the default
// mapping's round trip and is refused; the handler stores decoded paths,
// so such a key could never match.
func init() {
	retention.Default.Register(retention.Spec{Key: "golang", CanonicalPackage: retention.DefaultCanonical("golang")})
}

// GoHandler handles Go module proxy protocol requests.
type GoHandler struct {
	proxy        *Proxy
	upstreamURL  string
	proxyURL     string
	sumDBs       map[string]string
	sumDBOffline bool
	sumDBProbes  goSumDBProbes
}

// NewGoHandler creates a new Go module proxy handler.
func NewGoHandler(proxy *Proxy, proxyURL string) *GoHandler {
	return &GoHandler{
		proxy:       proxy,
		upstreamURL: goUpstream,
		proxyURL:    strings.TrimSuffix(proxyURL, "/"),
		sumDBs:      map[string]string{defaultGoSumDBName: defaultGoSumDBURL},
	}
}

// NewGoHandlerWithUpstream creates a Go module handler with a custom upstream.
func NewGoHandlerWithUpstream(proxy *Proxy, proxyURL, upstreamURL string) *GoHandler {
	h := NewGoHandler(proxy, proxyURL)
	h.upstreamURL = configuredUpstreamURL(upstreamURL, goUpstream)
	return h
}

// Routes returns the HTTP handler for Go proxy requests.
func (h *GoHandler) Routes() http.Handler {
	// Go module paths can contain slashes, so just use the handler directly
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.handleRequest(w, r)
	})
}

// handleRequest routes Go proxy requests based on the URL pattern.
func (h *GoHandler) handleRequest(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")

	// Checksum database requests go to the database, not the module upstream
	if rest, ok := strings.CutPrefix(path, "sumdb/"); ok {
		h.handleSumDB(w, r, rest)
		return
	}

	// Check for @v/ pattern to identify module requests
	if idx := strings.Index(path, "/@v/"); idx >= 0 {
		module := path[:idx]
		rest := path[idx+4:] // after "/@v/"

		decodedMod := decodeGoModule(module)
		switch {
		case rest == "list":
			// GET /{module}/@v/list - list versions
			h.proxyCached(w, r, decodedMod+"/@v/list")

		case strings.HasSuffix(rest, ".info"):
			// GET /{module}/@v/{version}.info - version metadata
			h.proxyCached(w, r, decodedMod+"/@v/"+rest)

		case strings.HasSuffix(rest, ".mod"):
			// GET /{module}/@v/{version}.mod - go.mod file
			h.proxyCached(w, r, decodedMod+"/@v/"+rest)

		case strings.HasSuffix(rest, ".zip"):
			// GET /{module}/@v/{version}.zip - source archive (cache this)
			version := strings.TrimSuffix(rest, ".zip")
			h.handleDownload(w, r, module, version)

		default:
			http.NotFound(w, r)
		}
		return
	}

	// Check for @latest
	if strings.HasSuffix(path, "/@latest") {
		module := strings.TrimSuffix(path, "/@latest")
		h.proxyCached(w, r, decodeGoModule(module)+"/@latest")
		return
	}

	http.NotFound(w, r)
}

// handleDownload serves a module zip, fetching and caching from upstream if needed.
func (h *GoHandler) handleDownload(w http.ResponseWriter, r *http.Request, module, version string) {
	// Decode module path (! followed by lowercase = uppercase)
	decodedModule := decodeGoModule(module)
	filename := goZipFilename(decodedModule, version)

	h.proxy.Logger.Info("go module download request",
		"module", decodedModule, "version", version)

	downloadURL := h.upstreamURL + r.URL.Path
	h.discardOtherModulesZip(decodedModule, module, version, filename)
	result, err := h.proxy.GetOrFetchArtifactFromURL(
		r.Context(), "golang", decodedModule, version, filename, downloadURL,
	)
	if err != nil {
		if errors.Is(err, fetch.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if errors.Is(err, ErrArtifactBlocked) || errors.Is(err, ErrVersionDenied) {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		if writeUpstreamHeld(w, err) {
			return
		}
		h.proxy.Logger.Error("failed to get artifact", "error", err)
		http.Error(w, "failed to fetch module", http.StatusBadGateway)
		return
	}

	ServeArtifactRequest(w, r, result)
}

// goZipFilename is the name a module zip is cached under. Module paths are
// case-sensitive, but the zip is stored under the module's PURL, which
// lowercases golang names, so github.com/Sirupsen/logrus and
// github.com/sirupsen/logrus share one package. A path with uppercase
// letters therefore gets a hash of its exact spelling in the filename. An
// all-lowercase path keeps the plain name, so existing cache entries stay
// valid.
func goZipFilename(module, version string) string {
	base := lastComponent(module) + "@" + version
	if module == strings.ToLower(module) {
		return base + ".zip"
	}
	digest := sha256.Sum256([]byte(module))
	return base + "~" + hex.EncodeToString(digest[:goModuleHashBytes]) + ".zip"
}

// goModuleHashBytes is how much of a module path's hash a cached zip's
// filename carries: enough to tell apart paths that differ only in case.
const goModuleHashBytes = 8

// discardOtherModulesZip drops a cached zip that another module stored
// under filename. Before module paths kept their case, a module with
// uppercase letters was cached under the plain filename of its lowercase
// twin, and whichever came first was served for both. The upstream URL a
// zip was fetched from names its module in escaped form, so a record whose
// URL names a different module is discarded and fetched again. Only the
// module path is compared, so pointing upstream.go elsewhere keeps the cache.
func (h *GoHandler) discardOtherModulesZip(module, escapedModule, version, filename string) {
	_, versionPURL, err := packagePURLStrings("golang", module, version)
	if err != nil {
		return
	}
	record, err := h.proxy.DB.GetArtifact(versionPURL, filename)
	if err != nil || record == nil || !record.StoragePath.Valid || record.Source == database.SourceLocal {
		return
	}
	// Only a URL that names another Go module proves the zip is someone
	// else's; a record of unknown origin is kept.
	if !strings.Contains(record.UpstreamURL, "/@v/") ||
		strings.HasSuffix(record.UpstreamURL, "/"+escapedModule+"/@v/"+version+".zip") {
		return
	}
	h.proxy.Logger.Warn("cached zip belongs to a module that differs only in case, discarding",
		"module", module, "version", version, "cached_from", record.UpstreamURL)
	if err := h.proxy.DB.DiscardArtifact(versionPURL, filename, record.StoragePath.String); err != nil {
		h.proxy.Logger.Warn("failed to discard cached zip", "module", module, "version", version, "error", err)
	}
}

// proxyCached forwards a request with metadata caching.
func (h *GoHandler) proxyCached(w http.ResponseWriter, r *http.Request, cacheKey string) {
	h.proxy.ProxyCached(w, r, h.upstreamURL+r.URL.Path, "golang", cacheKey, "*/*")
}

// decodeGoModule decodes an encoded module path.
// In the encoding, uppercase letters are represented as "!" followed by lowercase.
func decodeGoModule(encoded string) string {
	var b strings.Builder
	for i := 0; i < len(encoded); i++ {
		if encoded[i] == '!' && i+1 < len(encoded) {
			b.WriteByte(encoded[i+1] - asciiCaseOffset) // lowercase to uppercase
			i++
		} else {
			b.WriteByte(encoded[i])
		}
	}
	return b.String()
}

// lastComponent returns the last path component of a module path.
func lastComponent(path string) string {
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		return path[idx+1:]
	}
	return path
}
