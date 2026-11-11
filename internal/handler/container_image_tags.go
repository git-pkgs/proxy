package handler

import (
	"regexp"
	"time"
)

// imageSubjectTagsPattern matches the tags that hold what refers to an
// image rather than an image: the referrers tag schema, and the
// sha256-<hex>.sig, .att and .sbom tags of cosign.
var imageSubjectTagsPattern = regexp.MustCompile(`^sha256-[a-f0-9]{64}(\.[a-z0-9]+)?$`)

// recordImageTag notes which digest a tag of the repository pointed at when
// the upstream last confirmed it, for the web UI to list. A copy served stale
// during an outage confirms nothing.
func (h *ContainerHandler) recordImageTag(cacheName, reference string, manifest *cachedContainerManifest, stale bool) {
	if h.proxy.DB == nil || stale || manifestDigestReferencePattern.MatchString(reference) ||
		imageSubjectTagsPattern.MatchString(reference) || !mirrorDigestPattern.MatchString(manifest.contentDigest) {
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
