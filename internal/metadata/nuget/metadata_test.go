package nuget

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

// dayPolicy holds back versions published less than a day before the time
// they are evaluated at, and records what it was asked.
type dayPolicy struct {
	purls       []string
	evaluatedAt []time.Time
}

func (*dayPolicy) Enabled() bool { return true }

func (p *dayPolicy) Evaluate(ecosystem, packagePURL string, publishedAt, evaluatedAt time.Time) cooldown.Decision {
	p.purls = append(p.purls, ecosystem+" "+packagePURL)
	p.evaluatedAt = append(p.evaluatedAt, evaluatedAt)
	return cooldown.Decision{Allowed: evaluatedAt.Sub(publishedAt) >= 24*time.Hour}
}

var evaluatedAt = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

const (
	upstream = "https://api.nuget.example"
	proxy    = "https://proxy.example"
)

type withheldVersion struct {
	id, version string
	at          time.Time
}

func testOptions(policy CooldownPolicy, withheld *[]withheldVersion) Options {
	o := Options{UpstreamURL: upstream, ProxyURL: proxy, Cooldown: policy, EvaluatedAt: evaluatedAt}
	if withheld != nil {
		o.OnWithheld = func(id, version string, at time.Time) {
			*withheld = append(*withheld, withheldVersion{id, version, at})
		}
	}
	return o
}

func TestServiceURL(t *testing.T) {
	tests := map[string]string{
		"PackageBaseAddress/3.0.0":        proxy + "/nuget/v3-flatcontainer/",
		"RegistrationsBaseUrl":            proxy + "/nuget/v3/registration5-semver1/",
		"RegistrationsBaseUrl/3.0.0-rc":   proxy + "/nuget/v3/registration5-semver1/",
		"RegistrationsBaseUrl/3.4.0":      proxy + "/nuget/v3/registration5-gz-semver1/",
		"RegistrationsBaseUrl/3.6.0":      proxy + "/nuget/v3/registration5-gz-semver2/",
		"RegistrationsBaseUrl/Versioned":  proxy + "/nuget/v3/registration5-gz-semver2/",
		"SearchQueryService/3.5.0":        proxy + "/nuget/query",
		"SearchAutocompleteService/3.5.0": proxy + "/nuget/autocomplete",
		"PackagePublish/2.0.0":            "https://other.example/publish",
		"":                                "https://other.example/publish",
	}
	for serviceType, want := range tests {
		if got := ServiceURL(proxy, "https://other.example/publish", serviceType); got != want {
			t.Errorf("ServiceURL(%q) = %q, want %q", serviceType, got, want)
		}
	}
}

func TestRewriteServiceIndex(t *testing.T) {
	body := []byte(`{"version":"3.0.0","resources":[
		{"@id":"https://api.nuget.example/v3-flatcontainer/","@type":"PackageBaseAddress/3.0.0"},
		{"@id":"https://publish.example/","@type":"PackagePublish/2.0.0"},
		{"@type":"SearchQueryService"},
		"not an object"]}`)
	out, err := RewriteServiceIndex(body, proxy)
	if err != nil {
		t.Fatal(err)
	}
	var index struct {
		Version   string `json:"version"`
		Resources []any  `json:"resources"`
	}
	if err := json.Unmarshal(out, &index); err != nil {
		t.Fatal(err)
	}
	if index.Version != "3.0.0" || len(index.Resources) != 4 {
		t.Fatalf("index = %s", out)
	}
	ids := []string{}
	for _, r := range index.Resources[:3] {
		id, _ := r.(map[string]any)["@id"].(string)
		ids = append(ids, id)
	}
	want := []string{proxy + "/nuget/v3-flatcontainer/", "https://publish.example/", ""}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("resource %d @id = %q, want %q", i, ids[i], want[i])
		}
	}

	noResources := []byte(`{"version": "3.0.0"}`)
	if out, err := RewriteServiceIndex(noResources, proxy); err != nil || !bytes.Equal(out, noResources) {
		t.Errorf("index without resources = %s, %v; want it unchanged", out, err)
	}
	if _, err := RewriteServiceIndex([]byte(`{`), proxy); err == nil {
		t.Error("invalid index: no error")
	}
}

func TestDecodeDocument(t *testing.T) {
	if _, err := DecodeDocument([]byte(`null`)); err == nil {
		t.Error("null document: no error")
	}
	if _, err := DecodeDocument([]byte(`[`)); err == nil {
		t.Error("invalid document: no error")
	}
	if doc, err := DecodeDocument([]byte(`{"versions":[]}`)); err != nil || doc["versions"] == nil {
		t.Errorf("DecodeDocument = %v, %v", doc, err)
	}
}

func TestVersionKeyAndUnlistedTime(t *testing.T) {
	if got := VersionKey("1.0.0-Beta+build.5"); got != "1.0.0-beta" {
		t.Errorf("VersionKey = %q", got)
	}
	if !UnlistedTime("1900-01-01T00:00:00+00:00") {
		t.Error("1900 is not unlisted")
	}
	if UnlistedTime("2026-01-01T00:00:00Z") || UnlistedTime("not a time") {
		t.Error("a listed or invalid time is unlisted")
	}
}

func TestPublishedAllowed(t *testing.T) {
	policy := &dayPolicy{}
	var withheld []withheldVersion
	o := testOptions(policy, &withheld)

	recent := evaluatedAt.Add(-time.Hour).Format(time.RFC3339)
	old := evaluatedAt.Add(-48 * time.Hour).Format(time.RFC3339)
	if o.PublishedAllowed("Acme.Tool", "2.0.0-RC+meta", recent) {
		t.Error("recent version allowed")
	}
	if !o.PublishedAllowed("Acme.Tool", "1.0.0", old) {
		t.Error("old version held back")
	}
	if !o.PublishedAllowed("Acme.Tool", "3.0.0", "") || !o.PublishedAllowed("Acme.Tool", "3.0.0", "yesterday") {
		t.Error("version without a valid time held back")
	}
	want := []withheldVersion{{"acme.tool", "2.0.0-rc", evaluatedAt.Add(-time.Hour)}}
	if len(withheld) != 1 || withheld[0] != want[0] {
		t.Errorf("withheld = %v, want %v", withheld, want)
	}
	for i, at := range policy.evaluatedAt {
		if !at.Equal(evaluatedAt) {
			t.Errorf("evaluation %d at %v, want %v", i, at, evaluatedAt)
		}
	}
	if len(policy.purls) == 0 || policy.purls[0] != "nuget pkg:nuget/acme.tool" {
		t.Errorf("evaluated %v", policy.purls)
	}

	// Without a policy and without a callback, everything is let through
	// and nothing reported.
	if !testOptions(nil, nil).PublishedAllowed("Acme.Tool", "2.0.0", recent) {
		t.Error("no policy: version held back")
	}
	if testOptions(policy, nil).PublishedAllowed("Acme.Tool", "2.0.0", recent) {
		t.Error("no callback: recent version allowed")
	}
}

func TestCollectBlocked(t *testing.T) {
	recent := evaluatedAt.Add(-time.Hour).Format(time.RFC3339)
	old := evaluatedAt.Add(-48 * time.Hour).Format(time.RFC3339)
	document := &Times{Items: []Times{
		{Items: []Times{
			{CatalogEntry: CatalogTimes{set: true, version: "1.0.0", published: old}},
			{CatalogEntry: CatalogTimes{set: true, version: "2.0.0+Build", published: recent}},
		}},
		{Version: "3.0.0-Beta", Published: recent},
		// A node without a version blocks nothing.
		{Published: recent},
	}}
	blocked := map[string]bool{}
	testOptions(&dayPolicy{}, nil).CollectBlocked(document, "Acme", blocked)
	if len(blocked) != 2 || !blocked["2.0.0"] || !blocked["3.0.0-beta"] {
		t.Errorf("blocked = %v", blocked)
	}
}

func TestFilterVersions(t *testing.T) {
	document := map[string]any{"versions": []any{"1.0.0", "2.0.0-Beta", "3.0.0+build"}}
	if err := FilterVersions(document, map[string]bool{"2.0.0-beta": true, "3.0.0": true}); err != nil {
		t.Fatal(err)
	}
	if got := document["versions"].([]any); len(got) != 1 || got[0] != "1.0.0" {
		t.Errorf("versions = %v", got)
	}
	if err := FilterVersions(map[string]any{}, nil); err == nil {
		t.Error("no versions: no error")
	}
	if err := FilterVersions(map[string]any{"versions": []any{1}}, nil); err == nil {
		t.Error("non-string version: no error")
	}
}

func TestRegistrationPaths(t *testing.T) {
	ids := map[string]string{
		"/v3/registration5-gz-semver2/acme.tool/index.json": "acme.tool",
		"/v3/registration5-semver1/acme.tool/1.0.0.json":    "acme.tool",
		"/v3-flatcontainer/acme.tool/index.json":            "",
	}
	for path, want := range ids {
		if got := RegistrationID(path); got != want {
			t.Errorf("RegistrationID(%q) = %q, want %q", path, got, want)
		}
	}
	versions := map[string]string{
		"/v3/registration5-gz-semver2/acme.tool/1.0.0.json":  "1.0.0",
		"/v3/registration5-gz-semver2/acme.tool/index.json":  "",
		"/v3/registration5-gz-semver2/acme.tool/page/1.json": "",
		"/v3/registration5-gz-semver2/acme.tool/.json":       "",
		"/v3/registration5-gz-semver2/acme.tool/1.0.0":       "",
		"/v3-flatcontainer/acme.tool/1.0.0.json":             "",
	}
	for path, want := range versions {
		if got := RegistrationLeafVersion(path); got != want {
			t.Errorf("RegistrationLeafVersion(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestLinks(t *testing.T) {
	o := Options{UpstreamURL: upstream + "/base", ProxyURL: proxy}
	links := map[string]string{
		upstream + "/base/v3-flatcontainer/a/1.0.0/a.1.0.0.nupkg":      proxy + "/nuget/v3-flatcontainer/a/1.0.0/a.1.0.0.nupkg",
		upstream + "/base/v3/registration5-semver1/a/index.json?x=1#f": proxy + "/nuget/v3/registration5-semver1/a/index.json?x=1#f",
		"https://elsewhere.example/v3-flatcontainer/a/index.json":      proxy + "/nuget/v3-flatcontainer/a/index.json",
		// The upstream's base path is only taken off links to its host.
		"https://elsewhere.example/base/v3-flatcontainer/a/index.json": "https://elsewhere.example/base/v3-flatcontainer/a/index.json",
		upstream + "/base/v3/catalog0/data/a.json":                     upstream + "/base/v3/catalog0/data/a.json",
		"://bad": "://bad",
	}
	for link, want := range links {
		if got := o.ProxyLink(link); got != want {
			t.Errorf("ProxyLink(%q) = %q, want %q", link, got, want)
		}
	}
	if got := (Options{UpstreamURL: "://bad", ProxyURL: proxy}).ProxyLink(upstream + "/v3-flatcontainer/a"); got != upstream+"/v3-flatcontainer/a" {
		t.Errorf("ProxyLink with a bad upstream = %q", got)
	}

	catalog := map[string]string{
		upstream + "/base/v3/catalog0/data/a.json":    "/v3/catalog0/data/a.json",
		"http://api.nuget.example/base/v3/catalog0/a": "",
		"https://other.example/base/v3/catalog0/a":    "",
		upstream + "/elsewhere/v3/catalog0/a":         "",
		upstream + "/base/v3/../../secret":            "",
		upstream + "/base/v3/%2e%2e/secret":           "",
		upstream + "/base/v3/%252e%252e/secret":       "",
		upstream + "/base/v3/..%5Csecret":             "",
		upstream + "/base/v3/catalog0/a?x=1":          "",
		upstream + "/base/v3/catalog0/a#f":            "",
	}
	for link, want := range catalog {
		got, ok := o.CatalogPath(link)
		if got != want || ok != (want != "") {
			t.Errorf("CatalogPath(%q) = %q, %v; want %q", link, got, ok, want)
		}
	}

	index := "/v3/registration5-gz-semver2/a/index.json"
	if got, err := o.RegistrationPagePath(index, upstream+"/base/v3/registration5-gz-semver2/a/page/1.0.0/2.0.0.json"); err != nil || got != "/v3/registration5-gz-semver2/a/page/1.0.0/2.0.0.json" {
		t.Errorf("RegistrationPagePath = %q, %v", got, err)
	}
	if got, err := o.RegistrationPagePath(index, "page/1.json"); err != nil || got != "/v3/registration5-gz-semver2/a/page/1.json" {
		t.Errorf("relative RegistrationPagePath = %q, %v", got, err)
	}
	for _, link := range []string{"page/../index.json", "index.json", "page/1.json?x=1", "page/1.json#x",
		"https://other.example/base/v3/registration5-gz-semver2/a/page/1.json", "../b/page/1.json"} {
		if got, err := o.RegistrationPagePath(index, link); err == nil {
			t.Errorf("RegistrationPagePath(%q) = %q, want an error", link, got)
		}
	}
}

func TestDecodeTimes(t *testing.T) {
	doc, err := DecodeTimes([]byte(`{"@id":"x","items":[{"@id":"` + upstream + `/v3/registration5-gz-semver2/a/page/1.json","items":[
		{"catalogEntry":{"version":"1.0.0","published":"2026-01-01T00:00:00Z"}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if doc.ID != "" || len(doc.Items) != 1 || doc.Items[0].ID == "" {
		t.Errorf("only page links are kept: %+v", doc)
	}
	if v, p := doc.Items[0].Items[0].Release(); v != "1.0.0" || p != "2026-01-01T00:00:00Z" {
		t.Errorf("Release = %q, %q", v, p)
	}
	leaf, err := DecodeTimes([]byte(`{"catalogEntry":"` + upstream + `/v3/catalog0/a.json","published":"1900-01-01T00:00:00Z"}`))
	if err != nil || leaf.CatalogLink == "" {
		t.Errorf("standalone leaf = %+v, %v", leaf, err)
	}
	if v, p := leaf.Release(); v != "" || p != "1900-01-01T00:00:00Z" {
		t.Errorf("leaf Release = %q, %q", v, p)
	}
	for _, body := range []string{`null`, `[]`, `{"version":"1","version":"2"}`, `{} {}`, `{`} {
		if _, err := DecodeTimes([]byte(body)); err == nil {
			t.Errorf("DecodeTimes(%s): no error", body)
		}
	}
}

// registration is an index with one inlined page of three leaves, the
// newest published an hour before evaluatedAt.
func registration() string {
	leaf := func(version, published string) string {
		return `{"@id":"` + upstream + `/v3/registration5-gz-semver2/acme/` + version + `.json",
			"catalogEntry":{"version":"` + version + `","published":"` + published + `"},
			"packageContent":"` + upstream + `/v3-flatcontainer/acme/` + version + `/acme.` + version + `.nupkg"}`
	}
	old := evaluatedAt.Add(-72 * time.Hour).Format(time.RFC3339)
	recent := evaluatedAt.Add(-time.Hour).Format(time.RFC3339)
	return `{"@id":"` + upstream + `/v3/registration5-gz-semver2/acme/index.json","count":1,"items":[
		{"@id":"` + upstream + `/v3/registration5-gz-semver2/acme/index.json#page/1.0.0/3.0.0",
		 "count":3,"lower":"1.0.0","upper":"3.0.0","items":[` +
		leaf("1.0.0", old) + `,` + leaf("2.0.0", old) + `,` + leaf("3.0.0", recent) + `]}]}`
}

func TestWriteRegistrationFilters(t *testing.T) {
	var withheld []withheldVersion
	var out bytes.Buffer
	if err := WriteRegistration(&out, []byte(registration()), "Acme", true, nil, testOptions(&dayPolicy{}, &withheld)); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		ID    string `json:"@id"`
		Count int    `json:"count"`
		Items []struct {
			Count int    `json:"count"`
			Lower string `json:"lower"`
			Upper string `json:"upper"`
			Items []struct {
				PackageContent string `json:"packageContent"`
			} `json:"items"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("%v: %s", err, out.Bytes())
	}
	if !strings.HasPrefix(doc.ID, proxy+"/nuget/v3/") {
		t.Errorf("@id = %q", doc.ID)
	}
	page := doc.Items[0]
	if doc.Count != 1 || page.Count != 2 || page.Lower != "1.0.0" || page.Upper != "2.0.0" || len(page.Items) != 2 {
		t.Errorf("filtered registration = %s", out.Bytes())
	}
	if want := proxy + "/nuget/v3-flatcontainer/acme/2.0.0/acme.2.0.0.nupkg"; page.Items[1].PackageContent != want {
		t.Errorf("packageContent = %q, want %q", page.Items[1].PackageContent, want)
	}
	if len(withheld) != 1 || withheld[0].version != "3.0.0" || withheld[0].id != "acme" {
		t.Errorf("withheld = %v", withheld)
	}
}

func TestWriteRegistrationWithoutFilter(t *testing.T) {
	var out bytes.Buffer
	if err := WriteRegistration(&out, []byte(registration()), "Acme", false, nil, testOptions(&dayPolicy{}, nil)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), upstream+"/v3") {
		t.Errorf("upstream link left: %s", out.Bytes())
	}
	if n := strings.Count(out.String(), `"catalogEntry"`); n != 3 {
		t.Errorf("%d leaves copied, want 3", n)
	}
}

func TestWriteRegistrationInlinesLinkedPages(t *testing.T) {
	pageLink := upstream + "/v3/registration5-gz-semver2/acme/page/1.0.0/3.0.0.json"
	index := `{"count":1,"items":[{"@id":"` + pageLink + `","count":3,"lower":"1.0.0","upper":"3.0.0"}]}`
	full := registration()
	page := full[strings.Index(full, `{"@id":"`+upstream+`/v3/registration5-gz-semver2/acme/index.json#page`):strings.LastIndex(full, "]}")]
	var out bytes.Buffer
	err := WriteRegistration(&out, []byte(index), "acme", true, map[string][]byte{pageLink: []byte(page)}, testOptions(&dayPolicy{}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out.String(), `"catalogEntry"`); n != 2 || !strings.Contains(out.String(), `"count":2`) {
		t.Errorf("inlined page = %s", out.Bytes())
	}
	if err := WriteRegistration(&bytes.Buffer{}, []byte(index), "acme", true, map[string][]byte{}, testOptions(&dayPolicy{}, nil)); err == nil {
		t.Error("page not loaded: no error")
	}
}
