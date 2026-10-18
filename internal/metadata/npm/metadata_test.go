package npm

import (
	"encoding/json"
	"fmt"
	"github.com/git-pkgs/cooldown"
	"strings"
	"testing"
	"time"
)

// TestNPMRewriteMetadataKeepsUpstreamBytes checks that only tarball URLs
// change: everything else, including numbers encoding/json would round
// through float64 and characters it would HTML-escape, is copied verbatim.
func TestNPMRewriteMetadataKeepsUpstreamBytes(t *testing.T) {
	options := Options{ProxyURL: "http://proxy"}
	input := `{"name":"demo","_big":12345678901234567890,"readme":"<b>&</b>",` +
		`"versions":{"1.0.0":{"version":"1.0.0","dist":{"shasum":"s","tarball":"https://registry.npmjs.org/demo/-/demo-1.0.0.tgz","integrity":"i"}}}}`

	out, _, err := Rewrite("demo", []byte(input), options)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(input, "https://registry.npmjs.org/demo/-/demo-1.0.0.tgz", "http://proxy/npm/demo/-/demo-1.0.0.tgz", 1)
	if string(out) != want {
		t.Errorf("rewrite =\n%s\nwant\n%s", out, want)
	}
}

func TestNPMRewriteMetadataUnusualEntries(t *testing.T) {
	options := Options{ProxyURL: "http://proxy"}
	input := `{
		"versions": {
			"1.0.0": "not an object",
			"2.0.0": {"dist": "no object either"},
			"3.0.0": {"dist": {"tarball": 7}},
			"4.0.0": {"dist": {"tarball": "https://registry.npmjs.org/demo/-/renamed.tgz"}},
			"5.0.0": {"dist": {}}
		}
	}`
	out, _, err := Rewrite("demo", []byte(input), options)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Versions map[string]json.RawMessage `json:"versions"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	for v, want := range map[string]string{
		"1.0.0": `"not an object"`,
		"2.0.0": `{"dist": "no object either"}`,
		"3.0.0": `{"dist": {"tarball": 7}}`,
		"4.0.0": `{"dist": {"tarball": "http://proxy/npm/demo/-/demo-4.0.0.tgz"}}`,
		"5.0.0": `{"dist": {}}`,
	} {
		if got := string(doc.Versions[v]); got != want {
			t.Errorf("%s = %s, want %s", v, got, want)
		}
	}
}

func TestNPMRewriteMetadataInvalidJSON(t *testing.T) {
	options := Options{ProxyURL: "http://proxy"}
	for _, input := range []string{`{"versions": {}`, `[1]`, `{"versions": {}} trailing`} {
		if _, _, err := Rewrite("demo", []byte(input), options); err == nil {
			t.Errorf("%s: expected an error", input)
		}
	}
}

func TestNPMRewriteMetadataNoVersions(t *testing.T) {
	options := Options{ProxyURL: "http://proxy"}
	input := `{"name":"demo","versions":[]}`
	out, _, err := Rewrite("demo", []byte(input), options)
	if err != nil || string(out) != input {
		t.Errorf("rewrite = %s, %v; want input unchanged", out, err)
	}

	options.DeniedVersions = map[string]bool{"1.0.0": true}
	if _, _, err := Rewrite("demo", []byte(input), options); err == nil {
		t.Error("expected an error when a denylisted package has no versions object")
	}
}

// TestNPMRewriteMetadataCooldownAndDenylist runs both filters together and
// checks that versions, their time entries and the tags pointing at them are
// all removed, with latest moved to the newest version left.
func TestNPMRewriteMetadataCooldownAndDenylist(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	ts := func(age time.Duration) string { return now.Add(-age).UTC().Format(time.RFC3339) }

	options := Options{ProxyURL: "http://proxy", EvaluatedAt: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	options.Cooldown = &cooldown.Config{Default: "3d"}
	options.DeniedVersions = map[string]bool{"2.0.0": true}

	version := func(v string) string {
		return fmt.Sprintf(`%q:{"version":%q,"dist":{"tarball":"https://registry.npmjs.org/demo/-/demo-%s.tgz"}}`, v, v, v)
	}
	input := `{"name":"demo",` +
		`"dist-tags":{"latest":"3.0.0","beta":"2.0.0","next":"1.0.0"},` +
		`"versions":{` + version("1.0.0") + `,` + version("2.0.0") + `,` + version("3.0.0") + `},` +
		`"time":{"created":"` + ts(1000*time.Hour) + `","1.0.0":"` + ts(900*time.Hour) + `","2.0.0":"` + ts(800*time.Hour) + `","3.0.0":"` + ts(time.Hour) + `"}}`

	out, _, err := Rewrite("demo", []byte(input), options)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		DistTags map[string]string          `json:"dist-tags"`
		Versions map[string]json.RawMessage `json:"versions"`
		Time     map[string]string          `json:"time"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(doc.Versions) != 1 || doc.Versions["1.0.0"] == nil {
		t.Errorf("versions = %v, want only 1.0.0", doc.Versions)
	}
	if _, ok := doc.Time["2.0.0"]; ok {
		t.Error("time still lists denylisted 2.0.0")
	}
	if _, ok := doc.Time["3.0.0"]; ok {
		t.Error("time still lists 3.0.0, which is inside cooldown")
	}
	if _, ok := doc.Time["created"]; !ok {
		t.Error("time lost its created entry")
	}
	want := map[string]string{"latest": "1.0.0", "next": "1.0.0"}
	if fmt.Sprint(doc.DistTags) != fmt.Sprint(want) {
		t.Errorf("dist-tags = %v, want %v", doc.DistTags, want)
	}
	if !strings.Contains(string(doc.Versions["1.0.0"]), "http://proxy/npm/demo/-/demo-1.0.0.tgz") {
		t.Errorf("1.0.0 tarball not rewritten: %s", doc.Versions["1.0.0"])
	}
}

// TestNPMRewriteMetadataDuplicateKeys checks that a repeated top-level key
// cannot smuggle an unfiltered copy of the versions past the filters.
func TestNPMRewriteMetadataDuplicateKeys(t *testing.T) {
	options := Options{ProxyURL: "http://proxy", EvaluatedAt: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	options.DeniedVersions = map[string]bool{"2.0.0": true}
	input := `{"versions":{"2.0.0":{"dist":{"tarball":"https://registry.npmjs.org/demo/-/demo-2.0.0.tgz"}}},` +
		`"versions":{"1.0.0":{"dist":{"tarball":"https://registry.npmjs.org/demo/-/demo-1.0.0.tgz"}},"2.0.0":{"dist":{"tarball":"https://registry.npmjs.org/demo/-/demo-2.0.0.tgz"}}}}`
	out, _, err := Rewrite("demo", []byte(input), options)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "2.0.0") || strings.Count(string(out), `"versions"`) != 1 {
		t.Errorf("rewrite = %s", out)
	}
}
