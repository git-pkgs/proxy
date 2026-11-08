package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// composerRootOnlyKeys are composer.json keys that only apply to the root
// project. Repositories leave them out of the version entries they serve.
var composerRootOnlyKeys = []string{"repositories", "config", "minimum-stability", "prefer-stable"}

// composerLocalEntry is a pushed version as Composer metadata lists it.
type composerLocalEntry struct {
	version string
	entry   json.RawMessage
}

// composerLocalEntries returns the pushed versions of a package for one of
// its metadata files: the dev versions for the ~dev file, the tagged ones for
// the other.
func (h *ComposerHandler) composerLocalEntries(packageName string, dev bool) ([]composerLocalEntry, error) {
	local, err := h.proxy.localVersions("composer", packageName)
	if err != nil || len(local) == 0 {
		return nil, err
	}
	var entries []composerLocalEntry
	for _, lv := range local {
		if isDevVersion(lv.Version) != dev {
			continue
		}
		var manifest map[string]any
		if err := json.Unmarshal(lv.Manifest, &manifest); err != nil {
			return nil, fmt.Errorf("decoding manifest of %s %s: %w", packageName, lv.Version, err)
		}
		for _, key := range composerRootOnlyKeys {
			delete(manifest, key)
		}
		manifest["name"] = packageName
		manifest["version"] = lv.Version
		manifest["time"] = lv.PushedAt.Format(time.RFC3339)
		// As for routed packages, keep Composer from reporting installs of
		// a private version to the default repository's notify-batch.
		// Composer reads a null notification-url as absent too.
		if url, ok := manifest["notification-url"]; !ok || url == nil {
			manifest["notification-url"] = ""
		}
		manifest["dist"] = map[string]string{
			"type":      "zip",
			"url":       h.localDistURL(packageName, lv.Version, lv.Filename),
			"reference": lv.SHA256,
			"shasum":    "",
		}
		entry, err := json.Marshal(manifest)
		if err != nil {
			return nil, err
		}
		entries = append(entries, composerLocalEntry{version: lv.Version, entry: entry})
	}
	return entries, nil
}

func (h *ComposerHandler) localDistURL(packageName, version, filename string) string {
	return fmt.Sprintf("%s/composer/files/%s/%s/%s", h.proxyURL, packageName, url.PathEscape(version), filename)
}

// pushedComposerVersion reports whether version is one of the pushed ones.
// Tags are listed with and without a leading v, and both name one version.
func pushedComposerVersion(local []composerLocalEntry, version string) bool {
	for _, e := range local {
		if strings.TrimPrefix(e.version, "v") == strings.TrimPrefix(version, "v") {
			return true
		}
	}
	return false
}

// serveMetadataWithLocal serves a package's metadata with its pushed versions
// added, bypassing the rewrite cache like the npm handler does. When the
// upstream does not know the package, the document holds only the pushed
// versions.
func (h *ComposerHandler) serveMetadataWithLocal(w http.ResponseWriter, r *http.Request, packageName, upstreamURL, cacheKey string, local []composerLocalEntry) {
	upstream := h.proxy.fetchMetadataDocument(r.Context(), "composer", cacheKey, upstreamURL, contentTypeJSON, nil)
	var out []byte
	var err error
	switch {
	case errors.Is(upstream.err, ErrUpstreamNotFound):
		out, err = localComposerDocument(packageName, local)
	case upstream.err != nil:
		if writeUpstreamHeld(w, upstream.err) {
			return
		}
		h.proxy.Logger.Error("upstream request failed", "error", upstream.err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	default:
		out, _, err = h.rewriteMetadataWithLocal(upstream.body, packageName, local)
	}
	if err != nil {
		h.proxy.Logger.Error("failed to add pushed versions to composer metadata", "package", packageName, "error", err)
		http.Error(w, "failed to rewrite package metadata", http.StatusBadGateway)
		return
	}
	markStale(w, upstream.stale)
	serveRewrittenMetadata(w, r, h.proxy.uncachedRewrite("composer", h.proxyURL, cacheKey, out))
}

// serveHostedOnlyMetadata answers a metadata request for a package of a
// hosted-only vendor from its pushed versions alone.
func (h *ComposerHandler) serveHostedOnlyMetadata(w http.ResponseWriter, r *http.Request, packageName, cacheKey string, local []composerLocalEntry) {
	if len(local) == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	out, err := localComposerDocument(packageName, local)
	if err != nil {
		h.proxy.Logger.Error("failed to build composer metadata from pushed versions", "package", packageName, "error", err)
		http.Error(w, "failed to build package metadata", http.StatusInternalServerError)
		return
	}
	serveRewrittenMetadata(w, r, h.proxy.uncachedRewrite("composer", h.proxyURL, cacheKey, out))
}

func localComposerDocument(packageName string, local []composerLocalEntry) ([]byte, error) {
	entries := make([]json.RawMessage, 0, len(local))
	for _, e := range local {
		entries = append(entries, e.entry)
	}
	return json.Marshal(map[string]any{"packages": map[string]any{packageName: entries}})
}
