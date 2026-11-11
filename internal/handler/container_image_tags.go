package handler

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
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

// imageTagWalkDepth is how many levels below a tag's manifest
// ImageTagsContaining looks: an index, its platform manifests, and their
// configs and layers.
const imageTagWalkDepth = 2

// ImageTagsContaining returns those of tags whose image, as cached under
// cacheName, contains digest: the manifest the tag points at, a platform
// manifest it lists, or a config or layer of one. Only manifests clients or
// mirror runs pulled are known. Reading them does not count as a use of the
// cache.
func (h *ContainerHandler) ImageTagsContaining(ctx context.Context, cacheName, digest string, tags []database.ImageTag) []string {
	registryURL, upstreamName, _, ok := h.registryForName(cacheName)
	if !ok {
		return nil
	}
	// The manifests a walk read, by digest, with the digests they reference.
	refs := map[string][]string{}
	references := func(d string) []string {
		if known, done := refs[d]; done {
			return known
		}
		// A manifest fetched by tag is cached under its digest too.
		manifest, _, err := h.readContainerManifest(ctx, h.containerManifestSharedCacheKey(registryURL, upstreamName, d, ""))
		if err == nil && manifest != nil && manifest.contentDigest == d {
			refs[d] = imageManifestReferences(manifest.body)
		} else {
			refs[d] = nil
		}
		return refs[d]
	}

	var found []string
	for _, tag := range tags {
		level := []string{tag.Digest}
		for depth := 0; len(level) > 0 && !slices.Contains(level, digest); depth++ {
			if depth == imageTagWalkDepth {
				level = nil
				break
			}
			var next []string
			for _, d := range level {
				next = append(next, references(d)...)
			}
			level = next
		}
		if slices.Contains(level, digest) {
			found = append(found, tag.Tag)
		}
	}
	return found
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
