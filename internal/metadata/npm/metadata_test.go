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

func TestAbbreviated(t *testing.T) {
	for body, want := range map[string]bool{
		`{"name":"demo","modified":"2024-01-01T00:00:00Z","versions":{}}`:           true,
		`{"name":"demo","modified":"2024-01-01T00:00:00Z","time":{},"versions":{}}`: false,
		`{"name":"demo","versions":{}}`:                                             false,
		`["modified"]`:                                                              false,
		`{"modified":`:                                                              false,
	} {
		if got := Abbreviated([]byte(body)); got != want {
			t.Errorf("Abbreviated(%s) = %v, want %v", body, got, want)
		}
	}
}

// TestNPMRewriteAttestationsURL checks that dist.attestations.url points at
// the proxy whichever order dist lists its members in, and that nothing else
// in dist changes.
func TestNPMRewriteAttestationsURL(t *testing.T) {
	options := Options{ProxyURL: "http://proxy"}
	tests := []struct{ name, version, dist, want string }{
		{"demo", "1.0.0",
			`{"tarball":"https://registry.npmjs.org/demo/-/demo-1.0.0.tgz","attestations":{"url":"https://registry.npmjs.org/-/npm/v1/attestations/demo@1.0.0","provenance":{"predicateType":"p"}}}`,
			`{"tarball":"http://proxy/npm/demo/-/demo-1.0.0.tgz","attestations":{"url":"http://proxy/npm/-/npm/v1/attestations/demo@1.0.0","provenance":{"predicateType":"p"}}}`},
		{"@scope/demo", "2.0.0-rc.1",
			`{"attestations":{"provenance":{},"url":"https://registry.npmjs.org/-/npm/v1/attestations/@scope%2fdemo@2.0.0-rc.1"},"tarball":"https://registry.npmjs.org/@scope/demo/-/demo-2.0.0-rc.1.tgz"}`,
			`{"attestations":{"provenance":{},"url":"http://proxy/npm/-/npm/v1/attestations/@scope%2Fdemo@2.0.0-rc.1"},"tarball":"http://proxy/npm/@scope%2Fdemo/-/demo-2.0.0-rc.1.tgz"}`},
		{"demo", "3.0.0",
			`{"attestations":{"url":7}}`,
			`{"attestations":{"url":7}}`},
	}
	for _, tt := range tests {
		input := `{"versions":{"` + tt.version + `":{"dist":` + tt.dist + `}}}`
		out, _, err := Rewrite(tt.name, []byte(input), options)
		if err != nil {
			t.Fatal(err)
		}
		want := `{"versions":{"` + tt.version + `":{"dist":` + tt.want + `}}}`
		if string(out) != want {
			t.Errorf("%s@%s: rewrite =\n%s\nwant\n%s", tt.name, tt.version, out, want)
		}
	}
}

// TestNPMRewriteCooldownDropsTags checks that cooldown, like the denylist,
// drops a tag other than latest that names a version it withholds, in both
// the in-memory and the streamed rewrite.
func TestNPMRewriteCooldownDropsTags(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	ts := func(age time.Duration) string { return now.Add(-age).UTC().Format(time.RFC3339) }
	options := Options{ProxyURL: "http://proxy", EvaluatedAt: now, Cooldown: &cooldown.Config{Default: "3d"}}
	input := `{"name":"demo",` +
		`"dist-tags":{"latest":"3.0.0","next":"3.0.0","legacy":"1.0.0"},` +
		`"versions":{"1.0.0":{},"2.0.0":{},"3.0.0":{}},` +
		`"time":{"1.0.0":"` + ts(900*time.Hour) + `","2.0.0":"` + ts(800*time.Hour) + `","3.0.0":"` + ts(time.Hour) + `"}}`

	rewrite := map[string]func() ([]byte, []string, error){
		"memory": func() ([]byte, []string, error) { return Rewrite("demo", []byte(input), options) },
		"stream": func() ([]byte, []string, error) { return rewriteStream("demo", []byte(input), options) },
	}
	for name, run := range rewrite {
		t.Run(name, func(t *testing.T) {
			out, _, err := run()
			if err != nil {
				t.Fatal(err)
			}
			var doc struct {
				DistTags map[string]string `json:"dist-tags"`
			}
			if err := json.Unmarshal(out, &doc); err != nil {
				t.Fatalf("output is not JSON: %v\n%s", err, out)
			}
			want := map[string]string{"latest": "2.0.0", "legacy": "1.0.0"}
			if fmt.Sprint(doc.DistTags) != fmt.Sprint(want) {
				t.Errorf("dist-tags = %v, want %v", doc.DistTags, want)
			}
		})
	}
}

// TestNPMRewriteCooldownRetargetsLatestSemver checks that when cooldown
// filtering removes the version dist-tags.latest named, its replacement
// follows semver order instead of publish time: the highest semver version
// below the removed one, ignoring prereleases unless the removed latest was
// one itself, and falling back to the newest by publish time when nothing
// qualifies, leaving dist-tags alone while latest still resolves.
func TestNPMRewriteCooldownRetargetsLatestSemver(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	ts := func(age time.Duration) string { return now.Add(-age).UTC().Format(time.RFC3339) }

	tests := []struct {
		name     string
		latest   string
		versions []struct {
			version string
			age     time.Duration
		}
		want string
	}{
		{
			// A platform-specific prerelease published after the newest
			// eligible stable must not become latest.
			name:   "prerelease published last skipped for stable latest",
			latest: "0.167.0",
			versions: []struct {
				version string
				age     time.Duration
			}{{"0.165.0", 300 * time.Hour}, {"0.166.0", 200 * time.Hour}, {"0.167.0-alpha.4-win32-arm64", 100 * time.Hour}},
			want: "0.166.0",
		},
		{
			// next-shaped: the newest remaining release is a canary, which
			// a stable latest was never allowed to point at.
			name:   "newest canary skipped for older stable latest",
			latest: "16.4.4",
			versions: []struct {
				version string
				age     time.Duration
			}{{"16.5.0-canary.37", 100 * time.Hour}, {"16.4.3", 500 * time.Hour}, {"16.4.2", 700 * time.Hour}},
			want: "16.4.3",
		},
		{
			// pnpm-shaped: a backport published after a newer version stays
			// higher in semver order than one published before it.
			name:   "newer version published earlier wins over late backport",
			latest: "12.18.0",
			versions: []struct {
				version string
				age     time.Duration
			}{{"12.17.0", 400 * time.Hour}, {"11.28.10", 300 * time.Hour}},
			want: "12.17.0",
		},
		{
			name:   "prerelease latest keeps prereleases eligible",
			latest: "2.0.0-rc.2",
			versions: []struct {
				version string
				age     time.Duration
			}{{"2.0.0-rc.1", 300 * time.Hour}, {"1.9.0", 100 * time.Hour}},
			want: "2.0.0-rc.1",
		},
		{
			name:   "only prereleases remain falls back to publish time",
			latest: "3.0.0",
			versions: []struct {
				version string
				age     time.Duration
			}{{"2.0.0-beta.1", 200 * time.Hour}, {"2.0.0-beta.2", 100 * time.Hour}},
			want: "2.0.0-beta.2",
		},
		{
			name:   "latest still present is left alone",
			latest: "2.0.0",
			versions: []struct {
				version string
				age     time.Duration
			}{{"2.0.0", 500 * time.Hour}, {"2.1.0", time.Hour}},
			want: "2.0.0",
		},
		{
			name:   "removed latest not semver takes highest version",
			latest: "next-1.2.3",
			versions: []struct {
				version string
				age     time.Duration
			}{{"1.0.1", 400 * time.Hour}, {"0.9.0", 100 * time.Hour}},
			want: "1.0.1",
		},
		{
			name:   "equal precedence broken by version string",
			latest: "2.0.0",
			versions: []struct {
				version string
				age     time.Duration
			}{{"1.0.0+001", 100 * time.Hour}, {"1.0.0+002", 200 * time.Hour}},
			want: "1.0.0+002",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries := tt.versions
			present := false
			for _, e := range entries {
				present = present || e.version == tt.latest
			}
			if !present {
				// Publish latest an hour ago, so cooldown removes it.
				entries = append(entries[:len(entries):len(entries)], struct {
					version string
					age     time.Duration
				}{tt.latest, time.Hour})
			}
			var versions, times strings.Builder
			for i, v := range entries {
				if i > 0 {
					versions.WriteByte(',')
					times.WriteByte(',')
				}
				fmt.Fprintf(&versions, `%q:{}`, v.version)
				fmt.Fprintf(&times, `%q:%q`, v.version, ts(v.age))
			}
			fmt.Fprintf(&times, `,%q:%q`, "created", ts(1000*time.Hour))
			legacy := tt.versions[0].version
			const input = `{"name":"demo","dist-tags":{"latest":%q,"legacy":%q},"versions":{%s},"time":{%s}}`

			rewrite := map[string]func() ([]byte, []string, error){
				"memory": func() ([]byte, []string, error) {
					return Rewrite("demo", fmt.Appendf(nil, input, tt.latest, legacy, versions.String(), times.String()), Options{ProxyURL: "http://proxy", EvaluatedAt: now, Cooldown: &cooldown.Config{Default: "3d"}})
				},
				"stream": func() ([]byte, []string, error) {
					return rewriteStream("demo", fmt.Appendf(nil, input, tt.latest, legacy, versions.String(), times.String()), Options{ProxyURL: "http://proxy", EvaluatedAt: now, Cooldown: &cooldown.Config{Default: "3d"}})
				},
			}
			for name, run := range rewrite {
				t.Run(name, func(t *testing.T) {
					out, _, err := run()
					if err != nil {
						t.Fatal(err)
					}
					var doc struct {
						DistTags map[string]string `json:"dist-tags"`
					}
					if err := json.Unmarshal(out, &doc); err != nil {
						t.Fatalf("output is not JSON: %v\n%s", err, out)
					}
					want := map[string]string{"latest": tt.want, "legacy": legacy}
					if fmt.Sprint(doc.DistTags) != fmt.Sprint(want) {
						t.Errorf("dist-tags = %v, want %v", doc.DistTags, want)
					}
				})
			}
		})
	}
}
