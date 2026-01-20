package handler

import (
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	goUpstream = "https://proxy.golang.org"
)

// GoHandler handles Go module proxy protocol requests.
type GoHandler struct {
	proxy       *Proxy
	upstreamURL string
	proxyURL    string
}

// NewGoHandler creates a new Go module proxy handler.
func NewGoHandler(proxy *Proxy, proxyURL string) *GoHandler {
	return &GoHandler{
		proxy:       proxy,
		upstreamURL: goUpstream,
		proxyURL:    strings.TrimSuffix(proxyURL, "/"),
	}
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

	// Sumdb requests - proxy through
	if strings.HasPrefix(path, "sumdb/") {
		h.proxyUpstream(w, r)
		return
	}

	// Check for @v/ pattern to identify module requests
	if idx := strings.Index(path, "/@v/"); idx >= 0 {
		module := path[:idx]
		rest := path[idx+4:] // after "/@v/"

		switch {
		case rest == "list":
			// GET /{module}/@v/list - list versions
			h.proxyUpstream(w, r)

		case strings.HasSuffix(rest, ".info"):
			// GET /{module}/@v/{version}.info - version metadata
			h.proxyUpstream(w, r)

		case strings.HasSuffix(rest, ".mod"):
			// GET /{module}/@v/{version}.mod - go.mod file
			h.proxyUpstream(w, r)

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
		h.proxyUpstream(w, r)
		return
	}

	http.NotFound(w, r)
}

// handleDownload serves a module zip, fetching and caching from upstream if needed.
func (h *GoHandler) handleDownload(w http.ResponseWriter, r *http.Request, module, version string) {
	// Decode module path (! followed by lowercase = uppercase)
	decodedModule := decodeGoModule(module)
	filename := fmt.Sprintf("%s@%s.zip", lastComponent(decodedModule), version)

	h.proxy.Logger.Info("go module download request",
		"module", decodedModule, "version", version)

	result, err := h.proxy.GetOrFetchArtifact(r.Context(), "golang", decodedModule, version, filename)
	if err != nil {
		h.proxy.Logger.Error("failed to get artifact", "error", err)
		http.Error(w, "failed to fetch module", http.StatusBadGateway)
		return
	}

	ServeArtifact(w, result)
}

// proxyUpstream forwards a request to proxy.golang.org without caching.
func (h *GoHandler) proxyUpstream(w http.ResponseWriter, r *http.Request) {
	upstreamURL := h.upstreamURL + r.URL.Path

	h.proxy.Logger.Debug("proxying to upstream", "url", upstreamURL)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.proxy.Logger.Error("upstream request failed", "error", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Copy response headers
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}

	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// decodeGoModule decodes an encoded module path.
// In the encoding, uppercase letters are represented as "!" followed by lowercase.
func decodeGoModule(encoded string) string {
	var b strings.Builder
	for i := 0; i < len(encoded); i++ {
		if encoded[i] == '!' && i+1 < len(encoded) {
			b.WriteByte(encoded[i+1] - 32) // lowercase to uppercase
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
