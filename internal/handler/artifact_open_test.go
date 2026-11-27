package handler

import (
	"context"
	"database/sql"
	"io"
	"testing"

	"github.com/git-pkgs/proxy/internal/storage"
	"github.com/git-pkgs/purl"
)

const openTestContent = "open test artifact"

// openTestArtifact seeds npm demo 1.0.0 and returns what findStored makes
// of it.
func openTestArtifact(t *testing.T) (*Proxy, *mockStorage, *ApprovedArtifact) {
	t.Helper()
	proxy, db, store, _ := setupTestProxy(t)
	seedPackage(t, db, store, "npm", "demo", "1.0.0", "demo-1.0.0.tgz", openTestContent)
	a, err := proxy.findStored(purl.MakePURLString("npm", "demo", ""), purl.MakePURLString("npm", "demo", "1.0.0"), "demo-1.0.0.tgz")
	if err != nil || a == nil {
		t.Fatalf("findStored = %v, %v; want the seeded artifact", a, err)
	}
	return proxy, store, a
}

func readAll(t *testing.T, r io.ReadCloser) (string, error) {
	t.Helper()
	defer func() { _ = r.Close() }()
	b, err := io.ReadAll(r)
	return string(b), err
}

func TestFindStored(t *testing.T) {
	proxy, _, a := openTestArtifact(t)
	if !a.cached || a.StoragePath != "npm/demo/1.0.0/demo-1.0.0.tgz" || a.Artifact.Filename != "demo-1.0.0.tgz" || a.ecosystem != "npm" {
		t.Errorf("findStored = %+v", a)
	}
	if got, err := proxy.findStored("pkg:npm/missing", "pkg:npm/missing@1.0.0", "missing-1.0.0.tgz"); err != nil || got != nil {
		t.Errorf("missing artifact = %v, %v; want nil", got, err)
	}
}

func TestFindStoredDiscardsAnUnusableRecord(t *testing.T) {
	proxy, db, store, _ := setupTestProxy(t)
	seedPackage(t, db, store, "npm", "demo", "1.0.0", "demo-1.0.0.tgz", openTestContent)
	versionPURL := purl.MakePURLString("npm", "demo", "1.0.0")
	art, err := db.GetArtifact(versionPURL, "demo-1.0.0.tgz")
	if err != nil || art == nil {
		t.Fatal(err)
	}
	art.ContentHash = sql.NullString{String: "not hex", Valid: true}
	if err := db.UpsertArtifact(art); err != nil {
		t.Fatal(err)
	}

	if got, err := proxy.findStored(purl.MakePURLString("npm", "demo", ""), versionPURL, "demo-1.0.0.tgz"); err != nil || got != nil {
		t.Errorf("findStored = %v, %v; want nil", got, err)
	}
	if cached, _ := db.GetCachedArtifact(purl.MakePURLString("npm", "demo", ""), versionPURL, "demo-1.0.0.tgz"); cached != nil {
		t.Error("unusable record still cached")
	}
}

func TestOpenCachedArtifact(t *testing.T) {
	proxy, _, a := openTestArtifact(t)
	res, err := proxy.Open(context.Background(), a)
	if err != nil || res == nil || res.Reader == nil || !res.Cached {
		t.Fatalf("Open = %+v, %v", res, err)
	}
	if got, err := readAll(t, res.Reader); err != nil || got != openTestContent {
		t.Errorf("read %q, %v", got, err)
	}
}

func TestOpenForHeadOpensNothing(t *testing.T) {
	for _, seekable := range []bool{false, true} {
		proxy, store, a := openTestArtifact(t)
		store.seekable = seekable
		res, err := proxy.Open(ServingHead(context.Background()), a)
		if err != nil || res == nil || res.Reader != nil || res.rangeable != seekable {
			t.Errorf("seekable %v: Open = %+v, %v", seekable, res, err)
		}
		if store.opens != 0 {
			t.Errorf("seekable %v: %d opens for a HEAD", seekable, store.opens)
		}
	}
}

func TestOpenDirectServe(t *testing.T) {
	proxy, store, a := openTestArtifact(t)
	proxy.DirectServe = true
	store.signedURL = "https://bucket.example/signed"
	res, err := proxy.Open(context.Background(), a)
	if err != nil || res == nil || res.RedirectURL != store.signedURL || res.Reader != nil || store.opens != 0 {
		t.Errorf("Open = %+v, %v, %d opens; want a redirect", res, err, store.opens)
	}

	store.signErr = storage.ErrSignedURLUnsupported
	res, err = proxy.Open(context.Background(), a)
	if err != nil || res == nil || res.Reader == nil || res.RedirectURL != "" {
		t.Fatalf("unsupported signing: Open = %+v, %v; want a reader", res, err)
	}
	_ = res.Reader.Close()
}

func TestOpenMissingObject(t *testing.T) {
	proxy, store, a := openTestArtifact(t)
	delete(store.files, a.StoragePath)
	if res, err := proxy.Open(context.Background(), a); err != nil || res != nil {
		t.Errorf("cached: Open = %+v, %v; want nil so the caller fetches again", res, err)
	}
	fresh := *a
	fresh.cached = false
	if res, err := proxy.Open(context.Background(), &fresh); err == nil || res != nil {
		t.Errorf("fresh: Open = %+v, %v; want an error", res, err)
	}
}

func TestOpenCorruptObjectIsDiscarded(t *testing.T) {
	proxy, store, a := openTestArtifact(t)
	store.files[a.StoragePath] = []byte("tampered bytes")
	res, err := proxy.Open(context.Background(), a)
	if err != nil || res == nil {
		t.Fatalf("Open = %+v, %v", res, err)
	}
	if _, err := readAll(t, res.Reader); err == nil {
		t.Error("tampered bytes read without an error")
	}
	if cached, _ := proxy.DB.GetCachedArtifact(purl.MakePURLString("npm", "demo", ""), a.Artifact.PURL, a.Artifact.Filename); cached != nil {
		t.Error("corrupt artifact still cached")
	}
}

func TestOpenFetchedArtifactSkipsCacheDelivery(t *testing.T) {
	proxy, store, a := openTestArtifact(t)
	proxy.DirectServe = true
	store.signedURL = "https://bucket.example/signed"
	fresh := *a
	fresh.cached = false
	res, err := proxy.Open(ServingHead(context.Background()), &fresh)
	if err != nil || res == nil || res.Reader == nil || res.RedirectURL != "" || res.Cached {
		t.Fatalf("Open = %+v, %v; want a reader over the fetched bytes", res, err)
	}
	if got, err := readAll(t, res.Reader); err != nil || got != openTestContent {
		t.Errorf("read %q, %v", got, err)
	}
}
