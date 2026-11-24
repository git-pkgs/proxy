package pub

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

// fixedPolicy holds back versions published after cutoff, as judged at the
// evaluation time it is given.
type fixedPolicy struct {
	enabled   bool
	window    time.Duration
	evaluated []time.Time
	purls     []string
}

func (p *fixedPolicy) Enabled() bool { return p.enabled }

func (p *fixedPolicy) Evaluate(ecosystem, packagePURL string, publishedAt, evaluatedAt time.Time) cooldown.Decision {
	p.evaluated = append(p.evaluated, evaluatedAt)
	p.purls = append(p.purls, ecosystem+" "+packagePURL)
	availableAt := publishedAt.Add(p.window)
	return cooldown.Decision{Allowed: !evaluatedAt.Before(availableAt), AvailableAt: availableAt}
}

var now = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

type doc struct {
	Latest   map[string]any   `json:"latest"`
	Versions []map[string]any `json:"versions"`
}

func decode(t *testing.T, b []byte) doc {
	t.Helper()
	var d doc
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return d
}

func versions(d doc) []string {
	var out []string
	for _, v := range d.Versions {
		out = append(out, v["version"].(string))
	}
	return out
}

const metadata = `{"name":"demo","latest":{"version":"2.0.0","archive_url":"https://pub.dev/up/2.0.0"},"versions":[
{"version":"1.0.0","archive_url":"https://pub.dev/up/1.0.0","published":"2026-01-01T00:00:00Z"},
{"version":"2.0.0","archive_url":"https://pub.dev/up/2.0.0","published":"2026-11-20T11:00:00Z"},
{"version":"","archive_url":"https://pub.dev/up/empty"},
"not an object"]}`

func TestRewritePointsArchivesAtTheProxy(t *testing.T) {
	out, err := Rewrite("demo", []byte(metadata), Options{ProxyURL: "https://proxy.example"})
	if err != nil {
		t.Fatal(err)
	}
	d := decode(t, out)
	if got := versions(d); len(got) != 2 || got[0] != "1.0.0" || got[1] != "2.0.0" {
		t.Fatalf("versions = %v", got)
	}
	for _, v := range d.Versions {
		want := "https://proxy.example/pub/packages/demo/versions/" + v["version"].(string) + ".tar.gz"
		if v["archive_url"] != want {
			t.Errorf("archive_url = %v, want %s", v["archive_url"], want)
		}
	}
	if d.Latest["archive_url"] != "https://proxy.example/pub/packages/demo/versions/2.0.0.tar.gz" {
		t.Errorf("latest = %v", d.Latest)
	}
}

func TestRewriteEvaluatesCooldownAtEvaluatedAt(t *testing.T) {
	policy := &fixedPolicy{enabled: true, window: 3 * time.Hour}
	var filtered []string
	var filteredAt []time.Time
	out, err := Rewrite("demo", []byte(metadata), Options{
		ProxyURL:    "https://proxy.example",
		Cooldown:    policy,
		EvaluatedAt: now,
		OnCooldownFilter: func(name, version string, publishedAt time.Time) {
			filtered = append(filtered, name+"@"+version)
			filteredAt = append(filteredAt, publishedAt)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	d := decode(t, out)
	if got := versions(d); len(got) != 1 || got[0] != "1.0.0" {
		t.Fatalf("versions = %v", got)
	}
	if d.Latest["version"] != "1.0.0" {
		t.Errorf("latest = %v, want 1.0.0", d.Latest)
	}
	if len(filtered) != 1 || filtered[0] != "demo@2.0.0" || !filteredAt[0].Equal(time.Date(2026, 11, 20, 11, 0, 0, 0, time.UTC)) {
		t.Errorf("filtered = %v at %v", filtered, filteredAt)
	}
	for _, at := range policy.evaluated {
		if !at.Equal(now) {
			t.Errorf("evaluated at %v, want %v", at, now)
		}
	}
	if len(policy.purls) == 0 || policy.purls[0] != "pub pkg:pub/demo" {
		t.Errorf("purls = %v", policy.purls)
	}

	// Later, 2.0.0 has left cooldown.
	out, err = Rewrite("demo", []byte(metadata), Options{Cooldown: policy, EvaluatedAt: now.Add(2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if got := versions(decode(t, out)); len(got) != 2 {
		t.Errorf("versions later = %v", got)
	}
}

func TestRewriteKeepsVersionsWithoutUsablePublishTime(t *testing.T) {
	policy := &fixedPolicy{enabled: true, window: 1000 * time.Hour}
	body := `{"versions":[{"version":"1.0.0"},{"version":"1.1.0","published":"yesterday"},{"version":"1.2.0","published":7}]}`
	out, err := Rewrite("demo", []byte(body), Options{Cooldown: policy, EvaluatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if got := versions(decode(t, out)); len(got) != 3 {
		t.Errorf("versions = %v", got)
	}
	if len(policy.evaluated) != 0 {
		t.Errorf("evaluated %d times", len(policy.evaluated))
	}
}

func TestRewriteWithoutAnEnabledPolicy(t *testing.T) {
	for name, policy := range map[string]CooldownPolicy{"nil": nil, "disabled": &fixedPolicy{window: 1000 * time.Hour}} {
		t.Run(name, func(t *testing.T) {
			out, err := Rewrite("demo", []byte(metadata), Options{Cooldown: policy, EvaluatedAt: now})
			if err != nil {
				t.Fatal(err)
			}
			if got := versions(decode(t, out)); len(got) != 2 {
				t.Errorf("versions = %v", got)
			}
		})
	}
}

func TestRewriteLatest(t *testing.T) {
	policy := &fixedPolicy{enabled: true, window: 3 * time.Hour}
	old, recent := `"2026-01-01T00:00:00Z"`, `"2026-11-20T11:00:00Z"`
	entry := func(v, published string) string {
		return `{"version":"` + v + `","published":` + published + `}`
	}
	tests := []struct {
		name, versions, latest string
		want                   any
	}{
		{"newest stable left", entry("1.0.0", old) + "," + entry("1.5.0", old) + "," + entry("2.0.0-beta", old) + "," + entry("2.0.0", recent), "2.0.0", "1.5.0"},
		{"only prereleases left", entry("1.0.0-a", old) + "," + entry("1.0.0-b", old) + "," + entry("2.0.0", recent), "2.0.0", "1.0.0-b"},
		{"none left", entry("2.0.0", recent), "2.0.0", nil},
		{"unparseable skipped", entry("x.y", old) + "," + entry("1.0.0", old) + "," + entry("2.0.0", recent), "2.0.0", "1.0.0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"latest":{"version":"` + tc.latest + `"},"versions":[` + tc.versions + `]}`
			out, err := Rewrite("demo", []byte(body), Options{Cooldown: policy, EvaluatedAt: now})
			if err != nil {
				t.Fatal(err)
			}
			d := decode(t, out)
			if tc.want == nil {
				if d.Latest != nil {
					t.Errorf("latest = %v, want none", d.Latest)
				}
				return
			}
			if d.Latest["version"] != tc.want {
				t.Errorf("latest = %v, want %v", d.Latest["version"], tc.want)
			}
		})
	}
}

func TestRewriteWithoutVersionsOrLatest(t *testing.T) {
	body := []byte(`{"name":"demo"}`)
	out, err := Rewrite("demo", body, Options{})
	if err != nil || string(out) != string(body) {
		t.Errorf("Rewrite = %s, %v; want the body unchanged", out, err)
	}
	out, err = Rewrite("demo", []byte(`{"versions":[{"version":"1.0.0"}]}`), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := decode(t, out).Latest["version"]; ok {
		t.Errorf("latest added: %s", out)
	}
	if _, err := Rewrite("demo", []byte(`{`), Options{}); err == nil {
		t.Error("invalid JSON accepted")
	}
}

func TestArchiveURL(t *testing.T) {
	if got := ArchiveURL("https://proxy.example", "demo", "1.0.0"); got != "https://proxy.example/pub/packages/demo/versions/1.0.0.tar.gz" {
		t.Errorf("ArchiveURL = %s", got)
	}
}
