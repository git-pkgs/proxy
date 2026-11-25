package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/git-pkgs/proxy/internal/metadata/cargo"
	"github.com/git-pkgs/proxy/internal/retention"
)

const (
	cargoUpstream     = "https://index.crates.io"
	cargoDownloadBase = "https://static.crates.io/crates"
)

// Crates are stored under pkg:cargo/{name}, with the name cargo puts in the
// download URL, which is the name as published. purl keeps cargo names as
// they are, so package rules use the default key mapping and have to match
// that name exactly.
func init() {
	retention.Default.Register(retention.Spec{
		Key:              "cargo",
		CanonicalPackage: retention.DefaultCanonical("cargo"),
		CompareVersions:  retention.VersionOrder("cargo"),
	})
}

// CargoHandler handles cargo registry protocol requests.
type CargoHandler struct {
	proxy       *Proxy
	indexURL    string
	downloadURL string
	proxyURL    string
}

// NewCargoHandler creates a new cargo protocol handler.
func NewCargoHandler(proxy *Proxy, proxyURL, indexURL, downloadURL string) *CargoHandler {
	if strings.TrimSpace(indexURL) == "" {
		indexURL = cargoUpstream
	}
	if strings.TrimSpace(downloadURL) == "" {
		downloadURL = cargoDownloadBase
	}

	return &CargoHandler{
		proxy:       proxy,
		indexURL:    strings.TrimSuffix(indexURL, "/"),
		downloadURL: strings.TrimSuffix(downloadURL, "/"),
		proxyURL:    strings.TrimSuffix(proxyURL, "/"),
	}
}

// Routes returns the HTTP handler for cargo requests.
// Mount this at /cargo on your router.
func (h *CargoHandler) Routes() http.Handler {
	mux := http.NewServeMux()

	// Config endpoint
	mux.HandleFunc("GET /config.json", h.handleConfig)

	// Sparse index endpoints
	// Crate names 1-2 chars: /1/{name} or /2/{name}
	// Crate names 3 chars: /3/{first_char}/{name}
	// Crate names 4+ chars: /{first_two}/{second_two}/{name}
	mux.HandleFunc("GET /1/{name}", h.handleIndex)
	mux.HandleFunc("GET /2/{name}", h.handleIndex)
	mux.HandleFunc("GET /3/{a}/{name}", h.handleIndex)
	mux.HandleFunc("GET /{a}/{b}/{name}", h.handleIndex)

	// Download endpoint
	mux.HandleFunc("GET /crates/{name}/{version}/download", h.handleDownload)

	return mux
}

// CargoConfig is the registry configuration returned by config.json.
type CargoConfig struct {
	DL  string `json:"dl"`
	API string `json:"api,omitempty"`
}

// handleConfig returns the registry configuration.
func (h *CargoHandler) handleConfig(w http.ResponseWriter, r *http.Request) {
	config := CargoConfig{
		DL: cargo.DownloadTemplate(h.proxyURL),
	}

	w.Header().Set(headerContentType, "application/json")
	_ = json.NewEncoder(w).Encode(config)
}

// handleIndex proxies the crate index from upstream.
func (h *CargoHandler) handleIndex(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	h.proxy.Logger.Debug("cargo index request", "crate", name)

	indexPath := h.buildIndexPath(name)
	upstreamURL := fmt.Sprintf("%s/%s", h.indexURL, indexPath)

	body, contentType, err := h.proxy.FetchOrCacheMetadata(r.Context(), "cargo", name, upstreamURL, "text/plain")
	if err != nil {
		if errors.Is(err, ErrUpstreamNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if h.proxy.writeUpstreamRefusal(w, err) {
			return
		}
		h.proxy.Logger.Error("failed to fetch upstream index", "error", err)
		http.Error(w, "failed to fetch from upstream", http.StatusBadGateway)
		return
	}

	if contentType == "" {
		contentType = "text/plain; charset=utf-8"
	}

	w.Header().Set(headerContentType, contentType)
	w.WriteHeader(http.StatusOK)
	h.applyCooldownFiltering(w, body)
}

// applyCooldownFiltering writes the index body with the versions the
// denylist names and those cooldown holds back left out (cargo.Filter).
func (h *CargoHandler) applyCooldownFiltering(downstreamResponse http.ResponseWriter, body []byte) {
	out, err := cargo.Filter(body, h.filterOptions())
	_, _ = downstreamResponse.Write(out)
	if err != nil {
		h.proxy.Logger.Error("error reading index response", "error", err)
	}
}

// filterOptions are the policy the index filter applies. The callbacks log
// and record what it holds back.
func (h *CargoHandler) filterOptions() cargo.Options {
	o := cargo.Options{
		EvaluatedAt: time.Now(),
		OnWithheld: func(name, version string, publishedAt time.Time) {
			h.proxy.Logger.Info("cooldown: filtering cargo version",
				"crate", name, "version", version,
				"published", publishedAt.Format(time.RFC3339))
			h.proxy.recordWithheld("cargo", name, version, publishedAt)
		},
		OnInvalidEntry: func(err error) {
			h.proxy.Logger.Error("failed to parse json entry in index", "error", err)
		},
	}
	if policy := h.proxy.cooldown(); policy != nil {
		o.Cooldown = policy
	}
	if h.proxy.Denylist != nil {
		o.Denied = func(name, version string) bool {
			return h.proxy.versionDenied("cargo", name, version)
		}
	}
	return o
}

// publishTime reads a crate version's pubtime from the sparse index, sharing
// the index cache with handleIndex. Entries published before crates.io
// recorded pubtime have none. A cached index that doesn't list the version
// is fetched again (readListed).
func (h *CargoHandler) publishTime(r *http.Request, name, version string) (time.Time, bool, error) {
	upstreamURL := fmt.Sprintf("%s/%s", h.indexURL, h.buildIndexPath(name))
	listed, err := readListed(r.Context(), h.proxy, "cargo", name, upstreamURL, "text/plain", "",
		func(res metadataResult) (listedTime, bool, error) {
			published, ok, listed, err := cargo.PublishTime(res.body, version)
			return listedTime{published, ok}, listed, err
		})
	return listed.published, listed.ok, err
}

// buildIndexPath builds the sparse index path for a crate name.
func (h *CargoHandler) buildIndexPath(name string) string {
	return cargo.IndexPath(name)
}

// handleDownload serves a crate file, fetching and caching from upstream if needed.
func (h *CargoHandler) handleDownload(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	version := r.PathValue("version")

	if name == "" || version == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	filename := fmt.Sprintf("%s-%s.crate", name, version)

	h.proxy.Logger.Debug("cargo download request",
		"crate", name, "version", version, "filename", filename)

	if h.proxy.withholdInCooldown(w, r, downloadVersion{
		ecosystem: "cargo", policyName: name, storedName: name, version: version, cacheName: name, filename: filename,
		lookup: func() (time.Time, bool, bool, error) {
			published, ok, err := h.publishTime(r, name, version)
			return published, ok, false, err
		},
	}) {
		return
	}

	downloadURL := fmt.Sprintf(
		"%s/%s/%s",
		h.downloadURL,
		url.PathEscape(name),
		url.PathEscape(filename),
	)
	result, err := h.proxy.GetOrFetchArtifactFromURL(
		r.Context(), "cargo", name, version, filename, downloadURL,
	)
	if err != nil {
		h.proxy.serveArtifactError(w, err, "failed to fetch crate")
		return
	}

	ServeArtifactRequest(w, r, result)
}
