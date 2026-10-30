package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/git-pkgs/proxy/internal/jsonscan"
)

// npmPackagePrefix and npmDistTagsSuffix frame the path `npm dist-tag ls`
// reads: /-/package/{name}/dist-tags, with a scoped name's slash encoded.
const (
	npmPackagePrefix  = "/-/package/"
	npmDistTagsSuffix = "/dist-tags"
)

// handleDistTags answers `npm dist-tag ls` from the dist-tags of the
// packument the proxy serves, not from upstream's, so a tag never names a
// version cooldown or the denylist holds back, and pushed versions and
// package routes count as they do for the packument. Changing tags is not
// supported; Routes answers anything but GET with 405.
func (h *NPMHandler) handleDistTags(w http.ResponseWriter, r *http.Request) {
	packageName := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, npmPackagePrefix), npmDistTagsSuffix)
	if !validNPMPackageName(packageName) {
		JSONError(w, http.StatusBadRequest, "invalid package name")
		return
	}

	packument, stale, err := h.servedPackument(r.Context(), packageName)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		if errors.Is(err, ErrUpstreamNotFound) || errors.Is(err, ErrPackageNotFound) {
			JSONError(w, http.StatusNotFound, ErrPackageNotFound.Error())
			return
		}
		if writeUpstreamHeld(w, err) {
			return
		}
		h.proxy.Logger.Error("failed to read npm dist-tags", "package", packageName, "error", err)
		JSONError(w, http.StatusBadGateway, "failed to fetch from upstream")
		return
	}

	tags, err := jsonscan.Lookup(packument, "dist-tags")
	if err != nil || len(tags) == 0 || tags[0] != '{' {
		tags = []byte("{}")
	}
	markStale(w, stale)
	w.Header().Set(headerContentType, contentTypeJSON)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(tags)
}

// servedPackument returns the packument of packageName as the proxy serves
// it: from the package's registry or route, filtered by cooldown and the
// denylist, with pushed versions added, or built from the pushed versions
// alone in a hosted-only scope. stale reports a cached copy served because
// upstream failed.
func (h *NPMHandler) servedPackument(ctx context.Context, packageName string) (body []byte, stale bool, err error) {
	local, err := h.npmLocalEntries(packageName)
	if err != nil {
		return nil, false, err
	}
	if h.proxy.hostedOnly("npm", packageName) {
		if len(local) == 0 {
			return nil, false, ErrPackageNotFound
		}
		body, err := localNPMPackument(packageName, local)
		return body, false, err
	}

	registryURL, cacheKey := h.sourceFor(packageName)
	_, registryPath := npmMetadataPath(packageName)
	upstream := h.proxy.fetchMetadataDocument(ctx, "npm", cacheKey, registryURL+"/"+registryPath, h.packumentAccept(), h.packumentValidator())
	switch {
	case errors.Is(upstream.err, ErrUpstreamNotFound) && len(local) > 0:
		body, err := localNPMPackument(packageName, local)
		return body, false, err
	case upstream.err != nil:
		return nil, false, upstream.err
	case len(local) > 0:
		body, _, err := h.rewriteMetadataWithLocal(packageName, upstream.body, local)
		return body, upstream.stale, err
	}

	rewritten, err := h.proxy.cachedRewrite(ctx, "npm", h.proxyURL, cacheKey, upstream.body, upstream.digest, func(b []byte) ([]byte, []string, error) {
		return h.rewriteMetadataKeeping(packageName, b)
	})
	if err != nil {
		if h.proxy.mustFilterMetadata("npm", packageName) {
			return nil, false, err
		}
		// The packument route serves upstream's document when the rewrite
		// fails and nothing has to be filtered out.
		return upstream.body, upstream.stale, nil
	}
	return rewritten.content(), upstream.stale, nil
}

// validNPMPackageName reports whether name is a plain or scoped package
// name, without path traversal.
func validNPMPackageName(name string) bool {
	if name == "" || containsPathTraversal(name) || strings.ContainsAny(name, "\\") {
		return false
	}
	scope, rest, scoped := strings.Cut(name, "/")
	if !scoped {
		return !strings.HasPrefix(name, "@")
	}
	return len(scope) > 1 && scope[0] == '@' && rest != "" && !strings.Contains(rest, "/")
}
