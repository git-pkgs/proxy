package handler

import (
	"time"
)

// ReleaseAllowed reports whether clients would be served a version published
// at publishedAt: the denylist doesn't name it, and cooldown, where the
// ecosystem applies it, doesn't withhold it. Without a publish time a version
// counts as withheld wherever cooldown applies.
func (p *Proxy) ReleaseAllowed(ecosystem, name, version string, publishedAt time.Time) bool {
	if p.versionDenied(ecosystem, name, version) {
		return false
	}
	if !CooldownSupported(ecosystem) || !p.cooldownApplies(ecosystem, name) {
		return true
	}
	return !publishedAt.IsZero() && p.cooldown().IsAllowed(ecosystem, canonicalPackagePURL(ecosystem, name), publishedAt)
}
