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

// A blob's page names the tags whose cached image contains it: the manifest
// the tag points at, the platform manifests it lists, and their configs and
// layers, as far as clients pulled them.
func TestImageTagsContaining(t *testing.T) {
	h, _, image := imageTagsHandler(t)
	// containerd: a GET of the tag, then the platform manifest by digest.
	requestTagManifest(t, h, http.MethodGet, "multi")
	requestTagManifest(t, h, http.MethodGet, image.arm64Digest)
	// Docker: a HEAD of the tag, then the manifest by digest.
	requestTagManifest(t, h, http.MethodHead, "single")
	requestTagManifest(t, h, http.MethodGet, image.singleDigest)
	tags, err := h.proxy.DB.ImageTags(canonicalPackagePURL("oci", imageTagsRepo), 100)
	if err != nil {
		t.Fatal(err)
	}

	db := h.proxy.DB
	if _, err := db.Exec("UPDATE metadata_cache SET last_accessed_at = NULL"); err != nil {
		t.Fatal(err)
	}
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
		got := h.ImageTagsContaining(context.Background(), imageTagsRepo, tt.digest, tags)
		if !slices.Equal(got, tt.want) {
			t.Errorf("%s: tags = %v, want %v", name, got, tt.want)
		}
	}

	// Looking does not count as a use of the cached manifests.
	var touched int
	if err := db.Get(&touched, "SELECT COUNT(*) FROM metadata_cache WHERE last_accessed_at IS NOT NULL"); err != nil {
		t.Fatal(err)
	}
	if touched != 0 {
		t.Errorf("%d cached manifests were marked as used", touched)
	}
}
