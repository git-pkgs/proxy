package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
)

const (
	homebrewArtifactNamespace  = "homebrew"
	homebrewArtifactRepository = "homebrew/core"
	homebrewMetadataEcosystem  = "homebrew"
)

// HomebrewHandler proxies Homebrew's JSON API without modifying signed files.
type HomebrewHandler struct {
	proxy       *Proxy
	apiUpstream string
}

// NewHomebrewHandler creates a Homebrew JSON API handler.
func NewHomebrewHandler(proxy *Proxy, apiUpstream string) *HomebrewHandler {
	return &HomebrewHandler{
		proxy:       proxy,
		apiUpstream: strings.TrimSuffix(apiUpstream, "/"),
	}
}

// RegisterHomebrewArtifacts routes homebrew/core OCI requests to its configured
// registry and blocks other homebrew repositories from reaching the default
// OCI registry.
func RegisterHomebrewArtifacts(container *ContainerHandler, artifactUpstream string) {
	container.BlockRegistry(homebrewArtifactNamespace)
	container.RegisterRegistry(homebrewArtifactRepository, artifactUpstream)
}

// Routes returns the Homebrew JSON API handler. Mount this at /homebrew.
func (h *HomebrewHandler) Routes() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		requestPath := strings.TrimPrefix(r.URL.EscapedPath(), "/")
		if requestPath == "" || containsPathTraversal(requestPath) {
			http.NotFound(w, r)
			return
		}

		upstreamURL := h.apiUpstream + "/" + requestPath
		if r.URL.RawQuery != "" {
			upstreamURL += "?" + r.URL.RawQuery
		}

		h.proxy.ProxyCached(w, r, upstreamURL, homebrewMetadataEcosystem, homebrewMetadataCacheKey(requestPath, r.URL.RawQuery), r.Header.Get("Accept"))
	})
}

func homebrewMetadataCacheKey(requestPath, rawQuery string) string {
	sum := sha256.Sum256([]byte(requestPath + "\x00" + rawQuery))
	return hex.EncodeToString(sum[:])
}
