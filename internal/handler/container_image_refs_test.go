package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const imageRefsRepo = "team/app"

func imageRefsHandler(t *testing.T, keepLatest int) (*ContainerHandler, *fakeImageRegistry, fakeImage) {
	t.Helper()
	registry, image := newFakeImageRegistry(t)
	proxy := newMirrorTestProxy(t, registry)
	proxy.KeepLatestImages = keepLatest
	return NewContainerHandlerWithRegistry(proxy, "http://proxy.example", registry.server.URL), registry, image
}

func pullImageManifest(t *testing.T, h *ContainerHandler, reference string) {
	t.Helper()
	if response := pull(t, h, "/"+imageRefsRepo+"/manifests/"+reference, containerdManifestAccept); response.Code != http.StatusOK {
		t.Fatalf("manifest %s: status %d: %s", reference, response.Code, response.Body.String())
	}
}

// newestImageDigests returns what the newest image of the repository needs.
func newestImageDigests(t *testing.T, h *ContainerHandler) map[string]struct{} {
	t.Helper()
	digests, err := h.proxy.DB.NewestImageDigests(canonicalPackagePURL("oci", imageRefsRepo), 1)
	if err != nil {
		t.Fatalf("NewestImageDigests: %v", err)
	}
	return digests
}

func imageTaggedAt(t *testing.T, h *ContainerHandler, digest string) (time.Time, bool) {
	t.Helper()
	at, ok, err := h.proxy.DB.ImageManifestTaggedAt(canonicalPackagePURL("oci", imageRefsRepo), digest)
	if err != nil {
		t.Fatalf("ImageManifestTaggedAt: %v", err)
	}
	return at, ok
}

// moveTag points a tag of the fake registry at another manifest.
func (r *fakeImageRegistry) moveTag(tag, digest string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.manifests["/v2/"+imageRefsRepo+"/manifests/"+tag] = r.manifests["/v2/"+imageRefsRepo+"/manifests/"+digest]
}

// A pull by tag makes the manifest an image of the repository, and the
// manifests pulled by digest after it say which blobs it needs.
func TestContainerPullRecordsWhatAnImageReferences(t *testing.T) {
	h, _, image := imageRefsHandler(t, 1)

	pullImageManifest(t, h, "multi")
	if at, ok := imageTaggedAt(t, h, image.indexDigest); !ok || at.IsZero() {
		t.Fatalf("index after a pull by tag: recorded=%v tagged=%v, want both", ok, at)
	}
	pullImageManifest(t, h, image.arm64Digest)
	if at, ok := imageTaggedAt(t, h, image.arm64Digest); !ok || !at.IsZero() {
		t.Fatalf("platform manifest after a pull by digest: recorded=%v tagged=%v, want recorded and untagged", ok, at)
	}

	digests := newestImageDigests(t, h)
	for name, digest := range map[string]string{
		"index": image.indexDigest, "arm64 manifest": image.arm64Digest,
		"amd64 manifest": image.amd64Digest, "arm64 layer": image.arm64Layer,
	} {
		if _, ok := digests[digest]; !ok {
			t.Errorf("%s %s is not among the newest image's digests", name, digest)
		}
	}
	// The amd64 manifest was never pulled, so its blobs are unknown.
	if _, ok := digests[image.amd64Layer]; ok {
		t.Errorf("amd64 layer is among the digests though its manifest was not pulled")
	}
}

// Without keep_latest nothing is recorded.
func TestContainerPullRecordsNothingWithoutKeepLatest(t *testing.T) {
	h, _, image := imageRefsHandler(t, 0)
	pullImageManifest(t, h, "multi")
	pullImageManifest(t, h, image.arm64Digest)
	if _, ok := imageTaggedAt(t, h, image.indexDigest); ok {
		t.Errorf("index recorded though keep_latest is off")
	}
	if _, ok := imageTaggedAt(t, h, image.arm64Digest); ok {
		t.Errorf("platform manifest recorded though keep_latest is off")
	}
}

// Referrers and cosign tags hold what refers to an image. They are recorded
// like a pull by digest, so they don't count as images of their own.
func TestContainerPullOfReferrersTagIsNotAnImage(t *testing.T) {
	h, registry, image := imageRefsHandler(t, 1)
	subject := "sha256-" + image.indexDigest[len("sha256:"):]
	for _, tag := range []string{subject, subject + ".sig", subject + ".att"} {
		registry.moveTag(tag, image.singleDigest)
		pullImageManifest(t, h, tag)
	}
	if at, ok := imageTaggedAt(t, h, image.singleDigest); !ok || !at.IsZero() {
		t.Errorf("manifest behind referrers tags: recorded=%v tagged=%v, want recorded and untagged", ok, at)
	}
}

// A tag that is asked for again moves the image's tag time forward, also
// when the client only sends a HEAD, as containerd does for an image it has.
func TestContainerPullAgainMovesTagTimeForward(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			h, _, image := imageRefsHandler(t, 1)
			pullImageManifest(t, h, "single")
			db := h.proxy.DB
			if _, err := db.Exec(db.Rebind("UPDATE image_manifests SET tagged_at = ?"), 1000); err != nil {
				t.Fatal(err)
			}

			request := httptest.NewRequest(method, "/"+imageRefsRepo+"/manifests/single", nil)
			request.Header.Set("Accept", containerdManifestAccept)
			response := httptest.NewRecorder()
			h.Routes().ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("status %d", response.Code)
			}
			if at, _ := imageTaggedAt(t, h, image.singleDigest); time.Since(at) > time.Minute {
				t.Errorf("tag time after the second request = %v, want now", at)
			}
		})
	}
}

// When a tag moves, the image it points at now is the newest, and the one
// it pointed at before no longer is.
func TestContainerMovedTagMakesTheNewImageNewest(t *testing.T) {
	h, registry, image := imageRefsHandler(t, 1)
	registry.moveTag("stable", image.singleDigest)
	pullImageManifest(t, h, "stable")
	if _, ok := newestImageDigests(t, h)[image.singleLayer]; !ok {
		t.Fatalf("layer of the first image is not kept")
	}
	db := h.proxy.DB
	if _, err := db.Exec(db.Rebind("UPDATE image_manifests SET tagged_at = ?"), 1000); err != nil {
		t.Fatal(err)
	}

	registry.moveTag("stable", image.indexDigest)
	pullImageManifest(t, h, "stable")
	pullImageManifest(t, h, image.arm64Digest)

	digests := newestImageDigests(t, h)
	if _, ok := digests[image.arm64Layer]; !ok {
		t.Errorf("layer of the image the tag moved to is not kept")
	}
	if _, ok := digests[image.singleLayer]; ok {
		t.Errorf("layer of the image the tag moved away from is still kept")
	}
}

// A scheduled mirror run resolves tags itself, and records them like a pull.
func TestMirrorImageRecordsWhatAnImageReferences(t *testing.T) {
	h, _, image := imageRefsHandler(t, 1)
	platform, err := ParsePlatform("linux/arm64")
	if err != nil {
		t.Fatal(err)
	}
	ref := ImageRef{Name: imageRefsRepo, References: []string{"multi"}}
	if _, err := h.MirrorImage(context.Background(), ref, []Platform{platform}); err != nil {
		t.Fatalf("MirrorImage: %v", err)
	}
	if at, ok := imageTaggedAt(t, h, image.indexDigest); !ok || at.IsZero() {
		t.Fatalf("index after a mirror run: recorded=%v tagged=%v, want both", ok, at)
	}
	if _, ok := newestImageDigests(t, h)[image.arm64Layer]; !ok {
		t.Errorf("arm64 layer is not among the mirrored image's digests")
	}

	// A later run that finds the tag unchanged still moves its time forward.
	db := h.proxy.DB
	if _, err := db.Exec(db.Rebind("UPDATE image_manifests SET tagged_at = ? WHERE tagged_at > 0"), 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := h.MirrorImage(context.Background(), ref, []Platform{platform}); err != nil {
		t.Fatalf("second MirrorImage: %v", err)
	}
	if at, _ := imageTaggedAt(t, h, image.indexDigest); time.Since(at) > time.Minute {
		t.Errorf("tag time after the second run = %v, want now", at)
	}
}
