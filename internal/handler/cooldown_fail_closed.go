package handler

import "net/http"

// Metadata for a package that cooldown or the denylist applies to is never
// served unfiltered. When the publish times can't be read, or the document
// can't be rewritten, the request fails instead, so a rate-limited or
// failing upstream can't let withheld versions through.

// cooldownApplies reports whether cooldown withholds new versions of the
// package, taking package overrides and exemptions into account.
func (p *Proxy) cooldownApplies(ecosystem, name string) bool {
	return p.cooldownEnabled() && p.Cooldown.For(ecosystem, canonicalPackagePURL(ecosystem, name)) > 0
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
