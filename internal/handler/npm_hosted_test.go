package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/registries/fetch"
)

const requestPackument = `{
	"name": "request",
	"dist-tags": {"latest": "2.88.2"},
	"versions": {
		"2.88.2": {"name": "request", "version": "2.88.2", "dependencies": {"qs": "~6.5.1"},
			"dist": {"tarball": "https://registry.npmjs.org/request/-/request-2.88.2.tgz", "integrity": "sha512-upstream2"}},
		"2.88.3": {"name": "request", "version": "2.88.3", "dependencies": {"old": "1.0.0"},
			"dist": {"tarball": "https://registry.npmjs.org/request/-/request-2.88.3.tgz", "integrity": "sha512-upstream3"}}
	},
	"time": {"modified": "2020-02-11T00:00:00Z", "2.88.2": "2020-02-11T00:00:00Z", "2.88.3": "2020-02-12T00:00:00Z"}
}`

type npmHostedFixture struct {
	proxy            *Proxy
	handler          http.Handler
	upstreamRequests *int
	fetcher          *mockFetcher
}

// newNPMHostedFixture serves packuments from upstream, keyed by request path,
// and answers 404 for anything else.
func newNPMHostedFixture(t *testing.T, upstream map[string]string) npmHostedFixture {
	t.Helper()
	upstreamRequests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests++
		body, ok := upstream[r.URL.EscapedPath()]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	proxy, _, _, fetcher := setupTestProxy(t)
	proxy.HostedEnabled = true
	proxy.HTTPClient = srv.Client()
	return npmHostedFixture{proxy: proxy, handler: NewNPMHandler(proxy, "http://proxy.local", srv.URL).Routes(),
		upstreamRequests: &upstreamRequests, fetcher: fetcher}
}

func (f npmHostedFixture) push(t *testing.T, name, version, manifest string) HostedArtifact {
	t.Helper()
	target, err := HostedTargetFor("npm", name, version, "")
	if err != nil {
		t.Fatalf("HostedTargetFor: %v", err)
	}
	pushed, err := f.proxy.PushLocalArtifact(context.Background(), "npm", target, bytes.NewReader(npmTarball(manifest)), "", "")
	if err != nil {
		t.Fatalf("PushLocalArtifact: %v", err)
	}
	return pushed
}

func (f npmHostedFixture) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

type npmTestPackument struct {
	Name     string                     `json:"name"`
	DistTags map[string]string          `json:"dist-tags"`
	Versions map[string]npmTestVersion  `json:"versions"`
	Time     map[string]string          `json:"time"`
	Extra    map[string]json.RawMessage `json:"-"`
}

type npmTestVersion struct {
	Name         string            `json:"name"`
	Version      string            `json:"version"`
	ID           string            `json:"_id"`
	Dependencies map[string]string `json:"dependencies"`
	Dist         struct {
		Tarball   string `json:"tarball"`
		Integrity string `json:"integrity"`
	} `json:"dist"`
}

func decodePackument(t *testing.T, rec *httptest.ResponseRecorder) npmTestPackument {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var doc npmTestPackument
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decoding packument: %v\n%s", err, rec.Body)
	}
	return doc
}

func TestNPMMetadataListsPushedVersion(t *testing.T) {
	f := newNPMHostedFixture(t, map[string]string{"/request": requestPackument})
	pushed := f.push(t, "request", "2.88.4", `{"name":"request","version":"2.88.4","dependencies":{"qs":"~6.5.2"}}`)

	doc := decodePackument(t, f.get(t, "/request"))

	local, ok := doc.Versions["2.88.4"]
	if !ok {
		t.Fatalf("pushed version missing from versions: %v", doc.Versions)
	}
	if local.Name != "request" || local.Version != "2.88.4" || local.ID != "request@2.88.4" || local.Dependencies["qs"] != "~6.5.2" {
		t.Errorf("pushed version entry = %+v", local)
	}
	if local.Dist.Tarball != "http://proxy.local/npm/request/-/request-2.88.4.tgz" {
		t.Errorf("tarball = %q", local.Dist.Tarball)
	}
	if !strings.HasPrefix(local.Dist.Integrity, "sha512-") {
		t.Errorf("integrity = %q, want the pushed archive's sha512", local.Dist.Integrity)
	}
	if doc.Time["2.88.4"] != pushed.PushedAt.Format(time.RFC3339) {
		t.Errorf("time[2.88.4] = %q, want %q", doc.Time["2.88.4"], pushed.PushedAt.Format(time.RFC3339))
	}
	if doc.Versions["2.88.2"].Dist.Tarball != "http://proxy.local/npm/request/-/request-2.88.2.tgz" {
		t.Errorf("upstream version not rewritten: %+v", doc.Versions["2.88.2"])
	}
	if doc.DistTags["latest"] != "2.88.2" {
		t.Errorf("dist-tags.latest = %q, want upstream's 2.88.2", doc.DistTags["latest"])
	}
}

func TestNPMMetadataPushedVersionReplacesUpstream(t *testing.T) {
	f := newNPMHostedFixture(t, map[string]string{"/request": requestPackument})
	f.push(t, "request", "2.88.3", `{"name":"request","version":"2.88.3","dependencies":{"qs":"~6.5.2"}}`)

	rec := f.get(t, "/request")
	if body := rec.Body.String(); strings.Contains(body, "sha512-upstream3") || strings.Count(body, `"2.88.3":{`) != 1 {
		t.Fatalf("upstream entry for the pushed version still listed:\n%s", body)
	}
	local := decodePackument(t, rec).Versions["2.88.3"]
	if local.Dependencies["qs"] != "~6.5.2" || local.Dependencies["old"] != "" {
		t.Errorf("dependencies = %v, want the pushed package.json's", local.Dependencies)
	}
}

func TestNPMMetadataForPackageOnlyPushed(t *testing.T) {
	f := newNPMHostedFixture(t, nil)
	f.push(t, "@acme/private", "1.10.0", `{"name":"@acme/private","version":"1.10.0"}`)
	f.push(t, "@acme/private", "1.9.0", `{"name":"@acme/private","version":"1.9.0"}`)

	doc := decodePackument(t, f.get(t, "/@acme/private"))
	if doc.Name != "@acme/private" || len(doc.Versions) != 2 {
		t.Errorf("packument = %+v", doc)
	}
	if doc.DistTags["latest"] != "1.10.0" {
		t.Errorf("dist-tags.latest = %q, want the highest pushed version 1.10.0", doc.DistTags["latest"])
	}
	if doc.Versions["1.9.0"].Dist.Tarball != "http://proxy.local/npm/@acme/private/-/private-1.9.0.tgz" {
		t.Errorf("tarball = %q", doc.Versions["1.9.0"].Dist.Tarball)
	}

	if rec := f.get(t, "/@acme/other"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown package status = %d, want 404", rec.Code)
	}
}

func TestNPMVersionMetadataForPushedVersion(t *testing.T) {
	f := newNPMHostedFixture(t, map[string]string{"/request": requestPackument})
	f.push(t, "request", "2.88.4", `{"name":"request","version":"2.88.4","dependencies":{"qs":"~6.5.2"}}`)

	rec := f.get(t, "/request/2.88.4")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var version npmTestVersion
	if err := json.Unmarshal(rec.Body.Bytes(), &version); err != nil {
		t.Fatalf("decoding version: %v", err)
	}
	if version.Version != "2.88.4" || version.Dist.Tarball != "http://proxy.local/npm/request/-/request-2.88.4.tgz" {
		t.Errorf("version document = %+v", version)
	}
}

func TestNPMMetadataFollowsPushAndDelete(t *testing.T) {
	f := newNPMHostedFixture(t, map[string]string{"/request": requestPackument})
	f.proxy.CacheMetadata = true
	f.proxy.MetadataTTL = time.Hour
	f.proxy.SetMetadataRewriteCacheSize(1 << 20)

	before := f.get(t, "/request")
	if before.Code != http.StatusOK {
		t.Fatalf("status = %d", before.Code)
	}

	f.push(t, "request", "2.88.4", `{"name":"request","version":"2.88.4"}`)
	pushed := f.get(t, "/request")
	if _, ok := decodePackument(t, pushed).Versions["2.88.4"]; !ok {
		t.Fatal("pushed version not listed while the upstream document is cached")
	}
	if pushed.Header().Get("ETag") == before.Header().Get("ETag") {
		t.Error("ETag unchanged after a push")
	}

	if err := f.proxy.DeleteLocalVersion("npm", "request", "2.88.4"); err != nil {
		t.Fatalf("DeleteLocalVersion: %v", err)
	}
	if _, ok := decodePackument(t, f.get(t, "/request")).Versions["2.88.4"]; ok {
		t.Error("deleted version still listed")
	}
}

func TestNPMMetadataIgnoresPushesWhenHostedDisabled(t *testing.T) {
	f := newNPMHostedFixture(t, map[string]string{"/request": requestPackument})
	f.push(t, "request", "2.88.4", `{"name":"request","version":"2.88.4"}`)
	f.proxy.HostedEnabled = false

	if _, ok := decodePackument(t, f.get(t, "/request")).Versions["2.88.4"]; ok {
		t.Error("pushed version listed with hosted packages disabled")
	}
}

// With hosted packages disabled, a pushed archive is not served ahead of the
// upstream, but it is still never replaced by the upstream copy: the
// download is answered from its stored record like any cached archive.
func TestNPMDownloadWithHostedDisabledKeepsPush(t *testing.T) {
	f := newNPMHostedFixture(t, map[string]string{"/request": requestPackument})
	manifest := `{"name":"request","version":"2.88.3"}`
	f.push(t, "request", "2.88.3", manifest)
	f.proxy.HostedEnabled = false

	rec := f.get(t, "/request/-/request-2.88.3.tgz")
	if rec.Code != http.StatusOK || rec.Body.String() != string(npmTarball(manifest)) {
		t.Errorf("download status = %d, want 200 and the pushed tarball", rec.Code)
	}
	if f.fetcher.fetchCalled {
		t.Error("the upstream tarball was fetched for a pushed version")
	}
}

func TestNPMDownloadOfPushedPackageWithCooldown(t *testing.T) {
	f := newNPMHostedFixture(t, nil)
	f.proxy.Cooldown = &cooldown.Config{Default: "7d"}
	f.push(t, "@acme/private", "1.0.0", `{"name":"@acme/private","version":"1.0.0"}`)

	rec := f.get(t, "/@acme/private/-/private-1.0.0.tgz")
	if rec.Code != http.StatusOK {
		t.Fatalf("download status = %d, body %s, want the pushed tarball", rec.Code, rec.Body)
	}
	if _, ok := decodePackument(t, f.get(t, "/@acme/private")).Versions["1.0.0"]; !ok {
		t.Error("pushed version missing from the packument with cooldown enabled")
	}
}

func TestNPMDownloadOfPushedOverrideOfVersionInCooldown(t *testing.T) {
	recent := time.Now().Add(-time.Hour).Format(time.RFC3339)
	packument := `{"name":"request","dist-tags":{"latest":"2.88.2"},
		"versions":{
			"2.88.2":{"name":"request","version":"2.88.2","dist":{"tarball":"https://registry.npmjs.org/request/-/request-2.88.2.tgz"}},
			"2.88.3":{"name":"request","version":"2.88.3","dist":{"tarball":"https://registry.npmjs.org/request/-/request-2.88.3.tgz"}}},
		"time":{"2.88.2":"2020-02-11T00:00:00Z","2.88.3":"` + recent + `"}}`
	f := newNPMHostedFixture(t, map[string]string{"/request": packument})
	f.proxy.Cooldown = &cooldown.Config{Default: "7d"}
	f.push(t, "request", "2.88.3", `{"name":"request","version":"2.88.3"}`)

	if _, ok := decodePackument(t, f.get(t, "/request")).Versions["2.88.3"]; !ok {
		t.Fatal("pushed version missing from the packument")
	}
	rec := f.get(t, "/request/-/request-2.88.3.tgz")
	if rec.Code != http.StatusOK || rec.Body.String() != string(npmTarball(`{"name":"request","version":"2.88.3"}`)) {
		t.Errorf("download status = %d, want 200 and the pushed tarball: the packument lists it, so the download must not be withheld", rec.Code)
	}
}

func TestNPMHostedOnlyScopeNeverAsksUpstream(t *testing.T) {
	squatter := `{"name":"@acme/private","dist-tags":{"latest":"1.0.1"},"versions":{"1.0.1":{"name":"@acme/private","version":"1.0.1",
		"dist":{"tarball":"https://registry.npmjs.org/@acme/private/-/private-1.0.1.tgz"}}}}`
	f := newNPMHostedFixture(t, map[string]string{"/@acme%2fprivate": squatter, "/@acme%2Fprivate": squatter, "/request": requestPackument})
	f.proxy.HostedNPMScopes = []string{"@acme"}
	f.push(t, "@acme/private", "1.0.0", `{"name":"@acme/private","version":"1.0.0"}`)

	doc := decodePackument(t, f.get(t, "/@acme/private"))
	if len(doc.Versions) != 1 || doc.DistTags["latest"] != "1.0.0" {
		t.Errorf("packument = %+v, want only the pushed 1.0.0", doc)
	}
	if rec := f.get(t, "/@acme/private/latest"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"version":"1.0.0"`) {
		t.Errorf("latest status = %d, body %s", rec.Code, rec.Body)
	}
	if rec := f.get(t, "/@acme/private/1.0.1"); rec.Code != http.StatusNotFound {
		t.Errorf("unpushed version status = %d, want 404", rec.Code)
	}
	if rec := f.get(t, "/@acme/private/-/private-1.0.1.tgz"); rec.Code != http.StatusNotFound {
		t.Errorf("unpushed tarball status = %d, want 404", rec.Code)
	}
	if rec := f.get(t, "/@acme/other"); rec.Code != http.StatusNotFound {
		t.Errorf("unpushed package status = %d, want 404", rec.Code)
	}
	if rec := f.get(t, "/@acme/private/-/private-1.0.0.tgz"); rec.Code != http.StatusOK {
		t.Errorf("pushed tarball status = %d, want 200", rec.Code)
	}
	if *f.upstreamRequests != 0 {
		t.Errorf("hosted-only scope sent %d requests upstream", *f.upstreamRequests)
	}

	if rec := f.get(t, "/request"); rec.Code != http.StatusOK || *f.upstreamRequests == 0 {
		t.Errorf("package outside the scope status = %d after %d upstream requests, want it fetched upstream", rec.Code, *f.upstreamRequests)
	}
}

func TestNPMDownloadOfUnreadablePushedTarball(t *testing.T) {
	f := newNPMHostedFixture(t, map[string]string{"/request": requestPackument})
	f.push(t, "request", "2.88.3", `{"name":"request","version":"2.88.3"}`)
	removeStored(t, f.proxy, "pkg:npm/request@2.88.3", "request-2.88.3.tgz")

	if rec := f.get(t, "/request/-/request-2.88.3.tgz"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("download status = %d, body %s, want 503 rather than the upstream tarball", rec.Code, rec.Body)
	}
}

func TestNPMMetadataLeavesOutDeniedPushedVersions(t *testing.T) {
	f := newNPMHostedFixture(t, nil)
	f.push(t, "@acme/private", "1.0.0", `{"name":"@acme/private","version":"1.0.0"}`)
	f.push(t, "@acme/private", "1.1.0", `{"name":"@acme/private","version":"1.1.0"}`)
	setTestDenylist(t, f.proxy, "pkg:npm/@acme/private@1.1.0")

	doc := decodePackument(t, f.get(t, "/@acme/private"))
	if _, ok := doc.Versions["1.1.0"]; ok || len(doc.Versions) != 1 {
		t.Errorf("versions = %v, want the denied 1.1.0 left out", doc.Versions)
	}
	if doc.DistTags["latest"] != "1.0.0" {
		t.Errorf("dist-tags.latest = %q, want 1.0.0", doc.DistTags["latest"])
	}
}

func TestNPMDownloadOfPushedTarballWhileStreamingArtifacts(t *testing.T) {
	const manifest = `{"name":"request","version":"2.88.3"}`
	f := newNPMHostedFixture(t, map[string]string{"/request": requestPackument})
	f.fetcher.artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader("upstream tarball")), ContentType: "application/gzip"}
	f.push(t, "request", "2.88.3", manifest)
	f.proxy.StreamArtifacts = true

	rec := f.get(t, "/request/-/request-2.88.3.tgz")
	if rec.Code != http.StatusOK || rec.Body.String() != string(npmTarball(manifest)) {
		t.Errorf("download status = %d, body %q, want 200 and the pushed tarball", rec.Code, rec.Body)
	}
	if f.fetcher.fetchCalled {
		t.Error("a pushed tarball was fetched upstream while streaming artifacts")
	}
}

func TestNPMDownloadOfUnreadablePushedTarballWhileStreamingArtifacts(t *testing.T) {
	f := newNPMHostedFixture(t, map[string]string{"/request": requestPackument})
	f.push(t, "request", "2.88.3", `{"name":"request","version":"2.88.3"}`)
	removeStored(t, f.proxy, "pkg:npm/request@2.88.3", "request-2.88.3.tgz")
	f.proxy.StreamArtifacts = true

	if rec := f.get(t, "/request/-/request-2.88.3.tgz"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("download status = %d, body %s, want 503 rather than the upstream tarball", rec.Code, rec.Body)
	}
}
