package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/git-pkgs/proxy/internal/metadata/npm"
)

type npmLocalEntry = npm.LocalVersion

// npmLocalEntries returns the pushed versions of a package as packument
// entries, or nil when there are none or hosted packages are disabled.
func (h *NPMHandler) npmLocalEntries(packageName string) ([]npmLocalEntry, error) {
	local, err := h.proxy.localVersions("npm", packageName)
	if err != nil || len(local) == 0 {
		return nil, err
	}
	entries := make([]npmLocalEntry, 0, len(local))
	for _, lv := range local {
		var manifest map[string]any
		if err := json.Unmarshal(lv.Manifest, &manifest); err != nil {
			return nil, fmt.Errorf("decoding manifest of %s@%s: %w", packageName, lv.Version, err)
		}
		manifest["name"] = packageName
		manifest["version"] = lv.Version
		manifest["_id"] = packageName + "@" + lv.Version
		manifest["dist"] = map[string]string{
			"tarball":   h.proxyTarballURL(packageName, lv.Version, lv.Filename),
			"integrity": lv.Integrity,
		}
		entry, err := json.Marshal(manifest)
		if err != nil {
			return nil, err
		}
		entries = append(entries, npmLocalEntry{Version: lv.Version, Entry: entry, PublishedAt: lv.PushedAt.Format(time.RFC3339)})
	}
	return entries, nil
}

func findNPMLocalEntry(local []npmLocalEntry, version string) (npmLocalEntry, bool) {
	for _, e := range local {
		if e.Version == version {
			return e, true
		}
	}
	return npmLocalEntry{}, false
}

// npmMetadataRequest is a packument or version metadata request.
type npmMetadataRequest struct {
	packageName      string
	specifier        string
	versionLookup    bool
	upstreamURL      string
	accept           string
	metadataCacheKey string
}

// serveLocalMetadata answers the metadata requests that pushed versions
// decide, and reports whether it did: a request for a pushed version, any
// request for a package in a hosted-only scope, and a packument of a package
// with pushed versions. Everything else takes the usual upstream path.
func (h *NPMHandler) serveLocalMetadata(w http.ResponseWriter, r *http.Request, req npmMetadataRequest) bool {
	local, err := h.npmLocalEntries(req.packageName)
	if err != nil {
		h.proxy.Logger.Error("failed to read pushed npm versions", "package", req.packageName, "error", err)
		JSONError(w, http.StatusInternalServerError, "failed to read pushed versions")
		return true
	}
	switch e, pushed := findNPMLocalEntry(local, req.specifier); {
	case req.versionLookup && pushed:
		w.Header().Set(headerContentType, contentTypeJSON)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(e.Entry)
	case h.proxy.hostedOnly("npm", req.packageName):
		h.serveHostedOnlyMetadata(w, r, req.packageName, req.specifier, req.versionLookup, req.metadataCacheKey, local)
	case len(local) > 0 && !req.versionLookup:
		h.serveMetadataWithLocal(w, r, req.packageName, req.upstreamURL, req.accept, req.metadataCacheKey, local)
	default:
		return false
	}
	return true
}

// serveMetadataWithLocal serves a packument with the pushed versions of the
// package added. It bypasses the rewrite cache, whose entries are keyed by the
// upstream document alone, so a push or delete shows up on the next request.
// When the upstream does not know the package, the packument holds only the
// pushed versions.
func (h *NPMHandler) serveMetadataWithLocal(w http.ResponseWriter, r *http.Request, packageName, upstreamURL, accept, metadataCacheKey string, local []npmLocalEntry) {
	upstream := h.proxy.fetchMetadataDocument(r.Context(), "npm", metadataCacheKey, upstreamURL, accept)
	var out []byte
	var err error
	switch {
	case errors.Is(upstream.err, ErrUpstreamNotFound):
		out, err = localNPMPackument(packageName, local)
	case upstream.err != nil:
		if writeUpstreamHeld(w, upstream.err) {
			return
		}
		h.proxy.Logger.Error("failed to fetch npm metadata", "error", upstream.err)
		JSONError(w, http.StatusBadGateway, "failed to fetch from upstream")
		return
	default:
		out, _, err = h.rewriteMetadataWithLocal(packageName, upstream.body, local)
	}
	if err != nil {
		if errors.Is(err, ErrVersionDenied) {
			JSONError(w, http.StatusForbidden, err.Error())
			return
		}
		h.proxy.Logger.Error("failed to add pushed versions to npm metadata", "package", packageName, "error", err)
		JSONError(w, http.StatusBadGateway, "failed to rewrite package metadata")
		return
	}
	serveRewrittenMetadata(w, r, h.proxy.uncachedRewrite("npm", h.proxyURL, metadataCacheKey, out))
}

// serveHostedOnlyMetadata answers a metadata request for a package in a
// hosted-only scope from its pushed versions alone.
func (h *NPMHandler) serveHostedOnlyMetadata(w http.ResponseWriter, r *http.Request, packageName, specifier string, versionLookup bool, metadataCacheKey string, local []npmLocalEntry) {
	if len(local) == 0 {
		JSONError(w, http.StatusNotFound, ErrPackageNotFound.Error())
		return
	}
	if versionLookup {
		if specifier == "latest" {
			specifier = latestNPMVersion(local)
		}
		e, ok := findNPMLocalEntry(local, specifier)
		if !ok {
			JSONError(w, http.StatusNotFound, ErrVersionNotFound.Error())
			return
		}
		w.Header().Set(headerContentType, contentTypeJSON)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(e.Entry)
		return
	}
	out, err := localNPMPackument(packageName, local)
	if err != nil {
		h.proxy.Logger.Error("failed to build npm metadata from pushed versions", "package", packageName, "error", err)
		JSONError(w, http.StatusInternalServerError, "failed to build package metadata")
		return
	}
	serveRewrittenMetadata(w, r, h.proxy.uncachedRewrite("npm", h.proxyURL, metadataCacheKey, out))
}

// localNPMPackument builds a packument from pushed versions alone.
func localNPMPackument(packageName string, local []npmLocalEntry) ([]byte, error) {
	versions := make(map[string]json.RawMessage, len(local))
	times := make(map[string]string, len(local))
	for _, e := range local {
		versions[e.Version] = e.Entry
		times[e.Version] = e.PublishedAt
	}
	return json.Marshal(map[string]any{
		"name":      packageName,
		"dist-tags": map[string]string{"latest": latestNPMVersion(local)},
		"versions":  versions,
		"time":      times,
	})
}

// latestNPMVersion returns the highest pushed version, or the last pushed
// when a version is not semver.
func latestNPMVersion(local []npmLocalEntry) string {
	latest, latestTime := "", ""
	var latestSemver *semver.Version
	semverOnly := true
	for _, e := range local {
		if e.PublishedAt > latestTime {
			latest, latestTime = e.Version, e.PublishedAt
		}
		v, err := semver.NewVersion(e.Version)
		if err != nil {
			semverOnly = false
			continue
		}
		if latestSemver == nil || v.GreaterThan(latestSemver) {
			latestSemver = v
		}
	}
	if semverOnly && latestSemver != nil {
		return latestSemver.Original()
	}
	return latest
}
