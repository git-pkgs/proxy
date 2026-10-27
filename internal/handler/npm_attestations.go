package handler

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/git-pkgs/proxy/internal/jsonscan"
)

// errNoAttestations answers a request for a version whose packument entry
// lists no attestations.
var errNoAttestations = errors.New("attestations not found")

// handleAttestations serves /-/npm/v1/attestations/{name}@{version}, which
// `npm audit signatures` reads to verify provenance. Packuments point
// dist.attestations.url here (see npm.AttestationsURL).
//
// The document is fetched from the package's registry, the route's for a
// routed package, at the URL its packument lists for the version when that is
// on the same registry, and otherwise at the registry's standard path. A
// version's attestations never change once published, so they are cached
// like other immutable metadata and served without revalidation.
//
// Versions the denylist blocks get 403 and versions cooldown holds back 404,
// as their tarballs do. Pushed versions carry no upstream attestations, so
// they and packages in hosted-only scopes get 404.
func (h *NPMHandler) handleAttestations(w http.ResponseWriter, r *http.Request) {
	packageName, version, ok := parseNPMAttestationsPath(strings.TrimPrefix(r.URL.Path, npmAttestationsPrefix))
	if !ok {
		JSONError(w, http.StatusBadRequest, "invalid package name")
		return
	}

	if h.proxy.versionDenied("npm", packageName, version) {
		JSONError(w, http.StatusForbidden, ErrVersionDenied.Error()+": "+canonicalVersionPURL("npm", packageName, version))
		return
	}
	local, err := h.npmLocalEntries(packageName)
	if err != nil {
		h.proxy.Logger.Error("failed to read pushed npm versions", "package", packageName, "error", err)
		JSONError(w, http.StatusInternalServerError, "failed to read pushed versions")
		return
	}
	if _, pushed := findNPMLocalEntry(local, version); pushed || h.proxy.hostedOnly("npm", packageName) {
		JSONError(w, http.StatusNotFound, errNoAttestations.Error())
		return
	}

	registryURL, cacheKey := h.sourceFor(packageName)
	metadata := sync.OnceValues(func() ([]byte, error) {
		upstreamURL := registryURL + "/" + url.PathEscape(packageName)
		body, _, _, err := h.proxy.fetchOrCacheMetadata(r.Context(), "npm", cacheKey, upstreamURL, "", h.packumentValidator(), h.packumentAccept())
		return body, err
	})

	inCooldown, err := h.versionInCooldown(packageName, version, metadata)
	if err != nil {
		if writeUpstreamHeld(w, err) {
			return
		}
		h.proxy.Logger.Warn("cooldown: could not read npm publish time",
			"package", packageName, "version", version, "error", err)
		JSONError(w, http.StatusBadGateway, "failed to check cooldown")
		return
	}
	if inCooldown {
		JSONError(w, http.StatusNotFound, ErrVersionNotFound.Error())
		return
	}

	packument, err := metadata()
	if err != nil {
		if errors.Is(err, ErrUpstreamNotFound) {
			JSONError(w, http.StatusNotFound, ErrPackageNotFound.Error())
			return
		}
		if writeUpstreamHeld(w, err) {
			return
		}
		h.proxy.Logger.Error("failed to fetch npm metadata", "error", err)
		JSONError(w, http.StatusBadGateway, "failed to fetch from upstream")
		return
	}
	listed, _, _ := jsonscan.LookupString(packument, "versions", version, "dist", "attestations", "url")
	if listed == "" {
		JSONError(w, http.StatusNotFound, errNoAttestations.Error())
		return
	}
	upstreamURL, err := validateRegistryURL("attestations", listed, registryURL)
	if err != nil {
		// Mirrors of npmjs can keep its URLs in packuments. The proxy only
		// ever asks the package's own registry.
		upstreamURL = registryURL + "/-/npm/v1/attestations/" + url.PathEscape(packageName) + "@" + url.PathEscape(version)
	}

	h.proxy.proxyImmutable(w, r, upstreamURL, "npm", cacheKey+"/-/attestations/"+version, contentTypeJSON)
}

// parseNPMAttestationsPath splits "{name}@{version}" as the attestations
// endpoint takes it, where name may be "@scope/name" with the slash encoded
// or not. It rejects anything that is not one package and one version.
func parseNPMAttestationsPath(spec string) (packageName, version string, ok bool) {
	at := strings.LastIndex(spec, "@")
	if at <= 0 {
		return "", "", false
	}
	packageName, version = spec[:at], spec[at+1:]
	if version == "" || strings.ContainsAny(version, "/\\") || containsPathTraversal(packageName) || containsPathTraversal(version) {
		return "", "", false
	}
	scope, name, scoped := strings.Cut(packageName, "/")
	switch {
	case scoped:
		ok = len(scope) > 1 && scope[0] == '@' && name != "" && !strings.ContainsAny(name, "/@")
	default:
		ok = !strings.ContainsAny(packageName, "@\\")
	}
	return packageName, version, ok
}
