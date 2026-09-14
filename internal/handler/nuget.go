package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	nugetUpstream       = "https://api.nuget.org"
	nugetSearchUpstream = "https://azuresearch-usnc.nuget.org"
)

// NuGetHandler handles NuGet V3 API protocol requests.
type NuGetHandler struct {
	proxy       *Proxy
	upstreamURL string
	searchURL   string
	proxyURL    string
}

// NewNuGetHandler creates a new NuGet protocol handler.
func NewNuGetHandler(proxy *Proxy, proxyURL string) *NuGetHandler {
	return &NuGetHandler{
		proxy:       proxy,
		upstreamURL: nugetUpstream,
		searchURL:   nugetSearchUpstream,
		proxyURL:    strings.TrimSuffix(proxyURL, "/"),
	}
}

// NewNuGetHandlerWithUpstreams creates a NuGet handler with custom API and
// search upstreams.
func NewNuGetHandlerWithUpstreams(proxy *Proxy, proxyURL, upstreamURL, searchURL string) *NuGetHandler {
	h := NewNuGetHandler(proxy, proxyURL)
	h.upstreamURL = configuredUpstreamURL(upstreamURL, nugetUpstream)
	h.searchURL = configuredUpstreamURL(searchURL, nugetSearchUpstream)
	return h
}

// Routes returns the HTTP handler for NuGet requests.
func (h *NuGetHandler) Routes() http.Handler {
	mux := http.NewServeMux()

	// V3 API service index
	mux.HandleFunc("GET /v3/index.json", h.handleServiceIndex)

	// Package content (downloads)
	mux.HandleFunc("GET /v3-flatcontainer/{id}/{version}/{filename}", h.handleDownload)
	mux.HandleFunc("GET /v3-flatcontainer/{id}/index.json", h.handleVersionList)

	// Registration (package metadata) - use prefix matching since {version}.json isn't allowed
	for _, prefix := range nugetRegistrationPrefixes {
		mux.HandleFunc("GET "+prefix, h.handleRegistration)
	}

	// Search
	mux.HandleFunc("GET /query", h.proxyUpstream)

	// Autocomplete
	mux.HandleFunc("GET /autocomplete", h.proxyUpstream)

	return mux
}

// handleServiceIndex serves the NuGet V3 service index with rewritten URLs.
func (h *NuGetHandler) handleServiceIndex(w http.ResponseWriter, r *http.Request) {
	h.proxy.Logger.Info("nuget service index request")

	upstreamURL := h.upstreamURL + "/v3/index.json"

	body, _, err := h.proxy.FetchOrCacheMetadata(r.Context(), "nuget", "_service_index", upstreamURL)
	if err != nil {
		if errors.Is(err, ErrUpstreamNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		h.proxy.Logger.Error("upstream request failed", "error", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}

	rewritten, err := h.rewriteServiceIndex(body)
	if err != nil {
		if h.cooldownEnabled() {
			h.nugetMetadataError(w, err)
			return
		}
		h.proxy.Logger.Warn("failed to rewrite service index, proxying original", "error", err)
		w.Header().Set(headerContentType, "application/json")
		_, _ = w.Write(body)
		return
	}

	w.Header().Set(headerContentType, "application/json")
	_, _ = w.Write(rewritten)
}

// rewriteServiceIndex rewrites service URLs in the index to point at this proxy.
func (h *NuGetHandler) rewriteServiceIndex(body []byte) ([]byte, error) {
	var index map[string]any
	if err := json.Unmarshal(body, &index); err != nil {
		return nil, err
	}

	resources, ok := index["resources"].([]any)
	if !ok {
		return body, nil
	}

	for _, res := range resources {
		rmap, ok := res.(map[string]any)
		if !ok {
			continue
		}

		id, _ := rmap["@id"].(string)
		rtype, _ := rmap["@type"].(string)

		// Rewrite URLs for services we proxy. The service type determines the
		// local route because an upstream index may advertise a different host.
		if id != "" {
			rmap["@id"] = h.rewriteNuGetURL(id, rtype)
		}
	}

	return json.Marshal(index)
}

// rewriteNuGetURL rewrites a NuGet service URL based on its advertised type.
// Service types the proxy does not handle are returned unchanged.
func (h *NuGetHandler) rewriteNuGetURL(origURL, serviceType string) string {
	switch serviceType {
	case "PackageBaseAddress/3.0.0":
		return h.proxyURL + "/nuget/v3-flatcontainer/"
	case "RegistrationsBaseUrl", "RegistrationsBaseUrl/3.0.0-beta", "RegistrationsBaseUrl/3.0.0-rc":
		return h.proxyURL + "/nuget/v3/registration5-semver1/"
	case "RegistrationsBaseUrl/3.4.0":
		return h.proxyURL + "/nuget/v3/registration5-gz-semver1/"
	case "RegistrationsBaseUrl/3.6.0", "RegistrationsBaseUrl/Versioned":
		return h.proxyURL + "/nuget/v3/registration5-gz-semver2/"
	case "SearchQueryService", "SearchQueryService/3.0.0-rc", "SearchQueryService/3.5.0":
		return h.proxyURL + "/nuget/query"
	case "SearchAutocompleteService", "SearchAutocompleteService/3.5.0":
		return h.proxyURL + "/nuget/autocomplete"
	default:
		return origURL
	}
}

// handleDownload serves a package file, fetching and caching from upstream if needed.
func (h *NuGetHandler) handleDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	version := r.PathValue("version")
	filename := r.PathValue("filename")

	if id == "" || version == "" || filename == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	if h.cooldownEnabled() {
		allowed, err := h.nugetDownloadAllowed(r.Context(), id, version)
		if err != nil {
			h.nugetMetadataError(w, err)
			return
		}
		if !allowed {
			JSONError(w, http.StatusNotFound, "version not found")
			return
		}
	}

	// Only cache .nupkg files
	if !strings.HasSuffix(filename, ".nupkg") {
		h.proxyUpstream(w, r)
		return
	}

	h.proxy.Logger.Info("nuget download request",
		"id", id, "version", version, "filename", filename)

	// NuGet package IDs are case-insensitive, lowercase for storage
	name := strings.ToLower(id)
	upstreamURL := fmt.Sprintf("%s/v3-flatcontainer/%s/%s/%s", h.upstreamURL, name, version, filename)

	result, err := h.proxy.GetOrFetchArtifactFromURL(r.Context(), "nuget", name, version, filename, upstreamURL)
	if err != nil {
		h.proxy.serveArtifactError(w, err, "failed to fetch package")
		return
	}

	ServeArtifact(w, result)
}

// proxyUpstream forwards a request to NuGet without caching.
func (h *NuGetHandler) proxyUpstream(w http.ResponseWriter, r *http.Request) {
	// Build upstream URL based on the path
	upstreamURL := h.buildUpstreamURL(r)

	h.proxy.Logger.Debug("proxying to upstream", "url", upstreamURL)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
	}

	// Copy accept-encoding for compression
	if ae := r.Header.Get(headerAcceptEncoding); ae != "" {
		req.Header.Set(headerAcceptEncoding, ae)
	}

	resp, err := h.proxy.HTTPClient.Do(req)
	if err != nil {
		h.proxy.Logger.Error("upstream request failed", "error", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}

	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// buildUpstreamURL constructs the upstream URL for a request.
func (h *NuGetHandler) buildUpstreamURL(r *http.Request) string {
	path := r.URL.Path

	// Handle query and autocomplete which go to azuresearch
	if strings.HasPrefix(path, "/query") || strings.HasPrefix(path, "/autocomplete") {
		return h.searchURL + path + "?" + r.URL.RawQuery
	}

	return h.upstreamURL + path
}
