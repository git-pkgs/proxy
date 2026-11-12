package handler

import (
	"encoding/json"
	"regexp"
	"slices"
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

// recordImageManifestReferences notes the digests a served manifest
// references, which finds the tags whose image contains a blob. A digest
// fixes the content, so a stale copy is as good as a fresh one.
func (h *ContainerHandler) recordImageManifestReferences(cacheName string, manifest *cachedContainerManifest) {
	// The first record of a digest stands, so only a body that hashes to it
	// is recorded.
	if h.proxy.DB == nil || verifyDigest(manifest.contentDigest, manifest.body) != nil {
		return
	}
	references := imageManifestReferences(manifest.body)
	if len(references) == 0 {
		return
	}
	packagePURL := canonicalPackagePURL("oci", cacheName)
	if err := h.proxy.DB.RecordImageManifestReferences(packagePURL, manifest.contentDigest, references); err != nil {
		h.proxy.Logger.Warn("failed to record image manifest references", "package_purl", packagePURL, "digest", manifest.contentDigest, "error", err)
	}
}

// imageManifestReferences returns the digests an index or image manifest
// references.
func imageManifestReferences(body []byte) []string {
	var manifest ociManifest
	if json.Unmarshal(body, &manifest) != nil {
		return nil
	}
	var refs []string
	for _, descriptor := range slices.Concat(manifest.Manifests, manifest.Layers) {
		refs = append(refs, descriptor.Digest)
	}
	if manifest.Config != nil {
		refs = append(refs, manifest.Config.Digest)
	}
	return refs
}
