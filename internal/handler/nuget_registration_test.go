package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

const nugetTestProxyBase = "http://proxy.test/nuget"

// nugetTestLeaf is a registration leaf as a page embeds it. base is empty for
// the upstream document and the proxy's NuGet URL for the expected one.
func nugetTestLeaf(base, version, published string) string {
	return `{"@id":"` + base + nugetTestRegistration + version + `.json",` +
		`"catalogEntry":{"@id":"https://api.nuget.org/v3/catalog0/data/testpkg.` + version + `.json","id":"TestPkg",` +
		`"dependencyGroups":[{"dependencies":[{"id":"Dep","registration":"` + base + nugetRegistrationPath + `dep/index.json"}]}],` +
		`"published":"` + published + `","version":"` + version + `"},` +
		`"packageContent":"` + base + `/v3-flatcontainer/testpkg/` + version + `/testpkg.` + version + `.nupkg",` +
		`"registration":"` + base + nugetTestRegistration + `index.json"}`
}

func nugetRegistrationRoutes(t *testing.T, policy *cooldown.Config, documents map[string]string) http.Handler {
	t.Helper()
	upstream := nugetDocumentUpstream(t, documents)
	proxy := nugetTestProxy()
	proxy.Cooldown = policy
	return NewNuGetHandlerWithUpstreams(proxy, "http://proxy.test", upstream.URL, upstream.URL).Routes()
}

func assertNuGetJSON(t *testing.T, got, want string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal([]byte(got), &gotValue); err != nil {
		t.Fatalf("response is not JSON: %v: %s", err, got)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("expected document is not JSON: %v: %s", err, want)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Errorf("response differs\n got: %s\nwant: %s", got, want)
	}
}

func TestNuGetRegistrationIndexCooldown(t *testing.T) {
	old := time.Now().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	recent := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	const index = nugetTestRegistration + "index.json"
	const inlined = nugetTestRegistration + "page/1.0.0/2.0.0.json"
	const linked = nugetTestRegistration + "page/3.0.0/4.0.0.json"
	const allRecent = nugetTestRegistration + "page/5.0.0/5.0.0.json"

	routes := nugetRegistrationRoutes(t, &cooldown.Config{Default: "14d"}, map[string]string{
		index: `{"@id":"` + index + `","count":3,"items":[` +
			`{"@id":"` + inlined + `","count":2,"items":[` + nugetTestLeaf("", "1.0.0", old) + `,` + nugetTestLeaf("", "2.0.0", recent) + `],` +
			`"lower":"1.0.0","parent":"` + index + `","upper":"2.0.0"},` +
			`{"@id":"` + linked + `","count":2,"lower":"3.0.0","upper":"4.0.0"},` +
			`{"@id":"` + allRecent + `","count":1,"lower":"5.0.0","upper":"5.0.0"}]}`,
		linked: `{"@id":"` + linked + `","commitId":"only in the page","count":2,"items":[` +
			nugetTestLeaf("", "3.0.0", recent) + `,` + nugetTestLeaf("", "4.0.0", old) + `],"lower":"3.0.0","parent":"` + index + `","upper":"4.0.0"}`,
		allRecent: `{"items":[` + nugetTestLeaf("", "5.0.0", recent) + `]}`,
	})

	w := nugetGet(t, routes, index, http.StatusOK)
	if got := w.Header().Get(headerContentType); got != contentTypeJSON {
		t.Errorf("Content-Type = %q, want %q", got, contentTypeJSON)
	}
	const base = nugetTestProxyBase
	assertNuGetJSON(t, w.Body.String(), `{"@id":"`+base+index+`","count":2,"items":[`+
		`{"@id":"`+base+inlined+`","count":1,"items":[`+nugetTestLeaf(base, "1.0.0", old)+`],`+
		`"lower":"1.0.0","parent":"`+base+index+`","upper":"1.0.0"},`+
		`{"@id":"`+base+linked+`","count":1,"items":[`+nugetTestLeaf(base, "4.0.0", old)+`],"lower":"4.0.0","upper":"4.0.0"}]}`)
}

func TestNuGetRegistrationDocumentCooldown(t *testing.T) {
	old := time.Now().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	recent := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	const base = nugetTestProxyBase
	const index = nugetTestRegistration + "index.json"
	const page = nugetTestRegistration + "page/1.0.0/3.0.0.json"
	const leaf = nugetTestRegistration + "1.0.0.json"
	standalone := func(base, published string) string {
		return `{"@id":"` + base + leaf + `","catalogEntry":"https://api.nuget.org/v3/catalog0/data/testpkg.1.0.0.json","listed":true,` +
			`"packageContent":"` + base + `/v3-flatcontainer/testpkg/1.0.0/testpkg.1.0.0.nupkg","published":"` + published + `",` +
			`"registration":"` + base + index + `"}`
	}

	for _, tt := range []struct {
		name, path, document string
		status               int
		want                 string
	}{
		{
			name: "page",
			path: page,
			document: `{"@id":"` + page + `","count":3,"items":[` + nugetTestLeaf("", "1.0.0", recent) + `,` + nugetTestLeaf("", "2.0.0", old) + `,` +
				nugetTestLeaf("", "3.0.0", recent) + `],"lower":"1.0.0","parent":"` + index + `","upper":"3.0.0"}`,
			status: http.StatusOK,
			want: `{"@id":"` + base + page + `","count":1,"items":[` + nugetTestLeaf(base, "2.0.0", old) + `],` +
				`"lower":"2.0.0","parent":"` + base + index + `","upper":"2.0.0"}`,
		},
		{
			name:     "page with every version in cooldown",
			path:     page,
			document: `{"count":1,"items":[` + nugetTestLeaf("", "1.0.0", recent) + `],"lower":"1.0.0","upper":"1.0.0"}`,
			status:   http.StatusOK,
			want:     `{"count":0,"items":[],"lower":"1.0.0","upper":"1.0.0"}`,
		},
		{
			name:     "page without a count or bounds",
			path:     page,
			document: `{"items":[` + nugetTestLeaf("", "1.0.0", old) + `]}`,
			status:   http.StatusOK,
			want:     `{"count":1,"items":[` + nugetTestLeaf(base, "1.0.0", old) + `]}`,
		},
		{
			name:     "children that are not objects are left out",
			path:     page,
			document: `{"items":[7,` + nugetTestLeaf("", "1.0.0", old) + `,"x",null,[],` + nugetTestLeaf("", "2.0.0", recent) + `],"lower":"0"}`,
			status:   http.StatusOK,
			want:     `{"count":1,"items":[` + nugetTestLeaf(base, "1.0.0", old) + `],"lower":"1.0.0","upper":"1.0.0"}`,
		},
		{
			name:     "escaped member names",
			path:     page,
			document: `{"\u0069tems":[{"catalog\u0045ntry":{"publ\u0069shed":"` + recent + `"}},{"\u0040id":"` + leaf + `"}]}`,
			status:   http.StatusOK,
			want:     `{"count":1,"items":[{"@id":"` + base + leaf + `"}]}`,
		},
		{
			name:     "leaf without a version",
			path:     page,
			document: `{"items":[{"catalogEntry":{"published":"` + old + `"}}],"lower":"1.0.0"}`,
			status:   http.StatusOK,
			want:     `{"count":1,"items":[{"catalogEntry":{"published":"` + old + `"}}],"lower":null,"upper":null}`,
		},
		{
			name:     "leaf without a version after one with a version",
			path:     page,
			document: `{"items":[` + nugetTestLeaf("", "1.0.0", old) + `,{"catalogEntry":{"published":"` + old + `"}}],"lower":"0"}`,
			status:   http.StatusOK,
			want: `{"count":2,"items":[` + nugetTestLeaf(base, "1.0.0", old) + `,{"catalogEntry":{"published":"` + old + `"}}],` +
				`"lower":"1.0.0","upper":null}`,
		},
		{
			name:     "leaf without a publish time is kept",
			path:     page,
			document: `{"items":[{"catalogEntry":{"version":"1.0.0"}},{"catalogEntry":{"published":7,"version":"2.0.0"}}],"lower":"0"}`,
			status:   http.StatusOK,
			want:     `{"count":2,"items":[{"catalogEntry":{"version":"1.0.0"}},{"catalogEntry":{"published":7,"version":"2.0.0"}}],"lower":"1.0.0","upper":"2.0.0"}`,
		},
		{
			name:     "standalone leaf",
			path:     leaf,
			document: standalone("", old),
			status:   http.StatusOK,
			want:     standalone(base, old),
		},
		{
			name:     "standalone leaf in cooldown",
			path:     leaf,
			document: standalone("", recent),
			status:   http.StatusNotFound,
			want:     `{"error":"version not found"}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			routes := nugetRegistrationRoutes(t, &cooldown.Config{Default: "14d"}, map[string]string{tt.path: tt.document})
			assertNuGetJSON(t, nugetGet(t, routes, tt.path, tt.status).Body.String(), tt.want)
		})
	}
}

// An exempt package still gets its links rewritten, but nothing is filtered
// and no page is fetched for it.
func TestNuGetRegistrationWithoutEffectiveCooldown(t *testing.T) {
	recent := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	const base = nugetTestProxyBase
	const index = nugetTestRegistration + "index.json"
	const linked = nugetTestRegistration + "page/2.0.0/2.0.0.json"
	document := func(base string) string {
		return `{"@id":"` + base + index + `","count":9,"items":[` +
			`{"@id":"` + base + nugetTestRegistration + `page/1.0.0/1.0.0.json","count":5,"items":[7,` + nugetTestLeaf(base, "1.0.0", recent) + `],"lower":"0","upper":"9"},` +
			`{"@id":"` + base + linked + `","count":1,"lower":"2.0.0","upper":"2.0.0"}]}`
	}
	policy := &cooldown.Config{Default: "14d", Packages: map[string]string{"pkg:nuget/testpkg": "0"}}
	routes := nugetRegistrationRoutes(t, policy, map[string]string{index: document("")})
	assertNuGetJSON(t, nugetGet(t, routes, index, http.StatusOK).Body.String(), document(base))
}

// A registration that can't be filtered completely is refused as a whole, so
// a client never reads part of one as the full list.
func TestNuGetRegistrationRejectsBrokenPages(t *testing.T) {
	old := time.Now().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	const index = nugetTestRegistration + "index.json"
	const first = nugetTestRegistration + "page/1.0.0/1.0.0.json"
	const second = nugetTestRegistration + "page/2.0.0/2.0.0.json"
	firstPage := `{"items":[` + nugetTestLeaf("", "1.0.0", old) + `]}`
	linkedIndex := `{"items":[{"@id":"` + first + `"},{"@id":"` + second + `"}]}`

	for _, tt := range []struct {
		name      string
		documents map[string]string
		status    int
	}{
		{"missing page", map[string]string{index: linkedIndex, first: firstPage}, http.StatusNotFound},
		{"page that is not JSON", map[string]string{index: linkedIndex, first: firstPage, second: `{"items":[`}, http.StatusBadGateway},
		{"page without leaves", map[string]string{index: linkedIndex, first: firstPage, second: `{"count":1}`}, http.StatusBadGateway},
		{"page that is not an object", map[string]string{index: linkedIndex, first: firstPage, second: `[]`}, http.StatusBadGateway},
		{"index without pages", map[string]string{index: `{"count":0}`}, http.StatusBadGateway},
		{"index page that is not an object", map[string]string{index: `{"items":[7]}`}, http.StatusBadGateway},
		{"index that is not an object", map[string]string{index: `[]`}, http.StatusBadGateway},
		{"empty index", map[string]string{index: `null`}, http.StatusBadGateway},
		{"data after the index", map[string]string{index: `{"items":[]}{}`}, http.StatusBadGateway},
	} {
		t.Run(tt.name, func(t *testing.T) {
			routes := nugetRegistrationRoutes(t, &cooldown.Config{Default: "14d"}, tt.documents)
			body := nugetGet(t, routes, index, tt.status).Body.String()
			if !strings.HasPrefix(body, `{"error":`) || strings.Contains(body, "1.0.0") {
				t.Errorf("response is not only an error: %s", body)
			}
		})
	}
}

// What decides the cooldown is read once and the document copied by other
// code, and both have to read the same document. So members go by their exact
// names, as the generic decoding before did and encoding/json into a struct
// does not, and a document that repeats one of the deciding members, which
// one reader may take the first of and the other the last, is refused.
func TestNuGetRegistrationReadsMembersExactly(t *testing.T) {
	old := time.Now().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	recent := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	const index = nugetTestRegistration + "index.json"
	const page = nugetTestRegistration + "page/1.0.0/3.0.0.json"
	const leaf = nugetTestRegistration + "9.9.9.json"
	inCooldown := `{"catalogEntry":{"version":"9.9.9","published":"` + recent + `"}}`

	for _, tt := range []struct {
		name, path, document string
		status               int
		want                 string
	}{
		{"index with Items", index, `{"Items":[{"@id":"` + page + `","Items":[` + inCooldown + `]}]}`, http.StatusBadGateway, ""},
		{"leaf with Items", leaf, `{"Items":[],"version":"9.9.9","published":"` + recent + `"}`, http.StatusNotFound, ""},
		{"leaf with Published", leaf, `{"published":"` + recent + `","Published":"` + old + `"}`, http.StatusNotFound, ""},
		{"leaf with catalogEntrY", leaf, `{"published":"` + recent + `","catalogEntrY":{"published":"` + old + `"}}`, http.StatusNotFound, ""},
		{"leaf with only publiShed", leaf, `{"publiShed":"` + recent + `"}`, http.StatusOK, `{"publiShed":"` + recent + `"}`},
		{
			"page with a leaf's Published", page, `{"items":[{"catalogEntry":{"version":"9.9.9","published":"` + recent + `","Published":"` + old + `"}}]}`,
			http.StatusOK, `{"items":[],"count":0}`,
		},
		{"items twice", page, `{"items":[` + inCooldown + `],"items":[]}`, http.StatusBadGateway, ""},
		{"items twice in a page", index, `{"items":[{"items":[{}],"items":"x","catalogEntry":{"version":"9.9.9","published":"` + recent + `"}}]}`, http.StatusBadGateway, ""},
		{
			"catalogEntry twice", page, `{"items":[{"catalogEntry":{"published":"` + old + `"},"catalogEntry":"x","published":"` + recent + `"}]}`,
			http.StatusBadGateway, "",
		},
		{"published twice", leaf, `{"published":"` + recent + `","published":"` + old + `"}`, http.StatusBadGateway, ""},
		{"published twice, once escaped", leaf, `{"published":"` + recent + `","publi\u0073hed":"` + old + `"}`, http.StatusBadGateway, ""},
		{"published twice in catalogEntry", page, `{"items":[{"catalogEntry":{"published":"` + recent + `","published":"` + old + `"}}]}`, http.StatusBadGateway, ""},
		{"version twice in catalogEntry", page, `{"items":[{"catalogEntry":{"version":"1.0.0","version":"2.0.0"}}]}`, http.StatusBadGateway, ""},
		{"@id twice", index, `{"items":[{"@id":"` + page + `","@id":"` + page + `"}]}`, http.StatusBadGateway, ""},
		{"another member twice", leaf, `{"listed":false,"listed":true,"published":"` + old + `"}`, http.StatusOK, `{"listed":true,"published":"` + old + `"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			documents := map[string]string{page: `{"items":[` + inCooldown + `]}`}
			documents[tt.path] = tt.document
			routes := nugetRegistrationRoutes(t, &cooldown.Config{Default: "14d"}, documents)
			body := nugetGet(t, routes, tt.path, tt.status).Body.String()
			if tt.status == http.StatusOK {
				assertNuGetJSON(t, body, tt.want)
			} else if !strings.HasPrefix(body, `{"error":`) || strings.Contains(body, "9.9.9") {
				t.Errorf("response is not only an error: %s", body)
			}
		})
	}
}

// A page that the cooldown fetched the leaves of is a page the copy inlines
// them into, also when the page has something that only looks like leaves
// and comes after more of the index than is held back before it is sent.
func TestNuGetRegistrationInlinesWhatWasFetched(t *testing.T) {
	old := time.Now().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	const index = nugetTestRegistration + "index.json"
	const inlined = nugetTestRegistration + "page/1.0.0/1.0.99.json"
	const linked = nugetTestRegistration + "page/2.0.0/2.0.0.json"
	var leaves []string
	for i := range 100 {
		leaves = append(leaves, nugetTestLeaf("", fmt.Sprintf("1.0.%d", i), old))
	}
	document := `{"items":[{"@id":"` + inlined + `","items":[` + strings.Join(leaves, ",") + `]},{"@id":"` + linked + `","Items":[]}]}`
	if len(document) <= nugetFlushSize {
		t.Fatalf("index of %d bytes is sent in one piece", len(document))
	}
	routes := nugetRegistrationRoutes(t, &cooldown.Config{Default: "14d"}, map[string]string{
		index: document, linked: `{"items":[` + nugetTestLeaf("", "2.0.0", old) + `]}`,
	})

	var registration struct {
		Items []struct {
			Count int `json:"count"`
		} `json:"items"`
	}
	body := nugetGet(t, routes, index, http.StatusOK).Body.Bytes()
	if err := json.Unmarshal(body, &registration); err != nil {
		t.Fatalf("response of %d bytes is not JSON: %v", len(body), err)
	}
	if len(registration.Items) != 2 || registration.Items[0].Count != 100 || registration.Items[1].Count != 1 {
		t.Errorf("pages = %+v, want one of 100 leaves and one of 1", registration.Items)
	}
}

// Most links are rewritten without parsing them as URLs. That must give what
// parsing them gives, whatever the upstream, the proxy and the link look like.
func TestNuGetRegistrationLinksMatchProxyLink(t *testing.T) {
	paths := []string{
		"/v3-flatcontainer/testpkg/1.0.0/testpkg.1.0.0.nupkg",
		"/v3/registration5-gz-semver2/testpkg/index.json",
		"/v3/registration5-semver1/testpkg/1.0.0.json",
		"/v3/catalog0/data/testpkg.1.0.0.json#dependencygroup/net8.0",
		"/v3-flatcontainer/test pkg/1.0.0",
		"/v3-flatcontainer/test%2Fpkg/1.0.0",
		"/v3-flatcontainer/testpkg/1.0.0+build",
		"/v3-flatcontainer/testpkg/index.json?semVerLevel=2.0.0#fragment",
		"/v3-flatcontainer/../testpkg",
		"/v3-flatcontainer//testpkg",
		"/v3-flatcontainer/tëstpkg/1.0.0",
		`/v3-flatcontainer/<&>"testpkg"`,
		"/v3-flatcontainer",
		"/V3-flatcontainer/testpkg",
		"/%763-flatcontainer/testpkg",
		"/other/v3-flatcontainer/testpkg",
		"",
	}
	for _, upstream := range []string{
		"https://api.nuget.org", "https://api.nuget.org/", "https://mirror.test:8443/nuget/base", "https://mirror.test/a%20b",
		"https://user:secret@mirror.test", "https://mirror.test/base?key=1", "https://mirror.test/base#fragment", "",
	} {
		for _, proxyURL := range []string{
			"http://proxy.test", "HTTP://Proxy.Test:8080/pre fix", "http://proxy.test/a?b=1", `http://proxy.test/"quoted"`, "",
		} {
			h := &NuGetHandler{proxy: nugetTestProxy(), upstreamURL: upstream, proxyURL: proxyURL}
			for _, path := range paths {
				for _, link := range []string{upstream + path, path, "https://other.test" + path} {
					document, err := json.Marshal(map[string]string{"@id": link})
					if err != nil {
						t.Fatal(err)
					}
					var out strings.Builder
					if err := h.writeNuGetRegistration(&out, document, "testpkg", false, nil); err != nil {
						t.Fatal(err)
					}
					var got map[string]string
					if err := json.Unmarshal([]byte(out.String()), &got); err != nil {
						t.Fatalf("upstream %q, proxy %q, link %q: %v: %s", upstream, proxyURL, link, err, out.String())
					}
					if want := h.nugetProxyLink(link); got["@id"] != want {
						t.Errorf("upstream %q, proxy %q, link %q: rewritten to %q, want %q", upstream, proxyURL, link, got["@id"], want)
					}
				}
			}
		}
	}
}

// discardResponse is a response that keeps nothing, so that a test measures
// what the handler allocates and not what a recorder does.
type discardResponse struct {
	header http.Header
	status int
	bytes  int
}

func (d *discardResponse) Header() http.Header { return d.header }

func (d *discardResponse) WriteHeader(status int) { d.status = status }

func (d *discardResponse) Write(p []byte) (int, error) {
	if d.status == 0 {
		d.status = http.StatusOK
	}
	d.bytes += len(p)
	return len(p), nil
}

// Serving a registration must not build it in memory: a package with many
// versions has megabytes of it, and restore asks for it per package.
func TestNuGetRegistrationAllocatesLessThanRegistration(t *testing.T) {
	const pages, leavesPerPage = 20, 64
	old := time.Now().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	recent := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	const indexPath = nugetTestRegistration + "index.json"

	// As on nuget.org, the links of the registration point at the upstream.
	documents := map[string]string{}
	upstream := nugetDocumentUpstream(t, documents)
	var index strings.Builder
	registrationBytes := 0
	for page := range pages {
		published := old
		if page == pages-1 {
			published = recent
		}
		path := fmt.Sprintf("%spage/%d.json", nugetTestRegistration, page)
		documents[path] = strings.ReplaceAll(nugetRegistrationPage(page*leavesPerPage, leavesPerPage, published), "https://api.nuget.org", upstream.URL)
		registrationBytes += len(documents[path])
		if page > 0 {
			index.WriteByte(',')
		}
		fmt.Fprintf(&index, `{"@id":%q,"count":%d,"lower":"1.%d.0","upper":"1.%d.0"}`, upstream.URL+path, leavesPerPage, page*leavesPerPage, (page+1)*leavesPerPage-1)
	}
	documents[indexPath] = `{"count":` + fmt.Sprint(pages) + `,"items":[` + index.String() + `]}`

	proxy, _, _, _ := setupTestProxy(t)
	proxy.Cooldown = &cooldown.Config{Default: "14d"}
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	routes := NewNuGetHandlerWithUpstreams(proxy, "http://proxy.test", upstream.URL, upstream.URL).Routes()

	// The first request fills the metadata cache; the measured ones read from it.
	var registration struct {
		Count int `json:"count"`
		Items []struct {
			Count int               `json:"count"`
			Upper string            `json:"upper"`
			Items []json.RawMessage `json:"items"`
		} `json:"items"`
	}
	if err := json.Unmarshal(nugetGet(t, routes, indexPath, http.StatusOK).Body.Bytes(), &registration); err != nil {
		t.Fatal(err)
	}
	if registration.Count != pages-1 || len(registration.Items) != pages-1 {
		t.Fatalf("registration has %d pages and count %d, want %d", len(registration.Items), registration.Count, pages-1)
	}
	last := registration.Items[pages-2]
	if last.Count != leavesPerPage || len(last.Items) != leavesPerPage || last.Upper != fmt.Sprintf("1.%d.0", (pages-1)*leavesPerPage-1) {
		t.Fatalf("last page has %d leaves, count %d and upper %q", len(last.Items), last.Count, last.Upper)
	}

	const runs = 5
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range runs {
		response := &discardResponse{header: http.Header{}}
		routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, indexPath, nil))
		if response.status != http.StatusOK || response.bytes < registrationBytes/2 {
			t.Fatalf("status %d with %d bytes", response.status, response.bytes)
		}
	}
	runtime.ReadMemStats(&after)

	perRun := (after.TotalAlloc - before.TotalAlloc) / runs
	t.Logf("allocated %d bytes per request for a %d byte registration", perRun, registrationBytes)
	// One copy is read from the cache. Building the document took eleven more.
	if perRun > 3*uint64(registrationBytes) {
		t.Errorf("registration allocated %d bytes for a %d byte registration", perRun, registrationBytes)
	}
}
