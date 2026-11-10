package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/retention"
	"github.com/git-pkgs/purl"
	"github.com/git-pkgs/registries/fetch"
)

const (
	keepImagesRepo      = "team/app"
	keepImagesIndexType = "application/vnd.oci.image.index.v1+json"
	keepImagesImageType = "application/vnd.oci.image.manifest.v1+json"
)

// keepImagesRegistry is a registry with one repository whose tags can be
// moved between images.
type keepImagesRegistry struct {
	srv *httptest.Server
	mu  sync.Mutex
	// manifests holds the manifest documents by digest, tags the digest
	// each tag points at.
	manifests map[string]string
	types     map[string]string
	tags      map[string]string
	blobs     map[string]string
}

// keepImage is one image of the repository: an index with one platform
// manifest, which has a config and a layer.
type keepImage struct {
	index, manifest string
	config, layer   ociBlob
}

func newKeepImagesRegistry(t *testing.T) *keepImagesRegistry {
	t.Helper()
	r := &keepImagesRegistry{manifests: map[string]string{}, types: map[string]string{}, tags: map[string]string{}, blobs: map[string]string{}}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		const manifestsPrefix, blobsPrefix = "/v2/" + keepImagesRepo + "/manifests/", "/v2/" + keepImagesRepo + "/blobs/"
		if len(req.URL.Path) > len(blobsPrefix) && req.URL.Path[:len(blobsPrefix)] == blobsPrefix {
			if content, ok := r.blobs[req.URL.Path[len(blobsPrefix):]]; ok {
				_, _ = io.WriteString(w, content)
				return
			}
		}
		if len(req.URL.Path) > len(manifestsPrefix) && req.URL.Path[:len(manifestsPrefix)] == manifestsPrefix {
			digest := req.URL.Path[len(manifestsPrefix):]
			if tagged, ok := r.tags[digest]; ok {
				digest = tagged
			}
			if body, ok := r.manifests[digest]; ok {
				w.Header().Set("Content-Type", r.types[digest])
				w.Header().Set("Docker-Content-Digest", digest)
				if req.Method != http.MethodHead {
					_, _ = io.WriteString(w, body)
				}
				return
			}
		}
		http.NotFound(w, req)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *keepImagesRegistry) addManifest(t *testing.T, mediaType string, doc map[string]any) string {
	t.Helper()
	doc["schemaVersion"], doc["mediaType"] = 2, mediaType
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	digest := ociBlob{content: string(body)}.digest()
	r.manifests[digest], r.types[digest] = string(body), mediaType
	return digest
}

// addImage adds an image whose blobs are named after it.
func (r *keepImagesRegistry) addImage(t *testing.T, name string) keepImage {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	image := keepImage{
		config: ociBlob{keepImagesRepo, name + " config"},
		layer:  ociBlob{keepImagesRepo, name + " layer"},
	}
	for _, b := range []ociBlob{image.config, image.layer} {
		r.blobs[b.digest()] = b.content
	}
	image.manifest = r.addManifest(t, keepImagesImageType, map[string]any{
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": image.config.digest()},
		"layers": []map[string]any{{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": image.layer.digest()}},
	})
	image.index = r.addManifest(t, keepImagesIndexType, map[string]any{
		"manifests": []map[string]any{{
			"mediaType": keepImagesImageType, "digest": image.manifest,
			"platform": map[string]string{"os": "linux", "architecture": "arm64"},
		}},
	})
	return image
}

func (r *keepImagesRegistry) tag(tag string, image keepImage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tags[tag] = image.index
}

// newKeepImagesProxy serves the registry through a proxy that keeps the
// blobs of keepLatest images per repository.
func newKeepImagesProxy(t *testing.T, registry *keepImagesRegistry, keepLatest int) (*httptest.Server, *database.DB) {
	t.Helper()
	db, store := setupEvictionTest(t)
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(registry.srv.Client()), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), discardLogger())
	proxy.HTTPClient = registry.srv.Client()
	proxy.KeepLatestImages = keepLatest
	srv := httptest.NewServer(handler.NewContainerHandlerWithRegistry(proxy, "http://proxy.test", registry.srv.URL).Routes())
	t.Cleanup(srv.Close)
	return srv, db
}

// pullImage pulls a tag the way a client does: the tag, the platform
// manifest by digest, then its blobs.
func pullImage(t *testing.T, proxyURL, tag string, image keepImage) {
	t.Helper()
	base := proxyURL + "/" + keepImagesRepo
	ociGet(t, base+"/manifests/"+tag)
	ociGet(t, base+"/manifests/"+image.manifest)
	ociGet(t, base+"/blobs/"+image.config.digest())
	ociGet(t, base+"/blobs/"+image.layer.digest())
}

// ageImages makes every cached blob, every record and every tag sighting
// look old, with the images given first seen longest ago.
func ageImages(t *testing.T, db *database.DB, now time.Time, images ...keepImage) {
	t.Helper()
	recorded := now.Add(-30 * retentionDay).UnixMilli()
	if _, err := db.Exec(db.Rebind(`UPDATE image_manifests SET recorded_at = ?`), recorded); err != nil {
		t.Fatalf("backdating records: %v", err)
	}
	for i, image := range images {
		old := now.Add(-time.Duration(30-i) * retentionDay)
		ociBackdate(t, db, image.config, old, old)
		ociBackdate(t, db, image.layer, old, old)
		query := db.Rebind(`UPDATE image_manifests SET tagged_at = ? WHERE digest = ? AND tagged_at > 0`)
		if _, err := db.Exec(query, old.UnixMilli(), image.index); err != nil {
			t.Fatalf("backdating image %d: %v", i, err)
		}
	}
}

func imageCached(t *testing.T, db *database.DB, image keepImage) bool {
	t.Helper()
	config := ociArtifact(t, db, keepImagesRepo, image.config).StoragePath.Valid
	layer := ociArtifact(t, db, keepImagesRepo, image.layer).StoragePath.Valid
	if config != layer {
		t.Errorf("config cached = %v, layer cached = %v, want the same", config, layer)
	}
	return layer
}

// With keep_latest, the blobs of the images a tag most recently pointed at
// outlive their rule. Blobs of images the tag left longer ago are evicted.
func TestSweepRetentionKeepsBlobsOfNewestImages(t *testing.T) {
	for _, tt := range []struct {
		keepLatest int
		wantCached [3]bool
	}{
		{keepLatest: 0, wantCached: [3]bool{false, false, false}},
		{keepLatest: 1, wantCached: [3]bool{false, false, true}},
		{keepLatest: 2, wantCached: [3]bool{false, true, true}},
	} {
		registry := newKeepImagesRegistry(t)
		srv, db := newKeepImagesProxy(t, registry, tt.keepLatest)
		images := []keepImage{registry.addImage(t, "first"), registry.addImage(t, "second"), registry.addImage(t, "third")}
		for _, image := range images {
			registry.tag("beta", image)
			pullImage(t, srv.URL, "beta", image)
		}
		now := time.Now()
		ageImages(t, db, now, images...)

		rules := retention.Rules{Ecosystems: map[string]time.Duration{"oci": retentionDay}, KeepLatest: tt.keepLatest}
		sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)

		for i, image := range images {
			if got := imageCached(t, db, image); got != tt.wantCached[i] {
				t.Errorf("keep_latest %d: image %d cached = %v, want %v", tt.keepLatest, i+1, got, tt.wantCached[i])
			}
		}
	}
}

// A sweep drops the records of images that are no longer among the newest,
// so the table holds a few images per repository, and all records once
// keep_latest is turned off.
func TestSweepRetentionPrunesImageRecords(t *testing.T) {
	registry := newKeepImagesRegistry(t)
	srv, db := newKeepImagesProxy(t, registry, 1)
	images := []keepImage{registry.addImage(t, "first"), registry.addImage(t, "second")}
	for _, image := range images {
		registry.tag("beta", image)
		pullImage(t, srv.URL, "beta", image)
	}
	now := time.Now()
	ageImages(t, db, now, images...)
	recorded := func(digest string) bool {
		t.Helper()
		_, ok, err := db.ImageManifestTaggedAt(purl.MakePURLString("oci", keepImagesRepo, ""), digest)
		if err != nil {
			t.Fatalf("ImageManifestTaggedAt: %v", err)
		}
		return ok
	}

	rules := retention.Rules{Ecosystems: map[string]time.Duration{"oci": retentionDay}, KeepLatest: 1}
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	for _, digest := range []string{images[0].index, images[0].manifest} {
		if recorded(digest) {
			t.Errorf("manifest %s of the older image is still recorded", digest)
		}
	}
	for _, digest := range []string{images[1].index, images[1].manifest} {
		if !recorded(digest) {
			t.Errorf("manifest %s of the newest image is no longer recorded", digest)
		}
	}

	rules.KeepLatest = 0
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if recorded(images[1].index) {
		t.Error("records are left after keep_latest was turned off")
	}
}

// Manifests are recorded only when a retention rule can evict container
// blobs, since nothing reads the records otherwise.
func TestKeepLatestImagesNeedsAnOCIRule(t *testing.T) {
	for _, tt := range []struct {
		name       string
		def        string
		ecosystems map[string]string
		packages   map[string]string
		keepLatest int
		want       int
	}{
		{name: "no keep_latest", ecosystems: map[string]string{"oci": "1d"}, want: 0},
		{name: "no rule", keepLatest: 2, want: 0},
		{name: "rule for another ecosystem", ecosystems: map[string]string{"npm": "7d"}, keepLatest: 2, want: 0},
		{name: "oci rule", ecosystems: map[string]string{"oci": "1d"}, keepLatest: 2, want: 2},
		{name: "default", def: "30d", keepLatest: 2, want: 2},
		{name: "default with oci exempt", def: "30d", ecosystems: map[string]string{"oci": "0"}, keepLatest: 2, want: 0},
		{name: "rule for one repository", packages: map[string]string{"pkg:oci/library/nginx": "3d"}, keepLatest: 2, want: 2},
		{name: "rule for another ecosystem's package", packages: map[string]string{"pkg:npm/lodash": "3d"}, keepLatest: 2, want: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Storage.Retention.Default = tt.def
			cfg.Storage.Retention.Ecosystems = tt.ecosystems
			cfg.Storage.Retention.Packages = tt.packages
			cfg.Storage.Retention.KeepLatest = tt.keepLatest
			if got := keepLatestImages(cfg); got != tt.want {
				t.Errorf("keepLatestImages() = %d, want %d", got, tt.want)
			}
		})
	}
}

// proxy mirror runs in a process of its own and has to record the images it
// mirrors as serve does, or the sweep would not know their blobs.
func TestMirrorProxyRecordsImagesForKeepLatest(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Retention.Ecosystems = map[string]string{"oci": "1d"}
	cfg.Storage.Retention.KeepLatest = 2
	proxy, _ := NewMirrorProxy(cfg, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if proxy.KeepLatestImages != 2 {
		t.Errorf("KeepLatestImages = %d, want 2", proxy.KeepLatestImages)
	}
}

// An image pulled only by digest is not one of the newest images, so its
// blobs are evicted by age as without keep_latest.
func TestSweepRetentionKeepLatestIgnoresImagesPulledByDigest(t *testing.T) {
	registry := newKeepImagesRegistry(t)
	srv, db := newKeepImagesProxy(t, registry, 1)
	image := registry.addImage(t, "pinned")
	pullImage(t, srv.URL, image.index, image)
	now := time.Now()
	ageImages(t, db, now, image)

	rules := retention.Rules{Ecosystems: map[string]time.Duration{"oci": retentionDay}, KeepLatest: 1}
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)

	if imageCached(t, db, image) {
		t.Error("blobs of an image pulled only by digest were kept")
	}
}
