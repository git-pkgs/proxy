package handler

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/registries/fetch"
)

const nugetTestRegistration = nugetRegistrationPath + "testpkg/"

// nugetDocumentUpstream serves the given documents by path and 404s the rest.
func nugetDocumentUpstream(t *testing.T, documents map[string]string) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := documents[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func TestNuGetVersionListCooldown(t *testing.T) {
	old := time.Now().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	recent := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	entry := func(version, published string) string {
		return `{"catalogEntry":{"id":"TestPkg","version":"` + version + `","published":"` + published + `"}}`
	}
	const versions = `{"versions":["1.0.0","2.0.0","3.0.0-beta"]}`
	const pageOne = nugetTestRegistration + "page/1.0.0/1.0.0.json"
	const pageTwo = nugetTestRegistration + "page/2.0.0/3.0.0-beta.json"

	for _, tt := range []struct {
		name      string
		documents map[string]string
		want      string
	}{
		{
			name: "inlined pages",
			documents: map[string]string{
				nugetTestRegistration + "index.json": `{"items":[{"@id":"` + pageOne + `","items":[` + entry("1.0.0", old) + `]},` +
					`{"@id":"` + pageTwo + `","items":[` + entry("2.0.0", recent) + `,` + entry("3.0.0-beta", old) + `]}]}`,
			},
			want: `{"versions":["1.0.0","3.0.0-beta"]}`,
		},
		{
			name: "linked pages",
			documents: map[string]string{
				nugetTestRegistration + "index.json": `{"items":[{"@id":"` + pageOne + `"},{"@id":"` + pageTwo + `"}]}`,
				pageOne:                              `{"items":[` + entry("1.0.0", recent) + `]}`,
				pageTwo:                              `{"items":[` + entry("2.0.0", old) + `,` + entry("3.0.0-beta", recent) + `]}`,
			},
			want: `{"versions":["2.0.0"]}`,
		},
		{
			name: "inlined and linked pages",
			documents: map[string]string{
				nugetTestRegistration + "index.json": `{"items":[{"@id":"` + pageOne + `","items":[` + entry("1.0.0", recent) + `]},{"@id":"` + pageTwo + `"}]}`,
				pageTwo:                              `{"items":[` + entry("2.0.0", recent) + `,` + entry("3.0.0-beta", old) + `]}`,
			},
			want: `{"versions":["3.0.0-beta"]}`,
		},
		{
			name: "empty inlined page is not fetched",
			documents: map[string]string{
				nugetTestRegistration + "index.json": `{"items":[{"@id":"` + pageOne + `","items":[]},{"@id":"` + pageTwo + `","items":[` + entry("2.0.0", recent) + `]}]}`,
			},
			want: `{"versions":["1.0.0","3.0.0-beta"]}`,
		},
		{
			name: "version is compared without case and build metadata",
			documents: map[string]string{
				nugetTestRegistration + "index.json": `{"items":[{"items":[` + entry("3.0.0-BETA+build.5", recent) + `]}]}`,
			},
			want: `{"versions":["1.0.0","2.0.0"]}`,
		},
		{
			name: "leaf fields are used when catalogEntry is not an object",
			documents: map[string]string{
				nugetTestRegistration + "index.json": `{"items":[{"items":[` +
					`{"catalogEntry":"https://example.test/catalog/1.json","version":"1.0.0","published":"` + recent + `"},` +
					`{"catalogEntry":null,"version":"2.0.0","published":"` + recent + `"},` +
					`{"version":"3.0.0-beta","published":"` + old + `"}]}]}`,
			},
			want: `{"versions":["3.0.0-beta"]}`,
		},
		{
			name: "leaf fields are ignored when catalogEntry is an object",
			documents: map[string]string{
				nugetTestRegistration + "index.json": `{"items":[{"items":[` +
					`{"catalogEntry":{},"version":"1.0.0","published":"` + recent + `"},` +
					`{"catalogEntry":{"version":"2.0.0","published":"` + old + `"},"version":"2.0.0","published":"` + recent + `"}]}]}`,
			},
			want: versions,
		},
		{
			name: "leaves without a usable publish time are allowed",
			documents: map[string]string{
				nugetTestRegistration + "index.json": `{"items":[{"items":[` +
					entry("1.0.0", "yesterday") + `,` +
					`{"catalogEntry":{"version":"2.0.0","published":1700000000}},` +
					`{"catalogEntry":{"version":"3.0.0-beta"}}]}]}`,
			},
			want: versions,
		},
		{
			name: "values of the wrong type are skipped",
			documents: map[string]string{
				nugetTestRegistration + "index.json": `{"count":"two","items":[{"@id":7,"lower":{},"items":[` +
					`{"@id":[],"catalogEntry":{"version":1,"published":"` + recent + `","tags":"a"}},` +
					`{"items":"none","catalogEntry":{"version":"2.0.0","published":"` + recent + `"}}]}]}`,
			},
			want: `{"versions":["1.0.0","3.0.0-beta"]}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.documents["/v3-flatcontainer/testpkg/index.json"] = versions
			upstream := nugetDocumentUpstream(t, tt.documents)
			p := nugetTestProxy()
			p.Cooldown = &cooldown.Config{Default: "14d"}
			h := NewNuGetHandlerWithUpstreams(p, "http://proxy.test", upstream.URL, upstream.URL)
			w := nugetGet(t, h.Routes(), "/v3-flatcontainer/TestPkg/index.json", http.StatusOK)
			if got := strings.TrimSpace(w.Body.String()); got != tt.want {
				t.Errorf("version list = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestNuGetVersionListRejectsBrokenRegistration(t *testing.T) {
	const page = nugetTestRegistration + "page/1.0.0/2.0.0.json"
	linked := `{"items":[{"@id":"` + page + `"}]}`
	for _, tt := range []struct {
		name      string
		documents map[string]string
	}{
		{"null index", map[string]string{nugetTestRegistration + "index.json": `null`}},
		{"index is not an object", map[string]string{nugetTestRegistration + "index.json": `[]`}},
		{"truncated index", map[string]string{nugetTestRegistration + "index.json": `{"items":[{"items":[`}},
		{"index without pages", map[string]string{nugetTestRegistration + "index.json": `{"count":0}`}},
		{"pages are not a list", map[string]string{nugetTestRegistration + "index.json": `{"items":"none"}`}},
		{"page is not an object", map[string]string{nugetTestRegistration + "index.json": `{"items":["` + page + `"]}`}},
		{"page without a link", map[string]string{nugetTestRegistration + "index.json": `{"items":[{"count":1}]}`}},
		{"missing page", map[string]string{nugetTestRegistration + "index.json": linked}},
		{"null page", map[string]string{nugetTestRegistration + "index.json": linked, page: `null`}},
		{"page is not an object", map[string]string{nugetTestRegistration + "index.json": linked, page: `[]`}},
		{"page without leaves", map[string]string{nugetTestRegistration + "index.json": linked, page: `{"count":2}`}},
		{"truncated page", map[string]string{nugetTestRegistration + "index.json": linked, page: `{"items":[{"catalogEntry":{"version":`}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.documents["/v3-flatcontainer/testpkg/index.json"] = `{"versions":["1.0.0","2.0.0"]}`
			upstream := nugetDocumentUpstream(t, tt.documents)
			p := nugetTestProxy()
			p.Cooldown = &cooldown.Config{Default: "14d"}
			h := NewNuGetHandlerWithUpstreams(p, "http://proxy.test", upstream.URL, upstream.URL)
			status := http.StatusBadGateway
			if tt.name == "missing page" {
				status = http.StatusNotFound
			}
			nugetGet(t, h.Routes(), "/v3-flatcontainer/testpkg/index.json", status)
		})
	}
}

func TestNuGetVersionListRejectsUnsafePageLinks(t *testing.T) {
	for _, link := range []string{
		"https://other.example/page.json",
		nugetRegistrationPath + "other/page/1/2.json",
		"page/../index.json",
		"index.json",
		"page/1.json?x=1",
		"page/1.json#x",
	} {
		t.Run(link, func(t *testing.T) {
			registrationRequests := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v3-flatcontainer/testpkg/index.json" {
					_, _ = io.WriteString(w, `{"versions":["1.0.0"]}`)
					return
				}
				registrationRequests++
				_, _ = io.WriteString(w, `{"items":[{"@id":"`+link+`"}]}`)
			}))
			defer upstream.Close()
			p := nugetTestProxy()
			p.Cooldown = &cooldown.Config{Default: "14d"}
			h := NewNuGetHandlerWithUpstreams(p, "http://proxy.test", upstream.URL, upstream.URL)
			nugetGet(t, h.Routes(), "/v3-flatcontainer/testpkg/index.json", http.StatusBadGateway)
			if registrationRequests != 1 {
				t.Fatalf("unsafe page link was followed (%d registration requests)", registrationRequests)
			}
		})
	}
}

func TestNuGetDownloadCooldownLeafShapes(t *testing.T) {
	old := time.Now().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	recent := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	const catalog = `"catalogEntry":"https://api.nuget.org/v3/catalog0/data/2024.01.02.03.04.05/testpkg.2.0.0.json"`
	for _, tt := range []struct {
		name, leaf string
		want       int
	}{
		{"recent standalone leaf", `{` + catalog + `,"listed":true,"published":"` + recent + `"}`, http.StatusNotFound},
		{"old standalone leaf", `{` + catalog + `,"listed":true,"published":"` + old + `"}`, http.StatusOK},
		{"catalogEntry object wins", `{"catalogEntry":{"published":"` + recent + `"},"published":"` + old + `"}`, http.StatusNotFound},
		{"catalogEntry object without a time", `{"catalogEntry":{"version":"2.0.0"},"published":"` + recent + `"}`, http.StatusOK},
		{"time is not a string", `{"published":1700000000}`, http.StatusOK},
		{"leaf is not an object", `["` + recent + `"]`, http.StatusBadGateway},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p, _, _, fetcher := setupTestProxy(t)
			p.Cooldown = &cooldown.Config{Default: "14d"}
			fetcher.artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader("package")), ContentType: "application/octet-stream"}
			upstream := nugetDocumentUpstream(t, map[string]string{nugetTestRegistration + "2.0.0.json": tt.leaf})
			h := NewNuGetHandlerWithUpstreams(p, "http://proxy.test", upstream.URL, upstream.URL)
			nugetGet(t, h.Routes(), "/v3-flatcontainer/testpkg/2.0.0/testpkg.2.0.0.nupkg", tt.want)
			if fetcher.fetchCalled != (tt.want == http.StatusOK) {
				t.Errorf("artifact fetch called = %v", fetcher.fetchCalled)
			}
		})
	}
}

// nugetRegistrationPage builds a registration page shaped like nuget.org's,
// with the catalog fields that make up most of a real one.
func nugetRegistrationPage(first, leaves int, published string) string {
	var b strings.Builder
	b.WriteString(`{"@id":"https://api.nuget.org/v3/registration5-gz-semver2/testpkg/page.json","@type":"catalog:CatalogPage",` +
		`"commitId":"0b2f1c8e-5d5c-4b0f-9d0a-0d6f5c1f2a3b","commitTimeStamp":"2024-01-02T03:04:05.1234567+00:00","count":64,"items":[`)
	for i := range leaves {
		if i > 0 {
			b.WriteByte(',')
		}
		version := fmt.Sprintf("1.%d.0", first+i)
		fmt.Fprintf(&b, `{"@id":"https://api.nuget.org/v3/registration5-gz-semver2/testpkg/%[1]s.json","@type":"Package",`+
			`"commitId":"0b2f1c8e-5d5c-4b0f-9d0a-0d6f5c1f2a3b","commitTimeStamp":"2024-01-02T03:04:05.1234567+00:00",`+
			`"catalogEntry":{"@id":"https://api.nuget.org/v3/catalog0/data/2024.01.02.03.04.05/testpkg.%[1]s.json","@type":"PackageDetails",`+
			`"authors":"Example Authors","dependencyGroups":[`+
			`{"@id":"https://api.nuget.org/v3/catalog0/data/2024.01.02.03.04.05/testpkg.%[1]s.json#dependencygroup/.netstandard2.0","@type":"PackageDependencyGroup",`+
			`"dependencies":[{"@id":"https://api.nuget.org/v3/catalog0/data/2024.01.02.03.04.05/testpkg.%[1]s.json#dependencygroup/.netstandard2.0/example.dependency",`+
			`"@type":"PackageDependency","id":"Example.Dependency","range":"[8.0.0, )","registration":"https://api.nuget.org/v3/registration5-gz-semver2/example.dependency/index.json"}],`+
			`"targetFramework":".NETStandard2.0"},`+
			`{"@id":"https://api.nuget.org/v3/catalog0/data/2024.01.02.03.04.05/testpkg.%[1]s.json#dependencygroup/net8.0","@type":"PackageDependencyGroup","targetFramework":"net8.0"}],`+
			`"description":"An example package with a description about as long as the ones real packages publish for every version.",`+
			`"iconUrl":"https://api.nuget.org/v3-flatcontainer/testpkg/%[1]s/icon","id":"TestPkg","language":"","licenseExpression":"Apache-2.0",`+
			`"licenseUrl":"https://www.nuget.org/packages/TestPkg/%[1]s/license","listed":true,"minClientVersion":"",`+
			`"packageContent":"https://api.nuget.org/v3-flatcontainer/testpkg/%[1]s/testpkg.%[1]s.nupkg","projectUrl":"https://example.test/testpkg",`+
			`"published":"%[2]s","requireLicenseAcceptance":false,"summary":"","tags":["example","testing","package"],"title":"TestPkg","version":"%[1]s"},`+
			`"packageContent":"https://api.nuget.org/v3-flatcontainer/testpkg/%[1]s/testpkg.%[1]s.nupkg",`+
			`"registration":"https://api.nuget.org/v3/registration5-gz-semver2/testpkg/index.json"}`, version, published)
	}
	b.WriteString(`],"lower":"1.0.0","parent":"https://api.nuget.org/v3/registration5-gz-semver2/testpkg/index.json","upper":"1.63.0"}`)
	return b.String()
}

// Listing versions must not build every registration page in memory: restore
// asks for the list of each package it resolves, and only the publish times
// of a registration matter for it.
func TestNuGetVersionListAllocatesLessThanRegistration(t *testing.T) {
	const pages, leavesPerPage = 20, 64
	old := time.Now().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	recent := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)

	documents := map[string]string{}
	var index, versions strings.Builder
	registrationBytes := 0
	for page := range pages {
		published := old
		if page == pages-1 {
			published = recent
		}
		path := fmt.Sprintf("%spage/%d.json", nugetTestRegistration, page)
		documents[path] = nugetRegistrationPage(page*leavesPerPage, leavesPerPage, published)
		registrationBytes += len(documents[path])
		if page > 0 {
			index.WriteByte(',')
		}
		fmt.Fprintf(&index, `{"@id":%q,"count":%d}`, path, leavesPerPage)
	}
	for version := range pages * leavesPerPage {
		if version > 0 {
			versions.WriteByte(',')
		}
		fmt.Fprintf(&versions, `"1.%d.0"`, version)
	}
	documents[nugetTestRegistration+"index.json"] = `{"count":` + fmt.Sprint(pages) + `,"items":[` + index.String() + `]}`
	documents["/v3-flatcontainer/testpkg/index.json"] = `{"versions":[` + versions.String() + `]}`
	upstream := nugetDocumentUpstream(t, documents)

	proxy, _, _, _ := setupTestProxy(t)
	proxy.Cooldown = &cooldown.Config{Default: "14d"}
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	h := NewNuGetHandlerWithUpstreams(proxy, "http://proxy.test", upstream.URL, upstream.URL)
	routes := h.Routes()
	list := func() string {
		t.Helper()
		return nugetGet(t, routes, "/v3-flatcontainer/testpkg/index.json", http.StatusOK).Body.String()
	}

	// The first request fills the metadata cache; the measured ones read from it.
	body := list()
	allowed := (pages - 1) * leavesPerPage
	if got := strings.Count(body, `"1.`); got != allowed {
		t.Fatalf("version list has %d versions, want %d", got, allowed)
	}
	if strings.Contains(body, fmt.Sprintf(`"1.%d.0"`, allowed)) {
		t.Fatal("version list contains a version in cooldown")
	}

	const runs = 5
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range runs {
		list()
	}
	runtime.ReadMemStats(&after)

	perRun := (after.TotalAlloc - before.TotalAlloc) / runs
	t.Logf("allocated %d bytes per request for a %d byte registration", perRun, registrationBytes)
	// One copy is read from the cache. Decoding the old way took four more.
	if perRun > 3*uint64(registrationBytes) {
		t.Errorf("version list allocated %d bytes for a %d byte registration", perRun, registrationBytes)
	}
}
