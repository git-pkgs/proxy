package handler

import (
	"regexp"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/metadata/oci"
)

// imageSubjectTagPattern matches the tags that hold what refers to an image
// rather than an image of their own: the referrers tag schema, and the
// sha256-<hex>.sig, .att and .sbom tags of cosign.
var imageSubjectTagPattern = regexp.MustCompile(`^sha256-[a-f0-9]{64}(\.[a-z0-9]+)?$`)

// recordImageManifest notes which manifests and blobs a served manifest
// references, and for a tag that it pointed at the manifest. Retention reads
// the records to keep the blobs of the newest images of a repository, and
// the web UI to name the tags whose image contains a blob. The retention
// sweep drops the records of images that are no longer among the newest and
// that no tag points at.
func (h *ContainerHandler) recordImageManifest(cacheName, reference string, manifest *cachedContainerManifest, stale bool) {
	if h.proxy.DB == nil {
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

// imageManifestRefs returns the digests a manifest references; see
// oci.ManifestRefs.
func imageManifestRefs(body []byte) []string {
	return oci.ManifestRefs(body)
}
