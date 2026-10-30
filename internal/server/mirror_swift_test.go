package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/mirror"
	"github.com/git-pkgs/proxy/internal/storage"
)

// swiftUpstream serves the Swift registry API for acme.widget with releases
// 1.0.0, on the first page of the release list, and 1.1.0, on the second.
// Each release has a Package.swift that links a manifest for Swift 5.8.
type swiftUpstream struct {
	*httptest.Server
	// checksum overrides the archive checksum the release metadata lists.
	checksum    string
	hits        atomic.Int32
	archiveHits atomic.Int32
}

func swiftArchive(version string) string { return "zip of acme.widget " + version }

func newSwiftUpstream(t *testing.T) *swiftUpstream {
	t.Helper()
	u := &swiftUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		w.Header().Set("Content-Version", "1")
		rest, ok := strings.CutPrefix(r.URL.Path, "/acme/widget")
		switch {
		case r.URL.Path == "/identifiers":
			if r.URL.Query().Get("url") != "https://github.com/acme/widget" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"identifiers":["acme.widget"]}`)
		case !ok:
			http.NotFound(w, r)
		case rest == "":
			page := map[string]string{"": "1.0.0", "page=2": "1.1.0"}[r.URL.RawQuery]
			if page == "" {
				http.NotFound(w, r)
				return
			}
			if r.URL.RawQuery == "" {
				w.Header().Set("Link", fmt.Sprintf(`<%s/acme/widget?page=2>; rel="next"`, u.URL))
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"releases": map[string]any{
				page: map[string]string{"url": u.URL + "/acme/widget/" + page},
			}})
		case strings.HasSuffix(rest, ".zip"):
			u.archiveHits.Add(1)
			_, _ = io.WriteString(w, swiftArchive(strings.TrimSuffix(rest[1:], ".zip")))
		case strings.HasSuffix(rest, "/Package.swift"):
			version := strings.TrimSuffix(rest[1:], "/Package.swift")
			w.Header().Set("Content-Type", "text/x-swift")
			if r.URL.Query().Get("swift-version") == "5.8" {
				_, _ = io.WriteString(w, "// swift-tools-version:5.8\n// "+version)
				return
			}
			w.Header().Set("Link", fmt.Sprintf(`<%s/acme/widget/%s/Package.swift?swift-version=5.8>; rel="alternate"; filename="Package@swift-5.8.swift"; swift-tools-version="5.8"`, u.URL, version))
			_, _ = io.WriteString(w, "// swift-tools-version:5.9\n// "+version)
		default:
			version := rest[1:]
			sum := sha256.Sum256([]byte(swiftArchive(version)))
			checksum := hex.EncodeToString(sum[:])
			if u.checksum != "" {
				checksum = u.checksum
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "acme.widget", "version": version,
				"resources": []map[string]string{{"name": "source-archive", "type": "application/zip", "checksum": checksum}},
			})
		}
	}))
	t.Cleanup(u.Close)
	return u
}

// mirrorSwift mirrors purls from upstream the way proxy mirror does, with
// metadata caching on.
func mirrorSwift(t *testing.T, upstream *swiftUpstream, purls ...string) (*mirror.Progress, *handler.Proxy) {
	t.Helper()
	cfg := config.Default()
	cfg.Upstream.Swift = upstream.URL
	cfg.Upstream.AllowLoopback = true
	db, err := database.Create(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MigrateSchema(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := storage.OpenBucket(context.Background(), "file://"+t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy, _ := NewMirrorProxy(cfg, db, store, logger)
	proxy.CacheMetadata = true
	m := mirror.New(proxy, db, store, logger, 1)
	m.Versions = NewMirrorVersions(cfg.Upstream, proxy.HTTPClient)
	progress, err := m.Run(context.Background(), &mirror.PURLSource{PURLs: purls, Versions: m.Versions})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	return progress, proxy
}

func swiftRegistryGet(t *testing.T, routes http.Handler, path, accept string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Accept", accept)
	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, req)
	return rec
}

func TestMirrorSwiftReleaseIsServedFromCache(t *testing.T) {
	upstream := newSwiftUpstream(t)

	progress, proxy := mirrorSwift(t, upstream, "pkg:swift/Acme/Widget@1.0.0")

	if progress.Completed != 1 || upstream.archiveHits.Load() != 1 {
		t.Fatalf("completed = %d, archive hits = %d, errors = %v", progress.Completed, upstream.archiveHits.Load(), progress.Errors)
	}

	// SwiftPM resolves and downloads the release with the upstream down:
	// the release list and its pages, the release metadata, both manifests
	// and the archive come from the cache.
	upstream.Close()
	routes := handler.NewSwiftHandler(proxy, "http://proxy.test", upstream.URL).Routes()
	const jsonType = "application/vnd.swift.registry.v1+json"
	const swiftType = "application/vnd.swift.registry.v1+swift"
	checks := []struct {
		path, accept, body, link string
	}{
		{"/acme/widget", jsonType, "http://proxy.test/swift/acme/widget/1.0.0", "<http://proxy.test/swift/acme/widget?page=2>"},
		{"/acme/widget?page=2", jsonType, "http://proxy.test/swift/acme/widget/1.1.0", ""},
		{"/acme/widget/1.0.0", jsonType, `"source-archive"`, ""},
		{"/acme/widget/1.0.0/Package.swift", swiftType, "swift-tools-version:5.9", "<http://proxy.test/swift/acme/widget/1.0.0/Package.swift?swift-version=5.8>"},
		{"/acme/widget/1.0.0/Package.swift?swift-version=5.8.0", swiftType, "swift-tools-version:5.8", ""},
		{"/acme/widget/1.0.0.zip", "application/vnd.swift.registry.v1+zip", swiftArchive("1.0.0"), ""},
	}
	for _, check := range checks {
		rec := swiftRegistryGet(t, routes, check.path, check.accept)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), check.body) {
			t.Errorf("%s: status = %d, body %q; want 200 with %q", check.path, rec.Code, strings.TrimSpace(rec.Body.String()), check.body)
		}
		if link := rec.Header().Get("Link"); !strings.Contains(link, check.link) {
			t.Errorf("%s: Link = %q, want %q", check.path, link, check.link)
		}
	}
}

func TestMirrorSwiftRefusesChecksumMismatch(t *testing.T) {
	upstream := newSwiftUpstream(t)
	upstream.checksum = strings.Repeat("0", 64)

	progress, proxy := mirrorSwift(t, upstream, "pkg:swift/acme/widget@1.0.0")

	if progress.Failed != 1 || progress.Completed != 0 {
		t.Fatalf("completed = %d, failed = %d, errors = %v; want the archive refused", progress.Completed, progress.Failed, progress.Errors)
	}
	upstream.Close()
	routes := handler.NewSwiftHandler(proxy, "http://proxy.test", upstream.URL).Routes()
	if rec := swiftRegistryGet(t, routes, "/acme/widget/1.0.0.zip", "application/vnd.swift.registry.v1+zip"); rec.Code == http.StatusOK {
		t.Error("the refused archive was cached")
	}
}

func TestMirrorSwiftListsReleases(t *testing.T) {
	upstream := newSwiftUpstream(t)

	progress, _ := mirrorSwift(t, upstream, "pkg:swift/acme/widget")

	if progress.Total != 2 || progress.Completed != 2 || upstream.archiveHits.Load() != 2 {
		t.Errorf("total = %d, completed = %d, archive hits = %d, errors = %v; want both releases mirrored",
			progress.Total, progress.Completed, upstream.archiveHits.Load(), progress.Errors)
	}
}

func TestMirrorSwiftResolvesRepositoryPURLs(t *testing.T) {
	upstream := newSwiftUpstream(t)

	progress, proxy := mirrorSwift(t, upstream, "pkg:swift/github.com/acme/widget@1.0.0", "pkg:swift/github.com/acme/widget", "pkg:swift/github.com/acme/other@1.0.0")

	// The listed PURL's 1.0.0 is the versioned one, so it's mirrored once.
	if progress.Completed != 2 || progress.Failed != 1 || upstream.archiveHits.Load() != 2 {
		t.Fatalf("completed = %d, failed = %d, archive hits = %d, errors = %v; want both acme.widget releases mirrored and the unknown repository refused",
			progress.Completed, progress.Failed, upstream.archiveHits.Load(), progress.Errors)
	}
	if len(progress.Errors) != 1 || !strings.Contains(progress.Errors[0].Error, "no package for https://github.com/acme/other") {
		t.Errorf("errors = %v; want the unknown repository named", progress.Errors)
	}

	// The lookup is cached too, for clients that resolve a repository URL
	// through the registry.
	upstream.Close()
	routes := handler.NewSwiftHandler(proxy, "http://proxy.test", upstream.URL).Routes()
	rec := swiftRegistryGet(t, routes, "/identifiers?url=https%3A%2F%2Fgithub.com%2Facme%2Fwidget", "application/vnd.swift.registry.v1+json")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "acme.widget") {
		t.Errorf("identifiers: status = %d, body %q; want the cached lookup", rec.Code, rec.Body.String())
	}
}
