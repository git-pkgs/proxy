package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	// maxMirrorIndexDepth bounds how deep nested indexes are followed.
	maxMirrorIndexDepth = 2
	// mirrorErrorBodyLimit bounds how much of an error response is quoted.
	mirrorErrorBodyLimit = 200

	dockerReferenceTypeAnnotation   = "vnd.docker.reference.type"
	dockerReferenceDigestAnnotation = "vnd.docker.reference.digest"
	attestationManifestReference    = "attestation-manifest"
)

var (
	// Grammar from the OCI distribution spec.
	mirrorNamePattern   = regexp.MustCompile(`^[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*$`)
	mirrorTagPattern    = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)
	mirrorDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// dockerHubMirrorAliases are registry hosts that name Docker Hub in image
// references. hub.docker.com is the default registry of the docker PURL type.
var dockerHubMirrorAliases = []string{"docker.io", "index.docker.io", "registry-1.docker.io", "hub.docker.com"} //nolint:gochecknoglobals // fixed alias list

// ImageRef names an image to mirror.
type ImageRef struct {
	// Registry is the registry host the image reference names, as containerd
	// sends it in the ns query parameter. Empty routes Name like a pull
	// without ns.
	Registry string
	// Name is the repository name.
	Name string
	// References are the tags and digests to mirror.
	References []string
}

// Platform selects the entries of an image index to mirror. Empty fields
// match any value.
type Platform struct {
	OS           string
	Architecture string
	Variant      string
}

// ParsePlatform parses os/architecture[/variant].
func ParsePlatform(value string) (Platform, error) {
	os, rest, _ := strings.Cut(value, "/")
	architecture, variant, _ := strings.Cut(rest, "/")
	if os == "" || architecture == "" || strings.Contains(variant, "/") {
		return Platform{}, fmt.Errorf("invalid platform %q, want os/architecture[/variant]", value)
	}
	return Platform{OS: os, Architecture: architecture, Variant: variant}, nil
}

func (p Platform) matches(entry *ociPlatform) bool {
	if entry == nil {
		return false
	}
	if p.OS != "" && p.OS != entry.OS {
		return false
	}
	if p.Architecture != "" && p.Architecture != entry.Architecture {
		return false
	}
	return p.Variant == "" || p.Variant == platformVariant(entry)
}

// platformVariant fills in the variant arm64 images usually leave out.
func platformVariant(entry *ociPlatform) string {
	if entry.Variant == "" && entry.Architecture == "arm64" {
		return "v8"
	}
	return entry.Variant
}

// ImageResult reports what mirroring one image did.
type ImageResult struct {
	// Fetched counts manifests and blobs that were not cached before.
	Fetched int
	// Bytes is the size of the blobs fetched for this image.
	Bytes    int64
	Warnings []string
}

type ociDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	URLs        []string          `json:"urls"`
	Platform    *ociPlatform      `json:"platform"`
	Annotations map[string]string `json:"annotations"`
}

type ociPlatform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant"`
}

type ociManifest struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Manifests     []ociDescriptor `json:"manifests"`
	Config        *ociDescriptor  `json:"config"`
	Layers        []ociDescriptor `json:"layers"`
}

type imageMirror struct {
	h            *ContainerHandler
	ref          ImageRef
	registryURL  string
	upstreamName string
	cacheName    string
	platforms    []Platform
	seen         map[string]bool
	result       ImageResult
	// platformMatches counts index entries that matched the platform filter
	// during the current walk.
	platformMatches int
}

// MirrorImage fetches an image the way a client pull does: the manifest or
// index for each reference, the platform manifests, then config and layer
// blobs. Everything is stored under the keys a later pull through this handler
// reads, so the pull is served from cache. Platforms limit which index entries
// are followed; without them every platform is mirrored.
func (h *ContainerHandler) MirrorImage(ctx context.Context, ref ImageRef, platforms []Platform) (ImageResult, error) {
	ref, err := normalizeImageRef(ref, h.registryURL)
	if err != nil {
		return ImageResult{}, err
	}
	route := &http.Request{URL: &url.URL{RawQuery: url.Values{namespaceQueryParam: {ref.Registry}}.Encode()}}
	if ref.Registry == "" {
		route.URL.RawQuery = ""
	}
	registryURL, upstreamName, cacheName, ok := h.registryForRequest(route, ref.Name)
	if !ok {
		if ref.Registry != "" {
			return ImageResult{}, fmt.Errorf("registry %q is not configured as an OCI upstream", ref.Registry)
		}
		return ImageResult{}, fmt.Errorf("no OCI upstream for %q", ref.Name)
	}
	m := &imageMirror{
		h:            h,
		ref:          ref,
		registryURL:  registryURL,
		upstreamName: upstreamName,
		cacheName:    cacheName,
		platforms:    platforms,
		seen:         make(map[string]bool),
	}
	var digests []string
	for _, reference := range ref.References {
		mirror := m.mirrorTag
		if mirrorDigestPattern.MatchString(reference) {
			mirror = func(ctx context.Context, digest string) (string, error) { return m.mirrorManifest(ctx, digest, 0) }
		}
		digest, err := mirror(ctx, reference)
		if err != nil {
			return m.result, fmt.Errorf("%s:%s: %w", ref.Name, reference, err)
		}
		digests = append(digests, digest)
	}
	if len(digests) == 2 && digests[0] != digests[1] && mirrorDigestPattern.MatchString(ref.References[0]) {
		m.result.Warnings = append(m.result.Warnings, fmt.Sprintf("tag %s now points to %s, not %s; both were mirrored",
			ref.References[1], digests[1], ref.References[0]))
	}
	return m.result, nil
}

// normalizeImageRef validates the reference before any network access and
// adds the library/ prefix Docker Hub clients use for official images.
func normalizeImageRef(ref ImageRef, defaultRegistryURL string) (ImageRef, error) {
	if slices.Contains(dockerHubMirrorAliases, strings.ToLower(ref.Registry)) {
		ref.Registry = "docker.io"
	}
	dockerHub := ref.Registry == "docker.io" || (ref.Registry == "" && defaultRegistryURL == dockerHubRegistry)
	if dockerHub && !strings.Contains(ref.Name, "/") {
		ref.Name = "library/" + ref.Name
	}
	if !mirrorNamePattern.MatchString(ref.Name) {
		return ref, fmt.Errorf("invalid repository name %q", ref.Name)
	}
	if len(ref.References) == 0 {
		return ref, errors.New("a tag or digest is required; mirroring all tags is not supported")
	}
	for _, reference := range ref.References {
		if !mirrorTagPattern.MatchString(reference) && !mirrorDigestPattern.MatchString(reference) {
			return ref, fmt.Errorf("invalid tag or digest %q", reference)
		}
	}
	return ref, nil
}

// errTagUpstreamUnavailable marks tag lookups that failed because the
// upstream could not be reached or answered with 429 or 5xx.
var errTagUpstreamUnavailable = errors.New("upstream unavailable")

// mirrorTag resolves a tag upstream without caching it, mirrors the manifest
// it names by digest, and stores the tag only after that manifest and
// everything it references are cached. A failed mirror leaves the previously
// cached tag as it was. If the upstream is unreachable, the cached tag is
// mirrored as it is.
func (m *imageMirror) mirrorTag(ctx context.Context, tag string) (string, error) {
	accept := containerManifestAccept(&http.Request{Header: http.Header{}})
	cacheAccept := normalizeContainerManifestAccept(accept)
	before, _ := m.h.loadContainerManifestForAccept(ctx, m.registryURL, m.upstreamName, tag, accept, cacheAccept)

	manifest, err := m.resolveTag(ctx, tag, accept, before)
	if err != nil {
		if before == nil || !errors.Is(err, errTagUpstreamUnavailable) {
			return "", fmt.Errorf("manifest %s: %w", tag, err)
		}
		m.result.Warnings = append(m.result.Warnings, fmt.Sprintf("manifest %s served from stale cache, upstream unreachable", tag))
		_, err := m.mirrorManifest(ctx, before.contentDigest, 0)
		return before.contentDigest, err
	}

	// The digest entry is immutable, so it can be stored right away; the
	// walk below then reads it from cache instead of fetching it again.
	digest := manifest.contentDigest
	if before == nil || before.contentDigest != digest {
		m.result.Fetched++
		m.h.storeContainerManifestForAccept(ctx, m.registryURL, m.upstreamName, digest, accept, cacheAccept, manifest)
	}
	if _, err := m.mirrorManifest(ctx, digest, 0); err != nil {
		return "", err
	}

	// A client pull may have moved the tag while the walk ran. Its entry is
	// newer than what this mirror resolved, so it is kept.
	current, _ := m.h.loadContainerManifestForAccept(ctx, m.registryURL, m.upstreamName, tag, accept, cacheAccept)
	if current != nil && current.contentDigest != digest && (before == nil || current.contentDigest != before.contentDigest) {
		m.result.Warnings = append(m.result.Warnings, fmt.Sprintf("tag %s moved to %s while it was mirrored; kept that entry", tag, current.contentDigest))
		return digest, nil
	}
	m.h.storeContainerManifestForAccept(ctx, m.registryURL, m.upstreamName, tag, accept, cacheAccept, manifest)
	return digest, nil
}

// resolveTag returns the manifest a tag points to upstream without caching
// it. A HEAD request comes first: if the tag still points to the cached
// digest, the cached manifest is reused with a new fetch time, which spares a
// GET (registries such as Docker Hub do not count HEAD requests against pull
// limits).
func (m *imageMirror) resolveTag(ctx context.Context, tag, accept string, before *cachedContainerManifest) (*cachedContainerManifest, error) {
	if before != nil {
		response, err := m.requestTag(ctx, http.MethodHead, tag, accept)
		if err != nil {
			return nil, err
		}
		_ = response.Body.Close()
		if response.Header.Get("Docker-Content-Digest") == before.contentDigest {
			unchanged := *before
			unchanged.fetchedAt = time.Now()
			return &unchanged, nil
		}
	}

	response, err := m.requestTag(ctx, http.MethodGet, tag, accept)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := m.h.proxy.ReadMetadata(response.Body)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: reading manifest: %w", errTagUpstreamUnavailable, err)
	}
	digest := sha256Digest(body)
	if contentDigest := response.Header.Get("Docker-Content-Digest"); strings.HasPrefix(contentDigest, "sha256:") && contentDigest != digest {
		return nil, fmt.Errorf("digest verification failed: upstream says %s, body is %s", contentDigest, digest)
	}
	return &cachedContainerManifest{
		body:          body,
		contentType:   response.Header.Get(headerContentType),
		contentDigest: digest,
		etag:          response.Header.Get(headerETag),
		size:          int64(len(body)),
		lastModified:  parseHTTPTime(response.Header.Get(headerLastModified)),
		fetchedAt:     time.Now(),
	}, nil
}

// requestTag sends a manifest request for a tag upstream. Only 200 responses
// are returned; network errors, 429 and 5xx wrap errTagUpstreamUnavailable.
func (m *imageMirror) requestTag(ctx context.Context, method, tag, accept string) (*http.Response, error) {
	upstreamURL := fmt.Sprintf("%s/v2/%s/manifests/%s", m.registryURL, m.upstreamName, tag)
	request, err := http.NewRequestWithContext(ctx, method, upstreamURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", accept)
	response, err := m.h.proxy.HTTPClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %w", errTagUpstreamUnavailable, err)
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		if shouldServeStaleManifest(response.StatusCode) {
			return nil, fmt.Errorf("%w: status %d", errTagUpstreamUnavailable, response.StatusCode)
		}
		return nil, fmt.Errorf("upstream status %d", response.StatusCode)
	}
	return response, nil
}

// mirrorManifest stores the manifest for reference through the handler's own
// manifest route, then follows what it references. It returns the manifest
// digest.
func (m *imageMirror) mirrorManifest(ctx context.Context, reference string, depth int) (string, error) {
	cacheAccept := normalizeContainerManifestAccept(containerManifestAccept(&http.Request{Header: http.Header{}}))
	before, _ := m.h.loadContainerManifestForAccept(ctx, m.registryURL, m.upstreamName, reference, cacheAccept, cacheAccept)

	target := "/" + m.ref.Name + "/manifests/" + reference
	if m.ref.Registry != "" {
		target += "?" + url.Values{namespaceQueryParam: {m.ref.Registry}}.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	response := newMirrorResponse()
	m.h.Routes().ServeHTTP(response, request)
	if response.status != http.StatusOK {
		return "", fmt.Errorf("manifest %s: status %d: %s", reference, response.status, response.errorText())
	}
	if response.header.Get("Warning") != "" {
		m.result.Warnings = append(m.result.Warnings, fmt.Sprintf("manifest %s served from stale cache, upstream unreachable", reference))
	}
	digest := response.header.Get("Docker-Content-Digest")
	if !mirrorDigestPattern.MatchString(digest) {
		digest = sha256Digest(response.body.Bytes())
	}
	if before == nil || before.contentDigest != digest {
		m.result.Fetched++
	}
	if m.seen[digest] {
		return digest, nil
	}
	m.seen[digest] = true

	var manifest ociManifest
	if err := json.Unmarshal(response.body.Bytes(), &manifest); err != nil {
		return "", fmt.Errorf("manifest %s: %w", reference, err)
	}
	if manifest.SchemaVersion == 1 {
		return "", fmt.Errorf("manifest %s: schema 1 manifests are not supported", reference)
	}
	if manifest.Manifests == nil {
		return digest, m.mirrorBlobs(ctx, &manifest)
	}
	if depth == 0 {
		m.platformMatches = 0
	}
	if err := m.mirrorIndex(ctx, reference, &manifest, depth); err != nil {
		return "", err
	}
	// A nested index may lack the wanted platforms as long as the image as a
	// whole has them.
	if depth == 0 && len(m.platforms) > 0 && m.platformMatches == 0 {
		return "", fmt.Errorf("index %s: no manifest for the requested platforms", reference)
	}
	return digest, nil
}

// mirrorIndex follows the index entries for the wanted platforms. Docker
// fetches the attestation manifests of an index on every pull, so those of the
// selected platforms are mirrored too.
func (m *imageMirror) mirrorIndex(ctx context.Context, reference string, index *ociManifest, depth int) error {
	if depth >= maxMirrorIndexDepth {
		return fmt.Errorf("index %s: nested deeper than %d levels", reference, maxMirrorIndexDepth)
	}
	var selected, attestations []ociDescriptor
	selectedDigests := make(map[string]bool)
	for _, entry := range index.Manifests {
		if !mirrorDigestPattern.MatchString(entry.Digest) {
			return fmt.Errorf("index %s: invalid digest %q", reference, entry.Digest)
		}
		if entry.Annotations[dockerReferenceTypeAnnotation] == attestationManifestReference {
			attestations = append(attestations, entry)
			continue
		}
		if m.wantsPlatform(entry.Platform) {
			selected = append(selected, entry)
			selectedDigests[entry.Digest] = true
			if entry.Platform != nil {
				m.platformMatches++
			}
		}
	}
	for _, entry := range attestations {
		if len(m.platforms) == 0 || selectedDigests[entry.Annotations[dockerReferenceDigestAnnotation]] {
			selected = append(selected, entry)
		}
	}
	for _, entry := range selected {
		if _, err := m.mirrorManifest(ctx, entry.Digest, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func (m *imageMirror) wantsPlatform(platform *ociPlatform) bool {
	if len(m.platforms) == 0 || platform == nil {
		// Entries without a platform are nested indexes or artifacts; a
		// filter cannot judge them, so they are followed.
		return true
	}
	for _, want := range m.platforms {
		if want.matches(platform) {
			return true
		}
	}
	return false
}

func (m *imageMirror) mirrorBlobs(ctx context.Context, manifest *ociManifest) error {
	blobs := manifest.Layers
	if manifest.Config != nil {
		blobs = append([]ociDescriptor{*manifest.Config}, blobs...)
	}
	for _, blob := range blobs {
		if len(blob.URLs) > 0 || strings.Contains(blob.MediaType, "nondistributable") || strings.Contains(blob.MediaType, "foreign") {
			m.result.Warnings = append(m.result.Warnings, fmt.Sprintf("skipped non-distributable layer %s", blob.Digest))
			continue
		}
		if !mirrorDigestPattern.MatchString(blob.Digest) {
			return fmt.Errorf("invalid blob digest %q", blob.Digest)
		}
		if m.seen[blob.Digest] {
			continue
		}
		m.seen[blob.Digest] = true
		if err := m.mirrorBlob(ctx, blob.Digest); err != nil {
			return err
		}
	}
	return nil
}

// mirrorBlob stores a blob under the same identity handleBlobDownload uses.
func (m *imageMirror) mirrorBlob(ctx context.Context, digest string) error {
	proxy := m.h.proxy
	cached, err := proxy.HasCachedArtifact(ctx, "oci", m.cacheName, digest, digest)
	if err != nil {
		return fmt.Errorf("blob %s: %w", digest, err)
	}
	if cached {
		return nil
	}
	result, err := proxy.GetOrFetchArtifactFromURLWithDigest(ctx, "oci", m.cacheName, digest, digest,
		fmt.Sprintf("%s/v2/%s/blobs/%s", m.registryURL, m.upstreamName, digest), digest)
	if err != nil {
		return fmt.Errorf("blob %s: %w", digest, err)
	}
	if result.Reader != nil {
		_ = result.Reader.Close()
	}
	m.result.Fetched++
	m.result.Bytes += result.Artifact.Size
	return nil
}

// mirrorResponse collects the handler's manifest response in memory.
type mirrorResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newMirrorResponse() *mirrorResponse {
	return &mirrorResponse{header: make(http.Header), status: http.StatusOK}
}

func (r *mirrorResponse) Header() http.Header { return r.header }

func (r *mirrorResponse) WriteHeader(status int) { r.status = status }

func (r *mirrorResponse) Write(p []byte) (int, error) { return r.body.Write(p) }

func (r *mirrorResponse) errorText() string {
	text := strings.TrimSpace(r.body.String())
	if len(text) > mirrorErrorBodyLimit {
		text = text[:mirrorErrorBodyLimit] + "..."
	}
	return text
}
