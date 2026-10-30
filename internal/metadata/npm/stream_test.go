package npm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

// rewriteStream runs both passes of RewriteStream over body.
func rewriteStream(packageName string, body []byte, options Options) ([]byte, []string, error) {
	scan, err := ScanPackument(bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	var out bytes.Buffer
	kept, err := RewriteStream(packageName, scan, bytes.NewReader(body), &out, options)
	return out.Bytes(), kept, err
}

// TestRewriteStreamMatchesRewrite checks that the streamed rewrite writes the
// same bytes and keeps the same versions as Rewrite, over documents with the
// filtered members in either order, duplicates, and values Rewrite copies
// verbatim.
func TestRewriteStreamMatchesRewrite(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	ts := func(age time.Duration) string { return now.Add(-age).Format(time.RFC3339) }
	version := func(v string) string {
		return fmt.Sprintf(`%q:{"version":%q,"dist":{"tarball":"https://registry.npmjs.org/demo/-/demo-%s.tgz"}}`, v, v, v)
	}
	versions := `"versions":{` + version("1.0.0") + `,` + version("2.0.0") + `,` + version("3.0.0") + `}`
	times := `"time":{"created":"` + ts(1000*time.Hour) + `","1.0.0":"` + ts(900*time.Hour) + `","2.0.0":"` + ts(800*time.Hour) + `","3.0.0":"` + ts(time.Hour) + `"}`
	tags := `"dist-tags":{"latest":"3.0.0","beta":"2.0.0","next":"1.0.0"}`

	docs := map[string]string{
		"testdata":             string(packument),
		"tags, versions, time": `{"name":"demo",` + tags + `,` + versions + `,` + times + `}`,
		"time first":           `{` + times + `,"name":"demo",` + versions + `,` + tags + `}`,
		"no time":              `{"name":"demo",` + tags + `,` + versions + `}`,
		"time null":            `{"time":null,` + tags + `,` + versions + `}`,
		"tags not an object":   `{"dist-tags":"x",` + versions + `,` + times + `}`,
		"duplicates": `{"versions":{` + version("9.0.0") + `},` + tags + `,"time":{"9.0.0":"` + ts(time.Hour) + `"},` +
			versions + `,` + times + `,"dist-tags":{"latest":"3.0.0"}}`,
		"verbatim values": `{"_big":12345678901234567890,"readme":"<b>&</b>","nested":{"a":[1, 2 ,{"b":null}]},` + versions + `}`,
		"unusual entries": `{"versions":{"1.0.0":"not an object","2.0.0":{"dist":"x"},"3.0.0":{"dist":{"tarball":7}},` +
			`"4.0.0":{"dist":{"tarball":"https://registry.npmjs.org/demo/-/renamed.tgz"}},"5.0.0":{"dist":{}}}}`,
		"whitespace":     "{ \"name\" : \"demo\" ,\n\t\"versions\" : {\n \"1.0.0\" : { \"dist\" : { \"tarball\" : \"https://registry.npmjs.org/demo/-/demo-1.0.0.tgz\" } } } }",
		"empty versions": `{"name":"demo","versions":{}}`,
	}
	options := map[string]Options{
		"plain":    {ProxyURL: "http://proxy"},
		"cooldown": {ProxyURL: "http://proxy", Cooldown: &cooldown.Config{Default: "3d"}, EvaluatedAt: now},
		"denylist": {ProxyURL: "http://proxy", DeniedVersions: map[string]bool{"2.0.0": true}},
		"both": {ProxyURL: "http://proxy", Cooldown: &cooldown.Config{Default: "3d"}, EvaluatedAt: now,
			DeniedVersions: map[string]bool{"1.0.0": true}},
	}
	for docName, doc := range docs {
		for optName, opts := range options {
			t.Run(docName+"/"+optName, func(t *testing.T) {
				want, wantKept, err := Rewrite("demo", []byte(doc), opts)
				if err != nil {
					t.Fatalf("Rewrite: %v", err)
				}
				got, gotKept, err := rewriteStream("demo", []byte(doc), opts)
				if err != nil {
					t.Fatalf("RewriteStream: %v", err)
				}
				// Rewrite keeps the whitespace inside the members of a
				// filtered time map, which the stream doesn't see.
				if !bytes.Equal(compact(t, got), compact(t, want)) {
					t.Errorf("RewriteStream =\n%s\nRewrite =\n%s", got, want)
				}
				if docName == "verbatim values" && !bytes.Contains(got, []byte(`"_big":12345678901234567890,"readme":"<b>&</b>","nested":{"a":[1, 2 ,{"b":null}]}`)) {
					t.Errorf("values not copied verbatim: %s", got)
				}
				slices.Sort(gotKept)
				slices.Sort(wantKept)
				if !slices.Equal(gotKept, wantKept) {
					t.Errorf("kept %v, Rewrite kept %v", gotKept, wantKept)
				}
			})
		}
	}
}

func compact(t *testing.T, doc []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := json.Compact(&out, doc); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, doc)
	}
	return out.Bytes()
}

func TestRewriteStreamCallbacks(t *testing.T) {
	var filtered []string
	var rewritten []int
	options := Options{ProxyURL: "http://proxy", Cooldown: &cooldown.Config{Default: "7d"},
		EvaluatedAt:         time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		OnCooldownFilter:    func(version, _ string) { filtered = append(filtered, version) },
		OnTarballsRewritten: func(count int) { rewritten = append(rewritten, count) },
	}
	if _, _, err := rewriteStream("@example/widgets", packument, options); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(filtered, []string{"3.0.0"}) || !slices.Equal(rewritten, []int{2}) {
		t.Errorf("filtered %v, rewritten %v; want [3.0.0] and [2]", filtered, rewritten)
	}
}

func TestRewriteStreamNotStreamable(t *testing.T) {
	for name, tc := range map[string]struct {
		doc     string
		options Options
	}{
		"single version":  {`{"name":"demo","version":"1.0.0","dist":{"tarball":"t"}}`, Options{}},
		"versions array":  {`{"name":"demo","versions":[{"a":1}]}`, Options{}},
		"versions null":   {`{"name":"demo","versions":null}`, Options{}},
		"pushed versions": {`{"versions":{}}`, Options{LocalVersions: []LocalVersion{{Version: "1.0.0", Entry: []byte(`{}`)}}}},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			scan, err := ScanPackument(strings.NewReader(tc.doc))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := RewriteStream("demo", scan, strings.NewReader(tc.doc), &out, tc.options); !errors.Is(err, ErrNotStreamable) {
				t.Errorf("err = %v, want ErrNotStreamable", err)
			}
			if out.Len() != 0 {
				t.Errorf("wrote %q before refusing", out.String())
			}
		})
	}
}

func TestScanPackument(t *testing.T) {
	for _, body := range []string{`{"versions": {}`, `[1]`, `{"versions": {}} trailing`, `{"a":}`, ``} {
		if _, err := ScanPackument(strings.NewReader(body)); err == nil {
			t.Errorf("ScanPackument(%q): expected an error", body)
		}
	}
	for body, want := range map[string]bool{
		`{"name":"demo","modified":"2024-01-01T00:00:00Z","versions":{}}`:           true,
		`{"name":"demo","modified":"2024-01-01T00:00:00Z","time":{},"versions":{}}`: false,
		`{"name":"demo","versions":{}}`:                                             false,
	} {
		scan, err := ScanPackument(strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if scan.Abbreviated() != want || Abbreviated([]byte(body)) != want {
			t.Errorf("Abbreviated(%s) = %v, want %v", body, scan.Abbreviated(), want)
		}
	}
}
