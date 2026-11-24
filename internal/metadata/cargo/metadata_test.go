package cargo

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

var evaluatedAt = time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)

// policy holds back versions published less than three days before the
// time it is evaluated at, and records the PURLs it is asked about.
type policy struct {
	disabled bool
	purls    []string
	at       []time.Time
}

func (p *policy) Enabled() bool { return !p.disabled }

func (p *policy) Evaluate(ecosystem, packagePURL string, publishedAt, at time.Time) cooldown.Decision {
	p.purls = append(p.purls, ecosystem+" "+packagePURL)
	p.at = append(p.at, at)
	return cooldown.Decision{Allowed: !publishedAt.After(at.Add(-72 * time.Hour))}
}

func line(name, version, pubtime string) string {
	s := `{"name":"` + name + `","vers":"` + version + `","cksum":"ab"`
	if pubtime != "" {
		s += `,"pubtime":"` + pubtime + `"`
	}
	return s + `}`
}

func TestFilterWithholdsByPubtimeAtEvaluatedAt(t *testing.T) {
	old := evaluatedAt.Add(-96 * time.Hour).Format(time.RFC3339)
	recent := evaluatedAt.Add(-time.Hour).Format(time.RFC3339)
	body := line("serde", "1.0.0", old) + "\n" +
		line("serde", "1.0.1", recent) + "\n" +
		line("serde", "0.9.0", "") + "\n" +
		line("serde", "0.9.1", "not a time")
	p := &policy{}
	var withheld []string
	out, err := Filter([]byte(body), Options{
		Cooldown: p, EvaluatedAt: evaluatedAt,
		OnWithheld: func(name, version string, publishedAt time.Time) {
			withheld = append(withheld, name+"@"+version+" "+publishedAt.Format(time.RFC3339))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := line("serde", "1.0.0", old) + "\n" + line("serde", "0.9.0", "") + "\n" + line("serde", "0.9.1", "not a time") + "\n"
	if string(out) != want {
		t.Errorf("Filter =\n%s\nwant\n%s", out, want)
	}
	if !reflect.DeepEqual(withheld, []string{"serde@1.0.1 " + recent}) {
		t.Errorf("withheld = %v", withheld)
	}
	if !reflect.DeepEqual(p.purls, []string{"cargo pkg:cargo/serde", "cargo pkg:cargo/serde"}) {
		t.Errorf("evaluated %v", p.purls)
	}
	for _, at := range p.at {
		if !at.Equal(evaluatedAt) {
			t.Errorf("evaluated at %v, want %v", at, evaluatedAt)
		}
	}
}

func TestFilterDenylist(t *testing.T) {
	body := line("demo", "1.0.0", "") + "\n" + line("demo", "2.0.0", "") + "\n"
	for _, p := range []CooldownPolicy{nil, &policy{disabled: true}, &policy{}} {
		out, err := Filter([]byte(body), Options{
			Cooldown: p, EvaluatedAt: evaluatedAt,
			Denied: func(name, version string) bool { return name == "demo" && version == "1.0.0" },
		})
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != line("demo", "2.0.0", "")+"\n" {
			t.Errorf("Filter = %q", out)
		}
	}
}

func TestFilterDisabledCooldownKeepsRecentVersions(t *testing.T) {
	recent := evaluatedAt.Add(-time.Hour).Format(time.RFC3339)
	body := line("serde", "1.0.1", recent) + "\n"
	p := &policy{disabled: true}
	out, err := Filter([]byte(body), Options{Cooldown: p, EvaluatedAt: evaluatedAt, Denied: func(string, string) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != body {
		t.Errorf("Filter = %q", out)
	}
	if len(p.purls) != 0 {
		t.Errorf("disabled policy evaluated %v", p.purls)
	}
}

func TestFilterWithoutPolicyReturnsBodyAsIs(t *testing.T) {
	body := []byte("not json\n\n" + line("serde", "1.0.0", ""))
	for _, o := range []Options{{}, {Cooldown: &policy{disabled: true}}} {
		if o.Filters() {
			t.Errorf("%+v filters", o)
		}
		out, err := Filter(body, o)
		if err != nil || string(out) != string(body) {
			t.Errorf("Filter = %q, %v", out, err)
		}
	}
	if !(Options{Cooldown: &policy{}}).Filters() || !(Options{Denied: func(string, string) bool { return false }}).Filters() {
		t.Error("an enabled cooldown or a denylist should filter")
	}
}

func TestFilterLeavesOutInvalidEntries(t *testing.T) {
	body := "not json\n\n" + line("serde", "1.0.0", "")
	var invalid int
	out, err := Filter([]byte(body), Options{
		Cooldown:       &policy{},
		EvaluatedAt:    evaluatedAt,
		OnInvalidEntry: func(error) { invalid++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != line("serde", "1.0.0", "")+"\n" {
		t.Errorf("Filter = %q", out)
	}
	if invalid != 2 {
		t.Errorf("invalid entries reported = %d, want 2", invalid)
	}
	// Without the callback, invalid lines are still left out.
	if out, _ := Filter([]byte(body), Options{Cooldown: &policy{}, EvaluatedAt: evaluatedAt}); string(out) != line("serde", "1.0.0", "")+"\n" {
		t.Errorf("Filter = %q", out)
	}
}

func TestFilterKeepsLongEntries(t *testing.T) {
	entry := `{"name":"demo","vers":"2.0.0","extra":"` + strings.Repeat("x", 70<<10) + `"}` + "\n"
	out, err := Filter([]byte(entry), Options{Cooldown: &policy{}, EvaluatedAt: evaluatedAt})
	if err != nil || string(out) != entry {
		t.Fatalf("long entry: err %v, %d bytes", err, len(out))
	}
}

func TestPublishTime(t *testing.T) {
	pub := "2026-01-02T03:04:05Z"
	body := []byte(line("serde", "1.0.0", pub) + "\n\n" + line("serde", "0.9.0", "") + "\n")
	published, ok, listed, err := PublishTime(body, "1.0.0")
	if err != nil || !ok || !listed || published.Format(time.RFC3339) != pub {
		t.Errorf("1.0.0: %v %v %v %v", published, ok, listed, err)
	}
	_, ok, listed, err = PublishTime(body, "0.9.0")
	if err != nil || ok || !listed {
		t.Errorf("0.9.0 without pubtime: ok %v listed %v err %v", ok, listed, err)
	}
	_, ok, listed, err = PublishTime(body, "2.0.0")
	if err != nil || ok || listed {
		t.Errorf("2.0.0 unlisted: ok %v listed %v err %v", ok, listed, err)
	}
	_, _, listed, err = PublishTime([]byte("not json\n"), "1.0.0")
	if err == nil || listed {
		t.Errorf("invalid line: listed %v err %v", listed, err)
	}
}

func TestIndexPath(t *testing.T) {
	for name, want := range map[string]string{
		"a":     "1/a",
		"ab":    "2/ab",
		"abc":   "3/a/abc",
		"abcd":  "ab/cd/abcd",
		"Serde": "se/rd/serde",
	} {
		if got := IndexPath(name); got != want {
			t.Errorf("IndexPath(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestDownloadTemplate(t *testing.T) {
	if got := DownloadTemplate("http://proxy"); got != "http://proxy/cargo/crates/{crate}/{version}/download" {
		t.Errorf("DownloadTemplate = %q", got)
	}
}
