package handler

import (
	"time"
)

// recordImageTag notes which digest a tag of the repository pointed at when
// the upstream last confirmed it, for the web UI to list. A copy served stale
// during an outage confirms nothing.
func (h *ContainerHandler) recordImageTag(cacheName, reference string, manifest *cachedContainerManifest, stale bool) {
	if h.proxy.DB == nil || stale || manifestDigestReferencePattern.MatchString(reference) ||
		imageSubjectTagPattern.MatchString(reference) || !mirrorDigestPattern.MatchString(manifest.contentDigest) {
		return
	}
	seen := manifest.fetchedAt
	if seen.IsZero() {
		seen = time.Now()
	}
	packagePURL := canonicalPackagePURL("oci", cacheName)
	if err := h.proxy.DB.RecordImageTag(packagePURL, reference, manifest.contentDigest, seen); err != nil {
		h.proxy.Logger.Warn("failed to record image tag", "package_purl", packagePURL, "tag", reference, "error", err)
	}
}
