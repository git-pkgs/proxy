package handler

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

// Versions every download cooldown fixture lists: one published before the
// window, one inside it, and one without a publish time.
const (
	cooldownOldVersion    = "1.0.0"
	cooldownRecentVersion = "2.0.0"
	cooldownUntimed       = "3.0.0"
)

// downloadCooldownCase drives one ecosystem's artifact endpoint. The package
// is always named demo, so the policy key is pkg:{ecosystem}/demo.
type downloadCooldownCase struct {
	ecosystem string
	// storedName is the name the artifact is cached under; empty when the
	// ecosystem keeps no publish time in the versions table.
	storedName string
	// artifactVersion maps a release to the version the artifact is cached
	// under, and filename and path to its file and download path.
	artifactVersion func(version string) string
	filename        func(version string) string
	path            func(version string) string
	// metadata serves the upstream document holding the publish times; a
	// zero time is listed without one.
	metadata func(w http.ResponseWriter, r *http.Request, times map[string]time.Time)
	handler  func(proxy *Proxy, upstreamURL string) http.Handler
}

func sameVersion(version string) string { return version }

func chartContent(version string) string { return "chart " + version }

func downloadCooldownCases() []downloadCooldownCase {
	return []downloadCooldownCase{
		{
			ecosystem:       "cargo",
			storedName:      "demo",
			artifactVersion: sameVersion,
			filename:        func(v string) string { return "demo-" + v + ".crate" },
			path:            func(v string) string { return "/crates/demo/" + v + "/download" },
			metadata:        cargoCooldownMetadata,
			handler:         func(p *Proxy, u string) http.Handler { return NewCargoHandler(p, "http://proxy", u, u).Routes() },
		},
		{
			ecosystem:  "gem",
			storedName: "demo",
			// Platform gems carry the platform in the filename and the
			// cached version, but not in the versions API number.
			artifactVersion: func(v string) string { return v + "-x86_64-linux" },
			filename:        func(v string) string { return "demo-" + v + "-x86_64-linux.gem" },
			path:            func(v string) string { return "/gems/demo-" + v + "-x86_64-linux.gem" },
			metadata:        gemCooldownMetadata,
			handler:         func(p *Proxy, u string) http.Handler { return NewGemHandlerWithUpstream(p, "http://proxy", u).Routes() },
		},
		{
			ecosystem:       "hex",
			storedName:      "demo",
			artifactVersion: sameVersion,
			filename:        func(v string) string { return "demo-" + v + ".tar" },
			path:            func(v string) string { return "/tarballs/demo-" + v + ".tar" },
			metadata:        hexCooldownMetadata,
			handler: func(p *Proxy, u string) http.Handler {
				return NewHexHandlerWithUpstreams(p, "http://proxy", u, u).Routes()
			},
		},
		{
			ecosystem:       "pub",
			storedName:      "demo",
			artifactVersion: sameVersion,
			filename:        func(v string) string { return "demo-" + v + ".tar.gz" },
			path:            func(v string) string { return "/packages/demo/versions/" + v + ".tar.gz" },
			metadata:        pubCooldownMetadata,
			handler:         func(p *Proxy, u string) http.Handler { return NewPubHandlerWithUpstream(p, "http://proxy", u).Routes() },
		},
		{
			// Conda caches under {channel}/{name}, but the policy is keyed by
			// the name alone, as the repodata filter keys it.
			ecosystem:       "conda",
			storedName:      "conda-forge/demo",
			artifactVersion: sameVersion,
			filename:        func(v string) string { return "noarch/demo-" + v + "-py_0.conda" },
			path:            func(v string) string { return "/conda-forge/noarch/demo-" + v + "-py_0.conda" },
			metadata:        condaCooldownMetadata,
			handler: func(p *Proxy, u string) http.Handler {
				return NewCondaHandlerWithUpstream(p, "http://proxy", u).Routes()
			},
		},
		{
			// Helm caches charts under the repository and digest, so the
			// chart name and time come from the index on every check.
			ecosystem:       "helm",
			artifactVersion: func(v string) string { return sha256Hex(chartContent(v)) },
			filename:        func(v string) string { return "demo-" + v + ".tgz" },
			path:            func(v string) string { return "/stable/charts/" + sha256Hex(chartContent(v)) + "/demo-" + v + ".tgz" },
			metadata:        helmCooldownMetadata,
			handler: func(p *Proxy, u string) http.Handler {
				return NewHelmHandler(p, "http://proxy", map[string]string{"stable": u + "/charts"}).Routes()
			},
		},
	}
}

func cargoCooldownMetadata(w http.ResponseWriter, r *http.Request, times map[string]time.Time) {
	if r.URL.Path != "/de/mo/demo" {
		http.NotFound(w, r)
		return
	}
	for version, published := range times {
		line := map[string]string{"name": "demo", "vers": version}
		if !published.IsZero() {
			line["pubtime"] = published.Format(time.RFC3339)
		}
		encoded, _ := json.Marshal(line)
		_, _ = fmt.Fprintf(w, "%s\n", encoded)
	}
}

func gemCooldownMetadata(w http.ResponseWriter, r *http.Request, times map[string]time.Time) {
	if r.URL.Path != "/api/v1/versions/demo.json" {
		http.NotFound(w, r)
		return
	}
	var versions []gemVersion
	for version, published := range times {
		v := gemVersion{Number: version, Platform: "x86_64-linux"}
		if !published.IsZero() {
			v.CreatedAt = published.Format(time.RFC3339)
		}
		versions = append(versions, v)
	}
	_ = json.NewEncoder(w).Encode(versions)
}

func hexCooldownMetadata(w http.ResponseWriter, r *http.Request, times map[string]time.Time) {
	if r.URL.Path != "/api/packages/demo" {
		http.NotFound(w, r)
		return
	}
	var pkg hexPackageAPI
	for version, published := range times {
		release := hexRelease{Version: version}
		if !published.IsZero() {
			release.InsertedAt = published.Format(time.RFC3339Nano)
		}
		pkg.Releases = append(pkg.Releases, release)
	}
	_ = json.NewEncoder(w).Encode(pkg)
}

func pubCooldownMetadata(w http.ResponseWriter, r *http.Request, times map[string]time.Time) {
	if r.URL.Path != "/api/packages/demo" {
		http.NotFound(w, r)
		return
	}
	var versions []map[string]string
	for version, published := range times {
		v := map[string]string{"version": version}
		if !published.IsZero() {
			v["published"] = published.Format(time.RFC3339)
		}
		versions = append(versions, v)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"name": "demo", "versions": versions})
}

func condaCooldownMetadata(w http.ResponseWriter, r *http.Request, times map[string]time.Time) {
	if r.URL.Path != "/conda-forge/noarch/repodata.json" {
		http.NotFound(w, r)
		return
	}
	packages := map[string]map[string]any{}
	for version, published := range times {
		entry := map[string]any{"name": "demo", "version": version}
		if !published.IsZero() {
			entry["timestamp"] = published.UnixMilli()
		}
		packages["demo-"+version+"-py_0.conda"] = entry
	}
	// The download check asks for gzip, as proxyCached does.
	w.Header().Set(headerContentEncoding, "gzip")
	gz := gzip.NewWriter(w)
	_ = json.NewEncoder(gz).Encode(map[string]any{"packages.conda": packages})
	_ = gz.Close()
}

func helmCooldownMetadata(w http.ResponseWriter, r *http.Request, times map[string]time.Time) {
	if r.URL.Path != "/charts/index.yaml" {
		http.NotFound(w, r)
		return
	}
	_, _ = fmt.Fprintln(w, "apiVersion: v1\nentries:\n  demo:")
	for version, published := range times {
		_, _ = fmt.Fprintf(w, "    - digest: %s\n      urls: [demo-%s.tgz]\n      version: %s\n",
			sha256Hex(chartContent(version)), version, version)
		if !published.IsZero() {
			_, _ = fmt.Fprintf(w, "      created: %s\n", published.Format(time.RFC3339))
		}
	}
}

// seedCachedArtifact caches the release's artifact the way an earlier,
// cooldown-free download would have.
func (c downloadCooldownCase) seedCachedArtifact(t *testing.T, proxy *Proxy, version string) {
	t.Helper()
	db, store := proxy.DB, proxy.Storage.(*mockStorage)
	name := c.storedName
	content := "artifact " + version
	if c.ecosystem == "helm" {
		name, content = "stable", chartContent(version)
	}
	seedPackage(t, db, store, c.ecosystem, name, c.artifactVersion(version), c.filename(version), content)
}

// TestDownloadCooldownWithholdsCachedArtifacts requests each ecosystem's
// artifact endpoint for a version that was cached before the cooldown was
// raised. A version inside the window must be withheld, from the cache too;
// one past the window, or without a known publish time, is served.
func TestDownloadCooldownWithholdsCachedArtifacts(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	times := map[string]time.Time{
		cooldownOldVersion:    now.Add(-30 * 24 * time.Hour),
		cooldownRecentVersion: now.Add(-time.Hour),
		cooldownUntimed:       {},
	}
	statuses := map[string]int{
		cooldownOldVersion:    http.StatusOK,
		cooldownRecentVersion: http.StatusNotFound,
		cooldownUntimed:       http.StatusOK,
	}

	for _, c := range downloadCooldownCases() {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c.metadata(w, r, times)
		}))
		t.Cleanup(upstream.Close)

		for _, version := range []string{cooldownOldVersion, cooldownRecentVersion, cooldownUntimed} {
			t.Run(c.ecosystem+"/"+version, func(t *testing.T) {
				proxy, db, _, fetcher := setupTestProxy(t)
				proxy.HTTPClient = upstream.Client()
				fetcher.fetchErr = errors.New("artifact must be served from the cache")
				c.seedCachedArtifact(t, proxy, version)
				proxy.Cooldown = &cooldown.Config{Packages: map[string]string{
					canonicalPackagePURL(c.ecosystem, "demo"): "7d",
				}}

				w := httptest.NewRecorder()
				c.handler(proxy, upstream.URL).ServeHTTP(w, httptest.NewRequest(http.MethodGet, c.path(version), nil))

				if w.Code != statuses[version] {
					t.Fatalf("status = %d, want %d (body %q)", w.Code, statuses[version], w.Body.String())
				}
				if w.Code == http.StatusOK && w.Body.String() == "" {
					t.Error("served an empty body")
				}
				if c.storedName == "" || times[version].IsZero() {
					return
				}
				ver, err := db.GetVersionByPURL(canonicalVersionPURL(c.ecosystem, c.storedName, c.artifactVersion(version)))
				if err != nil || ver == nil || !ver.PublishedAt.Valid {
					t.Fatalf("publish time not stored: %+v, %v", ver, err)
				}
				if got, want := ver.PublishedAt.Time, times[version]; !got.Equal(want) {
					t.Errorf("stored publish time = %v, want %v", got, want)
				}
			})
		}
	}
}

// TestDownloadCooldownUsesStoredPublishTime checks that a stored publish
// time decides the download without asking the upstream, which here fails.
func TestDownloadCooldownUsesStoredPublishTime(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(upstream.Close)

	for _, c := range downloadCooldownCases() {
		if c.storedName == "" {
			continue
		}
		t.Run(c.ecosystem, func(t *testing.T) {
			proxy, db, _, fetcher := setupTestProxy(t)
			proxy.HTTPClient = upstream.Client()
			fetcher.fetchErr = errors.New("artifact must be served from the cache")
			c.seedCachedArtifact(t, proxy, cooldownRecentVersion)
			versionPURL := canonicalVersionPURL(c.ecosystem, c.storedName, c.artifactVersion(cooldownRecentVersion))
			packagePURL := canonicalPackagePURL(c.ecosystem, c.storedName)
			if err := db.SetVersionPublishedAt(versionPURL, packagePURL, time.Now().Add(-time.Hour)); err != nil {
				t.Fatalf("seeding publish time: %v", err)
			}
			proxy.Cooldown = &cooldown.Config{Default: "7d"}

			w := httptest.NewRecorder()
			c.handler(proxy, upstream.URL).ServeHTTP(w, httptest.NewRequest(http.MethodGet, c.path(cooldownRecentVersion), nil))
			if w.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (body %q)", w.Code, w.Body.String())
			}
		})
	}
}

// TestDownloadCooldownOtherPackagesSkipLookup checks that a download outside
// every cooldown rule is served without reading upstream metadata. Helm
// needs the index to learn the chart name, so it is left out.
func TestDownloadCooldownOtherPackagesSkipLookup(t *testing.T) {
	for _, c := range downloadCooldownCases() {
		if c.storedName == "" {
			continue
		}
		t.Run(c.ecosystem, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("unexpected upstream request %s", r.URL.Path)
				http.Error(w, "unexpected", http.StatusInternalServerError)
			}))
			t.Cleanup(upstream.Close)
			proxy, _, _, fetcher := setupTestProxy(t)
			proxy.HTTPClient = upstream.Client()
			fetcher.fetchErr = errors.New("artifact must be served from the cache")
			c.seedCachedArtifact(t, proxy, cooldownRecentVersion)
			proxy.Cooldown = &cooldown.Config{Packages: map[string]string{
				canonicalPackagePURL(c.ecosystem, "other"): "7d",
			}}

			w := httptest.NewRecorder()
			c.handler(proxy, upstream.URL).ServeHTTP(w, httptest.NewRequest(http.MethodGet, c.path(cooldownRecentVersion), nil))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %q)", w.Code, strings.TrimSpace(w.Body.String()))
			}
		})
	}
}
