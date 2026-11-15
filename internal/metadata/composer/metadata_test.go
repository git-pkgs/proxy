package composer

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

// dayCooldown holds back versions published within a day of the time a
// rewrite is evaluated at, so tests can tell that time from the clock.
type dayCooldown struct{ enabled bool }

func (c dayCooldown) Enabled() bool { return c.enabled }

func (dayCooldown) Evaluate(_, _ string, publishedAt, evaluatedAt time.Time) cooldown.Decision {
	return cooldown.Decision{Allowed: evaluatedAt.Sub(publishedAt) >= 24*time.Hour}
}

// A minified document: the second entry inherits the first one's fields.
const minifiedDocument = `{"minified": "composer/2.0", "packages": {"acme/tool": [
	{"name": "acme/tool", "version": "2.0.0", "time": "2026-01-10T12:00:00+00:00",
	 "dist": {"type": "zip", "url": "https://codeload.example/acme/tool/zip/bbb", "shasum": ""}},
	{"version": "1.0.0", "time": "2026-01-01T00:00:00+00:00",
	 "dist": {"type": "zip", "url": "https://codeload.example/acme/tool/zip/aaa", "shasum": ""}}
]}}`

func testOptions() Options {
	return Options{
		ProxyURL:    "https://proxy.example",
		Cooldown:    dayCooldown{enabled: true},
		EvaluatedAt: time.Date(2026, 1, 11, 0, 0, 0, 0, time.UTC),
	}
}

// Cooldown is decided at EvaluatedAt and reported; the version that stays
// still carries the fields it inherited from the one left out.
func TestRewrite(t *testing.T) {
	o := testOptions()
	var filtered []string
	o.OnCooldownFilter = func(name, version string, at time.Time) {
		filtered = append(filtered, name+"@"+version+" "+at.UTC().Format(time.RFC3339))
	}
	out, kept, err := Rewrite([]byte(minifiedDocument), o)
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if !slices.Equal(kept, []string{"acme/tool\x001.0.0"}) {
		t.Errorf("kept = %q", kept)
	}
	if !slices.Equal(filtered, []string{"acme/tool@2.0.0 2026-01-10T12:00:00Z"}) {
		t.Errorf("filtered = %v", filtered)
	}
	var doc struct {
		Packages map[string][]map[string]any `json:"packages"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	versions := doc.Packages["acme/tool"]
	if len(versions) != 1 || versions[0]["name"] != "acme/tool" || versions[0]["version"] != "1.0.0" {
		t.Fatalf("versions = %v", versions)
	}
	dist, _ := versions[0]["dist"].(map[string]any)
	if dist["url"] != "https://proxy.example/composer/files/acme/tool/1.0.0/aaa.zip" {
		t.Errorf("dist url = %v", dist["url"])
	}
}

// A disabled or missing policy keeps every version.
func TestRewriteWithoutCooldown(t *testing.T) {
	for _, policy := range []CooldownPolicy{nil, dayCooldown{enabled: false}} {
		o := testOptions()
		o.Cooldown = policy
		if _, kept, err := Rewrite([]byte(minifiedDocument), o); err != nil || len(kept) != 2 {
			t.Errorf("policy %v: kept = %q, %v; want both versions", policy, kept, err)
		}
	}
}

// Pushed versions take the place of upstream ones, and a routed package's
// versions don't report installs to the default repository.
func TestRewriteLocalAndRouted(t *testing.T) {
	o := testOptions()
	o.Cooldown = nil
	o.LocalPackage = "acme/tool"
	o.LocalVersions = []LocalVersion{{Version: "v1.0.0", Entry: json.RawMessage(`{"name": "acme/tool", "version": "1.0.0", "dist": {"type": "zip", "url": "https://proxy.example/composer/files/acme/tool/1.0.0/local.zip"}}`)}}
	o.Routed = func(name string) bool { return name == "acme/tool" }
	out, kept, err := Rewrite([]byte(minifiedDocument), o)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(kept, []string{"acme/tool\x002.0.0", "acme/tool\x00v1.0.0"}) {
		t.Errorf("kept = %q", kept)
	}
	if !strings.Contains(string(out), "local.zip") || strings.Contains(string(out), "zip/aaa") {
		t.Errorf("the pushed 1.0.0 did not replace the upstream one: %s", out)
	}
	if !strings.Contains(string(out), `"notification-url":""`) {
		t.Errorf("a routed package's versions keep the default notification URL: %s", out)
	}

	doc, err := LocalDocument("acme/other", o.LocalVersions)
	if err != nil || !strings.Contains(string(doc), `"acme/other"`) {
		t.Errorf("LocalDocument = %s, %v", doc, err)
	}
}

func TestFindDist(t *testing.T) {
	dist, err := FindDist([]byte(minifiedDocument), "acme/tool", "1.0.0")
	if err != nil || dist.URL != "https://codeload.example/acme/tool/zip/aaa" || dist.Type != "zip" ||
		string(dist.Published) != `"2026-01-01T00:00:00+00:00"` {
		t.Errorf("FindDist = %+v, %v", dist, err)
	}
	if dist, err := FindDist([]byte(minifiedDocument), "acme/tool", "9.9.9"); err != nil || dist.URL != "" {
		t.Errorf("missing version = %+v, %v", dist, err)
	}
}

func TestDistURLs(t *testing.T) {
	if got, ok := ProxyDistURL("https://proxy.example", "acme/tool", "1.0.0", "https://codeload.example/acme/tool/zip/aaa", "zip"); !ok ||
		got != "https://proxy.example/composer/files/acme/tool/1.0.0/aaa.zip" {
		t.Errorf("ProxyDistURL = %q, %v", got, ok)
	}
	if _, ok := ProxyDistURL("https://proxy.example", "tool", "1.0.0", "https://x/y.zip", "zip"); ok {
		t.Error("ProxyDistURL accepted a name without a vendor")
	}
	commit := strings.Repeat("a", 40)
	if !IsCommit(commit) || IsCommit("main") {
		t.Error("IsCommit")
	}
	if c, ok := ShaQueryCommit("archive.zip?sha=" + commit); !ok || c != commit {
		t.Errorf("ShaQueryCommit = %q, %v", c, ok)
	}
	if at, ok := PublishTime([]byte(`"2026-01-01T00:00:00+00:00"`)); !ok || !at.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("PublishTime = %v, %v", at, ok)
	}
	if !PushedVersion([]LocalVersion{{Version: "v2.0.0"}}, "2.0.0") || PushedVersion(nil, "2.0.0") {
		t.Error("PushedVersion")
	}
}
