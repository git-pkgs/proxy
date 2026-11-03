package handler

import "net/http"

// cooldownApplies reports whether cooldown withholds new versions of the
// package, taking package overrides and exemptions into account.
func (p *Proxy) cooldownApplies(ecosystem, name string) bool {
	return p.cooldownEnabled() && p.cooldown().For(ecosystem, canonicalPackagePURL(ecosystem, name)) > 0
}

// mustFilterMetadata reports whether the package's metadata may have
// versions to withhold, by cooldown or by the denylist, and so must not be
// served as upstream sent it.
func (p *Proxy) mustFilterMetadata(ecosystem, name string) bool {
	return p.cooldownApplies(ecosystem, name) ||
		len(p.Denylist.Versions(canonicalPackagePURL(ecosystem, name))) != 0
}

// writeCooldownLookupError answers a metadata request whose publish times
// couldn't be read: 503 with Retry-After when the rate limit gate held the
// request, 502 otherwise.
func (p *Proxy) writeCooldownLookupError(w http.ResponseWriter, ecosystem, name string, err error) {
	if writeUpstreamHeld(w, err) {
		return
	}
	p.Logger.Warn("cooldown: could not read publish times",
		"ecosystem", ecosystem, "package", name, "error", err)
	http.Error(w, "failed to check cooldown", http.StatusBadGateway)
}

// serveCachedDespiteCooldownError serves a download from the cache when its
// publish time couldn't be read, and reports whether it did. A cached file was
// already let through once, so the proxy keeps serving it while the upstream
// is unreachable; only a cache miss fails.
func (p *Proxy) serveCachedDespiteCooldownError(w http.ResponseWriter, r *http.Request, ecosystem, name, version, filename string, lookupErr error) bool {
	if p.versionDenied(ecosystem, name, version) {
		return false
	}
	cached, err := p.GetCachedArtifact(r.Context(), ecosystem, name, version, filename)
	if err != nil || cached == nil {
		return false
	}
	p.Logger.Warn("cooldown: could not read publish times, serving cached file",
		"ecosystem", ecosystem, "package", name, "version", version, "error", lookupErr)
	ServeArtifactRequest(w, r, cached)
	return true
}
