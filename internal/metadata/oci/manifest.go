package oci

import (
	"encoding/json"
	"regexp"
	"slices"
)

// MaxManifestRefs is how many references ManifestRefs returns at most. A
// manifest with more yields none.
const MaxManifestRefs = 4096

// DigestPattern matches the sha256 and sha512 digests the proxy accepts.
var DigestPattern = regexp.MustCompile(`^(sha256:[a-f0-9]{64}|sha512:[a-f0-9]{128})$`)

// Descriptor is an OCI content descriptor.
type Descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	URLs        []string          `json:"urls"`
	Platform    *Platform         `json:"platform"`
	Annotations map[string]string `json:"annotations"`
}

// Platform is the platform an index entry is for.
type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant"`
}

// Manifest is the part of an image manifest or index the proxy reads: an
// index's manifests, or an image's config and layers.
type Manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Manifests     []Descriptor `json:"manifests"`
	Config        *Descriptor  `json:"config"`
	Layers        []Descriptor `json:"layers"`
}

// ManifestRefs returns the digests of the manifests an index lists, or of
// the config and layers of an image manifest, each once. It returns nil for
// a document that doesn't parse, names none, or names more than
// MaxManifestRefs.
func ManifestRefs(body []byte) []string {
	var manifest Manifest
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
		if _, dup := seen[descriptor.Digest]; dup || !DigestPattern.MatchString(descriptor.Digest) {
			continue
		}
		seen[descriptor.Digest] = struct{}{}
		refs = append(refs, descriptor.Digest)
	}
	if len(refs) > MaxManifestRefs {
		return nil
	}
	return refs
}
