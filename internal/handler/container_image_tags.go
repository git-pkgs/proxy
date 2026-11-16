package handler

import (
	"slices"
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
	stored, known, err := h.proxy.DB.ImageTag(packagePURL, reference)
	if err != nil {
		h.proxy.Logger.Warn("failed to read image tag", "package_purl", packagePURL, "tag", reference, "error", err)
		return
	}
	// Times are stored in milliseconds. A fresh cache hit repeats what is
	// stored, and the one writer SQLite has should not hold the pull up.
	if known && seen.UnixMilli() <= stored.SeenAt.UnixMilli() {
		return
	}
	if known && stored.Digest != manifest.contentDigest && h.referencedBy(packagePURL, stored.Digest, manifest.contentDigest) {
		// One platform's manifest of the index the tag points at, served to
		// a client that accepts no index: the same image seen another way.
		err = h.proxy.DB.TouchImageTag(packagePURL, reference, seen)
	} else {
		err = h.proxy.DB.RecordImageTag(packagePURL, reference, manifest.contentDigest, seen)
	}
	if err != nil {
		h.proxy.Logger.Warn("failed to record image tag", "package_purl", packagePURL, "tag", reference, "error", err)
	}
}

// referencedBy reports whether the recorded manifest parent references digest.
func (h *ContainerHandler) referencedBy(packagePURL, parent, digest string) bool {
	refs, err := h.proxy.DB.ImageManifestRefs(packagePURL, parent)
	return err == nil && slices.Contains(refs, digest)
}
