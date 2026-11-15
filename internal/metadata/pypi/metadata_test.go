package pypi

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

// fixedCooldown holds back releases published within a day of the time a
// rewrite is evaluated at, so tests can tell that time from the clock.
type fixedCooldown struct{}

func (fixedCooldown) Enabled() bool { return true }

func (fixedCooldown) Evaluate(_, _ string, publishedAt, evaluatedAt time.Time) cooldown.Decision {
	return cooldown.Decision{Allowed: evaluatedAt.Sub(publishedAt) >= 24*time.Hour}
}

const jsonDocument = `{
	"info": {"name": "Demo", "version": "3.0.0"},
	"releases": {
		"1.0.0": [{"url": "https://files.example/packages/aa/demo-1.0.0.tar.gz", "upload_time_iso_8601": "2026-01-01T00:00:00Z"}],
		"2.0.0": [{"url": "https://files.example/packages/bb/demo-2.0.0.tar.gz", "upload_time_iso_8601": "2026-01-10T00:00:00Z"}],
		"3.0.0": [
			{"url": "https://files.example/packages/cc/demo-3.0.0.tar.gz", "upload_time_iso_8601": "2026-01-09T00:00:00Z"},
			{"url": "https://files.example/packages/dd/demo-3.0.0-py3-none-any.whl", "upload_time_iso_8601": "2026-01-10T12:00:00Z"}
		]
	},
	"urls": [{"url": "https://files.example/packages/dd/demo-3.0.0-py3-none-any.whl", "upload_time_iso_8601": "2026-01-10T12:00:00Z"}]
}`

func testOptions() Options {
	return Options{
		ProxyURL:    "https://proxy.example",
		DownloadURL: "https://files.example",
		Cooldown:    fixedCooldown{},
		EvaluatedAt: time.Date(2026, 1, 11, 0, 0, 0, 0, time.UTC),
	}
}

// Cooldown is decided at EvaluatedAt, by a release's newest file; the
// denylist leaves out what it names, and both are reported.
func TestRewriteJSON(t *testing.T) {
	o := testOptions()
	o.Denied = func(name, version string) bool { return name == "Demo" && version == "1.0.0" }
	var filtered, withheld []string
	var withheldAt time.Time
	o.OnFiltered = func(name, version string) { filtered = append(filtered, name+"@"+version) }
	o.OnWithheld = func(name, version string, at time.Time) {
		withheld = append(withheld, name+"@"+version)
		if version == "3.0.0" {
			withheldAt = at
		}
	}

	out, kept, err := RewriteJSON([]byte(jsonDocument), o)
	if err != nil {
		t.Fatalf("RewriteJSON: %v", err)
	}
	slices.Sort(kept)
	if !slices.Equal(kept, []string{"2.0.0"}) {
		t.Errorf("kept = %q, want only 2.0.0", kept)
	}
	slices.Sort(filtered)
	if !slices.Equal(filtered, []string{"Demo@1.0.0", "Demo@3.0.0"}) {
		t.Errorf("filtered = %v", filtered)
	}
	// 3.0.0 twice: as a release, and as the urls of the current version.
	if !slices.Equal(withheld, []string{"Demo@3.0.0", "Demo@3.0.0"}) {
		t.Errorf("withheld = %v", withheld)
	}
	if want := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC); !withheldAt.Equal(want) {
		t.Errorf("withheld at %v, want the newest file's %v", withheldAt, want)
	}

	var doc struct {
		Releases map[string][]struct{ URL string } `json:"releases"`
		URLs     []any                             `json:"urls"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if got := doc.Releases["2.0.0"][0].URL; got != "https://proxy.example/pypi/packages/packages/bb/demo-2.0.0.tar.gz" {
		t.Errorf("2.0.0 URL = %q", got)
	}
	if len(doc.URLs) != 0 {
		t.Errorf("urls = %v, want them dropped with 3.0.0", doc.URLs)
	}
}

// Without a policy or a denylist everything is kept and only URLs change.
func TestRewriteJSONWithoutPolicy(t *testing.T) {
	o := Options{ProxyURL: "https://proxy.example", DownloadURL: "https://files.example"}
	out, kept, err := RewriteJSON([]byte(jsonDocument), o)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 4 {
		t.Errorf("kept = %q, want three releases and the urls", kept)
	}
	if strings.Contains(string(out), "https://files.example") {
		t.Errorf("a download URL was left: %s", out)
	}
}

func TestRewriteSimpleHTMLAndJSON(t *testing.T) {
	o := testOptions()
	html := `<a href="https://files.example/packages/aa/demo-1.0.0.tar.gz#sha256=1">demo-1.0.0.tar.gz</a>
<a href="https://files.example/packages/bb/demo-2.0.0.tar.gz#sha256=2">demo-2.0.0.tar.gz</a>`
	got := string(RewriteSimpleHTML(FilterSimpleHTMLLinks([]byte(html), map[string]bool{"1.0.0": true}), map[string]bool{"1.0.0": true}, o))
	if strings.Contains(got, "1.0.0") || !strings.Contains(got, `href="https://proxy.example/pypi/packages/packages/bb/demo-2.0.0.tar.gz#sha256=2"`) {
		t.Errorf("simple HTML = %q", got)
	}

	simple := `{"files": [
		{"filename": "demo-1.0.0.tar.gz", "url": "https://files.example/packages/aa/demo-1.0.0.tar.gz"},
		{"filename": "demo-2.0.0.tar.gz", "url": "https://files.example/packages/bb/demo-2.0.0.tar.gz"}],
		"versions": ["1.0.0", "2.0.0"]}`
	out, err := RewriteSimpleJSON([]byte(simple), map[string]bool{"1.0.0": true}, o)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Files    []struct{ URL string } `json:"files"`
		Versions []string               `json:"versions"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Files) != 1 || doc.Files[0].URL != "https://proxy.example/pypi/packages/packages/bb/demo-2.0.0.tar.gz" ||
		!slices.Equal(doc.Versions, []string{"2.0.0"}) {
		t.Errorf("simple JSON = %s", out)
	}
}

func TestReleaseTimes(t *testing.T) {
	times, err := ReleaseTimes([]byte(jsonDocument))
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC); !times["3.0.0"].Equal(want) {
		t.Errorf("3.0.0 = %v, want the newest file's %v", times["3.0.0"], want)
	}
}

func TestFilenameIdentity(t *testing.T) {
	for filename, want := range map[string][2]string{
		"requests-2.31.0-py3-none-any.whl":          {"requests", "2.31.0"},
		"requests-2.31.0.tar.gz":                    {"requests", "2.31.0"},
		"requests-2.31.0-py3-none-any.whl.metadata": {"requests", "2.31.0"},
		"numpy-1.8.0-py2.7-macosx-10.9-x86_64.egg":  {"numpy", "1.8.0"},
		"numpy-1.8.0.win32-py2.7.exe":               {"numpy", "1.8.0"},
		"README.txt":                                {"", ""},
	} {
		if name, version := FilenameIdentity(filename); name != want[0] || version != want[1] {
			t.Errorf("FilenameIdentity(%q) = %q, %q; want %q, %q", filename, name, version, want[0], want[1])
		}
	}
}

// disabledCooldown would hold back everything, but is off.
type disabledCooldown struct{}

func (disabledCooldown) Enabled() bool { return false }

func (disabledCooldown) Evaluate(_, _ string, _, _ time.Time) cooldown.Decision {
	return cooldown.Decision{Allowed: false}
}

// A policy that is turned off holds nothing back.
func TestRewriteJSONWithDisabledCooldown(t *testing.T) {
	o := testOptions()
	o.Cooldown = disabledCooldown{}
	_, kept, err := RewriteJSON([]byte(jsonDocument), o)
	if err != nil || len(kept) != 4 {
		t.Errorf("kept = %q, %v; want every release and the urls", kept, err)
	}
}
