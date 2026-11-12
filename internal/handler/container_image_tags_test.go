package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
)

const imageTagsRepo = "team/app"

func imageTagsHandler(t *testing.T) (*ContainerHandler, *fakeImageRegistry, fakeImage) {
	t.Helper()
	registry, image := newFakeImageRegistry(t)
	proxy := newMirrorTestProxy(t, registry)
	return NewContainerHandlerWithRegistry(proxy, "http://proxy.example", registry.server.URL), registry, image
}

func requestTagManifest(t *testing.T, h *ContainerHandler, method, reference string) {
	t.Helper()
	request := httptest.NewRequest(method, "/"+imageTagsRepo+"/manifests/"+reference, nil)
	request.Header.Set("Accept", containerdManifestAccept)
	response := httptest.NewRecorder()
	h.Routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("%s %s: status %d: %s", method, reference, response.Code, response.Body.String())
	}
}

func imageTags(t *testing.T, h *ContainerHandler) map[string]database.ImageTag {
	t.Helper()
	tags, err := h.proxy.DB.ImageTags(canonicalPackagePURL("oci", imageTagsRepo), 100)
	if err != nil {
		t.Fatalf("ImageTags: %v", err)
	}
	byTag := make(map[string]database.ImageTag, len(tags))
	for _, tag := range tags {
		byTag[tag.Tag] = tag
	}
	return byTag
}

// A pull by tag, with a GET or only a HEAD as Docker and containerd send,
// records the digest the tag points at. Pulls by digest name no tag.
func TestContainerPullRecordsImageTags(t *testing.T) {
	h, _, image := imageTagsHandler(t)
	requestTagManifest(t, h, http.MethodGet, "multi")
	requestTagManifest(t, h, http.MethodHead, "single")
	requestTagManifest(t, h, http.MethodGet, image.arm64Digest)

	tags := imageTags(t, h)
	if len(tags) != 2 {
		t.Fatalf("tags = %+v, want multi and single", tags)
	}
	for tag, digest := range map[string]string{"multi": image.indexDigest, "single": image.singleDigest} {
		got := tags[tag]
		if got.Digest != digest || time.Since(got.SeenAt) > time.Minute {
			t.Errorf("tag %s = %+v, want %s seen just now", tag, got, digest)
		}
	}
}

// Referrers and cosign tags hold what refers to an image, not an image, so
// they are not listed.
func TestContainerPullOfReferrersTagRecordsNoTag(t *testing.T) {
	h, registry, image := imageTagsHandler(t)
	subject := "sha256-" + image.indexDigest[len("sha256:"):]
	for _, tag := range []string{subject, subject + ".sig", subject + ".att"} {
		registry.mu.Lock()
		registry.manifests["/v2/"+imageTagsRepo+"/manifests/"+tag] = registry.manifests["/v2/"+imageTagsRepo+"/manifests/"+image.singleDigest]
		registry.mu.Unlock()
		requestTagManifest(t, h, http.MethodGet, tag)
	}
	if tags := imageTags(t, h); len(tags) != 0 {
		t.Errorf("tags = %+v, want none", tags)
	}
}

// A copy served stale during an outage says nothing new about the tag.
func TestContainerStaleManifestKeepsTagTime(t *testing.T) {
	h, registry, _ := imageTagsHandler(t)
	requestTagManifest(t, h, http.MethodGet, "multi")
	db := h.proxy.DB
	if _, err := db.Exec(db.Rebind("UPDATE image_tags SET seen_at = ?"), 1000); err != nil {
		t.Fatal(err)
	}
	registry.offline.Store(true)
	requestTagManifest(t, h, http.MethodGet, "multi")
	if got := imageTags(t, h)["multi"].SeenAt; !got.Equal(time.UnixMilli(1000)) {
		t.Errorf("seen at = %v after a stale serve, want unchanged", got)
	}
}

// A scheduled mirror run resolves tags itself and records them like a pull.
func TestMirrorImageRecordsImageTags(t *testing.T) {
	h, _, image := imageTagsHandler(t)
	ref := ImageRef{Name: imageTagsRepo, References: []string{"multi"}}
	if _, err := h.MirrorImage(context.Background(), ref, nil); err != nil {
		t.Fatalf("MirrorImage: %v", err)
	}
	if got := imageTags(t, h)["multi"]; got.Digest != image.indexDigest {
		t.Errorf("tag multi after a mirror run = %+v, want %s", got, image.indexDigest)
	}
}

// Pulled manifests record what they reference, so a blob's page finds the
// tags whose image contains it: the manifest the tag points at, the platform
// manifests it lists, and their configs and layers, as far as clients pulled
// them.
func TestContainerPullRecordsWhatImagesContain(t *testing.T) {
	h, _, image := imageTagsHandler(t)
	// containerd: a GET of the tag, then the platform manifest by digest.
	requestTagManifest(t, h, http.MethodGet, "multi")
	requestTagManifest(t, h, http.MethodGet, image.arm64Digest)
	// Docker: a HEAD of the tag, then the manifest by digest.
	requestTagManifest(t, h, http.MethodHead, "single")
	requestTagManifest(t, h, http.MethodGet, image.singleDigest)

	for name, tt := range map[string]struct {
		digest string
		want   []string
	}{
		"index":          {image.indexDigest, []string{"multi"}},
		"platform":       {image.arm64Digest, []string{"multi"}},
		"layer":          {image.arm64Layer, []string{"multi"}},
		"single layer":   {image.singleLayer, []string{"single"}},
		"unpulled layer": {image.amd64Layer, nil},
		"unknown":        {"sha256:" + strings.Repeat("0", 64), nil},
	} {
		got, err := h.proxy.DB.ImageTagsContaining(canonicalPackagePURL("oci", imageTagsRepo), tt.digest, 10)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("%s: tags = %v, want %v", name, got, tt.want)
		}
	}
}

// A mirror run reads the manifests through the handler, so it records what
// they reference like a pull.
func TestMirrorImageRecordsWhatImagesContain(t *testing.T) {
	h, _, image := imageTagsHandler(t)
	ref := ImageRef{Name: imageTagsRepo, References: []string{"multi"}}
	if _, err := h.MirrorImage(context.Background(), ref, nil); err != nil {
		t.Fatalf("MirrorImage: %v", err)
	}
	got, err := h.proxy.DB.ImageTagsContaining(canonicalPackagePURL("oci", imageTagsRepo), image.amd64Layer, 10)
	if err != nil || !slices.Equal(got, []string{"multi"}) {
		t.Errorf("tags containing the amd64 layer after a mirror run = %v, %v; want multi", got, err)
	}
}

// The first record of a manifest stands, so a body that is not the one its
// digest names is not recorded under that digest.
func TestImageManifestReferencesNeedTheBodyTheDigestNames(t *testing.T) {
	h, _, image := imageTagsHandler(t)
	body := []byte(`{"schemaVersion":2,"layers":[{"digest":"sha256:` + strings.Repeat("1", 64) + `"}]}`)
	h.recordImageManifestReferences(imageTagsRepo, &cachedContainerManifest{body: body, contentDigest: image.singleDigest})
	requestTagManifest(t, h, http.MethodGet, "single")
	got, err := h.proxy.DB.ImageTagsContaining(canonicalPackagePURL("oci", imageTagsRepo), image.singleLayer, 10)
	if err != nil || !slices.Equal(got, []string{"single"}) {
		t.Errorf("tags containing the single layer = %v, %v; want single", got, err)
	}
}
