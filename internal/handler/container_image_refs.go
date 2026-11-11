package handler

import (
	"encoding/json"
	"regexp"
	"slices"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
)

// maxImageManifestRefs is how many references of one manifest are recorded.
// A manifest with more is not recorded.
const maxImageManifestRefs = 4096

// imageSubjectTagPattern matches the tags that hold what refers to an image
// rather than an image of their own: the referrers tag schema, and the
// sha256-<hex>.sig, .att and .sbom tags of cosign.
var imageSubjectTagPattern = regexp.MustCompile(`^sha256-[a-f0-9]{64}(\.[a-z0-9]+)?$`)

// recordImageManifest notes which manifests and blobs a served manifest
// references, and for a tag that it pointed at the manifest, so retention
// can keep the blobs of the newest images of a repository. It does nothing
// unless storage.retention.keep_latest covers container images. The
// retention sweep drops the records of images that are no longer among the
// newest.
func (h *ContainerHandler) recordImageManifest(cacheName, reference string, manifest *cachedContainerManifest, stale bool) {
	if h.proxy.KeepLatestImages <= 0 || h.proxy.DB == nil {
		return
	}
	// A manifest asked for by digest is recorded under that digest, the one
	// the manifest referencing it names.
	digest, tagged := reference, time.Time{}
	if !manifestDigestReferencePattern.MatchString(reference) {
		digest = manifest.contentDigest
		if !imageSubjectTagPattern.MatchString(reference) {
			// The time upstream last confirmed the tag, which a stale copy
			// served during an outage does not move.
			if tagged = manifest.fetchedAt; tagged.IsZero() && !stale {
				tagged = time.Now()
			}
		}
	}
	if !mirrorDigestPattern.MatchString(digest) {
		return
	}

	packagePURL := canonicalPackagePURL("oci", cacheName)
	known, _, hasRefs, err := h.proxy.DB.ImageManifestState(packagePURL, digest)
	if err != nil {
		h.proxy.Logger.Warn("failed to read image manifest record", "package_purl", packagePURL, "digest", digest, "error", err)
		return
	}
	var refs []string
	if !hasRefs {
		// The answer to a HEAD for a manifest that is not cached has no
		// body to read references from. A tag seen that way is recorded
		// without them, and the pull of the manifest by digest that follows
		// fills them in.
		refs = imageManifestRefs(manifest.body)
	}
	// Tag times are stored in milliseconds.
	newerTag := tagged.UnixMilli() > known.UnixMilli()
	if !newerTag && len(refs) == 0 {
		return
	}
	record := database.ImageManifest{PackagePURL: packagePURL, Digest: digest, Refs: refs, TaggedAt: tagged}
	if err := h.proxy.DB.RecordImageManifest(record); err != nil {
		h.proxy.Logger.Warn("failed to record image manifest", "package_purl", packagePURL, "digest", digest, "error", err)
	}
}

// imageManifestRefs returns the digests of the manifests an index lists, or
// of the config and layers of an image manifest. It returns nil for a
// document that names none, or more than maxImageManifestRefs.
func imageManifestRefs(body []byte) []string {
	var manifest ociManifest
	if len(body) == 0 || json.Unmarshal(body, &manifest) != nil {
		return nil
	}
	descriptors := slices.Concat(manifest.Manifests, manifest.Layers)
	if manifest.Config != nil {
		descriptors = append(descriptors, *manifest.Config)
	}
	seen := make(map[string]struct{}, len(descriptors))
	var refs []string
	for _, descriptor := range descriptors {
		if _, dup := seen[descriptor.Digest]; dup || !mirrorDigestPattern.MatchString(descriptor.Digest) {
			continue
		}
		seen[descriptor.Digest] = struct{}{}
		refs = append(refs, descriptor.Digest)
	}
	if len(refs) > maxImageManifestRefs {
		return nil
	}
	return refs
}
