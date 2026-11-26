package gem

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

const index = "---\n" +
	"1 |checksum:zzz\n" +
	"1.0.0 |checksum:aaa\n" +
	"1.1.0 dep:>= 1|checksum:bbb\n" +
	"\n" +
	"2.0.0-x86_64-linux |checksum:ccc\n" +
	"2.0.0"

func TestIndexVersions(t *testing.T) {
	got := IndexVersions([]byte(index))
	want := []string{"1", "1.0.0", "1.1.0", "2.0.0-x86_64-linux", "2.0.0"}
	if !slices.Equal(got, want) {
		t.Errorf("IndexVersions = %q, want %q", got, want)
	}
	if got := IndexVersions(nil); got != nil {
		t.Errorf("IndexVersions(nil) = %q, want nil", got)
	}
}

// windowPolicy holds a version back until window has passed since it was
// published, as judged at the evaluation time it is given.
type windowPolicy struct {
	disabled  bool
	window    time.Duration
	evaluated []time.Time
	purls     []string
}

func (p *windowPolicy) Enabled() bool { return !p.disabled }

func (p *windowPolicy) Evaluate(ecosystem, packagePURL string, publishedAt, evaluatedAt time.Time) cooldown.Decision {
	p.evaluated = append(p.evaluated, evaluatedAt)
	p.purls = append(p.purls, ecosystem+" "+packagePURL)
	availableAt := publishedAt.Add(p.window)
	return cooldown.Decision{Allowed: !evaluatedAt.Before(availableAt), AvailableAt: availableAt}
}

var now = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// published gives every version in index a time: 1.1.0 and the platform
// build of 2.0.0 an hour ago, the others a month ago.
func published() map[string]time.Time {
	old, recent := now.AddDate(0, -1, 0), now.Add(-time.Hour)
	return map[string]time.Time{
		"1":                  old,
		"1.0.0":              old,
		"1.1.0":              recent,
		"2.0.0-x86_64-linux": recent,
		"2.0.0":              old,
	}
}

type withheldVersion struct {
	name, version string
	publishedAt   time.Time
}

func TestFilterIndex(t *testing.T) {
	policy := &windowPolicy{window: 72 * time.Hour}
	var reported []withheldVersion
	out, kept := FilterIndex("demo", []byte(index), Options{
		Cooldown:    policy,
		EvaluatedAt: now,
		Published:   published(),
		OnWithheld: func(name, version string, publishedAt time.Time) {
			reported = append(reported, withheldVersion{name, version, publishedAt})
		},
	})

	wantOut := "---\n1 |checksum:zzz\n1.0.0 |checksum:aaa\n\n2.0.0\n"
	if string(out) != wantOut {
		t.Errorf("FilterIndex output = %q, want %q", out, wantOut)
	}
	if want := []string{"1", "1.0.0", "2.0.0"}; !slices.Equal(kept, want) {
		t.Errorf("kept = %q, want %q", kept, want)
	}
	recent := now.Add(-time.Hour)
	want := []withheldVersion{{"demo", "1.1.0", recent}, {"demo", "2.0.0-x86_64-linux", recent}}
	if !slices.Equal(reported, want) {
		t.Errorf("OnWithheld got %v, want %v", reported, want)
	}
	if len(policy.purls) != 5 || slices.ContainsFunc(policy.purls, func(p string) bool { return p != "gem pkg:gem/demo" }) {
		t.Errorf("policy asked about %q, want gem pkg:gem/demo for each of the 5 versions", policy.purls)
	}
	if slices.ContainsFunc(policy.evaluated, func(at time.Time) bool { return !at.Equal(now) }) {
		t.Errorf("policy evaluated at %v, want only %v", policy.evaluated, now)
	}
}

func TestFilterIndexWithholdsNothing(t *testing.T) {
	tests := map[string]Options{
		"no policy":       {Published: published(), WithholdUnknown: true},
		"policy disabled": {Cooldown: &windowPolicy{disabled: true, window: 72 * time.Hour}, EvaluatedAt: now, WithholdUnknown: true},
		"all old enough":  {Cooldown: &windowPolicy{window: 30 * time.Minute}, EvaluatedAt: now, Published: published()},
	}
	for name, o := range tests {
		t.Run(name, func(t *testing.T) {
			o.OnWithheld = func(_, version string, _ time.Time) { t.Errorf("withheld %s", version) }
			out, kept := FilterIndex("demo", []byte(index), o)
			if string(out) != index+"\n" {
				t.Errorf("FilterIndex output = %q, want the index with a final newline", out)
			}
			if len(kept) != 5 {
				t.Errorf("kept = %q, want 5 versions", kept)
			}
		})
	}
}

// A version the times don't have is kept, unless the caller says its times
// are too old to trust: then it is held back, with no publish time to
// report.
func TestFilterIndexVersionWithoutTime(t *testing.T) {
	times := published()
	delete(times, "1.0.0")
	o := Options{Cooldown: &windowPolicy{window: 30 * time.Minute}, EvaluatedAt: now, Published: times}

	if _, kept := FilterIndex("demo", []byte(index), o); !slices.Contains(kept, "1.0.0") {
		t.Errorf("kept = %q, want 1.0.0 in it", kept)
	}

	var reported []withheldVersion
	o.WithholdUnknown = true
	o.OnWithheld = func(name, version string, publishedAt time.Time) {
		reported = append(reported, withheldVersion{name, version, publishedAt})
	}
	out, kept := FilterIndex("demo", []byte(index), o)
	if slices.Contains(kept, "1.0.0") || strings.Contains(string(out), "1.0.0 ") {
		t.Errorf("kept = %q, output = %q; want 1.0.0 held back", kept, out)
	}
	if want := []withheldVersion{{"demo", "1.0.0", time.Time{}}}; !slices.Equal(reported, want) {
		t.Errorf("OnWithheld got %v, want %v", reported, want)
	}
}

// A version whose time didn't parse is kept, whatever WithholdUnknown says.
func TestFilterIndexKeepsVersionWithZeroTime(t *testing.T) {
	times := published()
	times["1.1.0"] = time.Time{}
	policy := &windowPolicy{window: 72 * time.Hour}
	_, kept := FilterIndex("demo", []byte(index), Options{
		Cooldown: policy, EvaluatedAt: now, Published: times, WithholdUnknown: true,
	})
	if want := []string{"1", "1.0.0", "1.1.0", "2.0.0"}; !slices.Equal(kept, want) {
		t.Errorf("kept = %q, want %q", kept, want)
	}
	if len(policy.evaluated) != 4 {
		t.Errorf("policy evaluated %d versions, want 4: not the one without a time", len(policy.evaluated))
	}
}

func TestFilterIndexWithoutOnWithheld(t *testing.T) {
	out, kept := FilterIndex("demo", []byte("1.0.0 |x\n"), Options{
		Cooldown:    &windowPolicy{window: 72 * time.Hour},
		EvaluatedAt: now,
		Published:   map[string]time.Time{"1.0.0": now},
	})
	if string(out) != "" || kept != nil {
		t.Errorf("FilterIndex = %q, %q; want nothing kept", out, kept)
	}
}

func TestPublishTimes(t *testing.T) {
	body := `[
		{"number":"1.0.0","platform":"ruby","created_at":"2024-01-02T03:04:05Z"},
		{"number":"2.0.0","platform":"x86_64-linux","created_at":"2024-02-03T04:05:06.000Z"},
		{"number":"3.0.0","platform":"","created_at":"not a time"}
	]`
	times, err := PublishTimes([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]time.Time{
		"1.0.0":              time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC),
		"2.0.0-x86_64-linux": time.Date(2024, 2, 3, 4, 5, 6, 0, time.UTC),
		"3.0.0":              {},
	}
	if len(times) != len(want) {
		t.Fatalf("PublishTimes = %v, want %v", times, want)
	}
	for v, w := range want {
		if got, ok := times[v]; !ok || !got.Equal(w) {
			t.Errorf("PublishTimes[%q] = %v (%v), want %v", v, got, ok, w)
		}
	}
	if _, err := PublishTimes([]byte("{")); err == nil {
		t.Error("PublishTimes accepted invalid JSON")
	}
}

func TestFilenameIdentity(t *testing.T) {
	tests := []struct{ filename, name, version string }{
		{"rails-7.1.0.gem", "rails", "7.1.0"},
		{"aws-sdk-s3-1.142.0.gem", "aws-sdk-s3", "1.142.0"},
		{"sorbet-static-0.5.11002-universal-darwin-22.gem", "sorbet-static", "0.5.11002-universal-darwin-22"},
		{"devise-2fa-1.0.0.gem", "devise-2fa", "1.0.0"},
		{"jruby-launcher-1.1.19-java.gem", "jruby-launcher", "1.1.19-java"},
		{"nokogiri-1.16.0-universal-java-17.gem", "nokogiri", "1.16.0-universal-java-17"},
		{"foo-1.0.0.pre.1.gem", "foo", "1.0.0.pre.1"},
		{"odd-1-+.gem", "odd", "1-+"},
		{"a-1-2-+.gem", "a", "1-2-+"},
		{"noversion.gem", "", ""},
		{"x" + strings.Repeat("-1", 300) + ".gem", "", ""},
	}
	for _, tt := range tests {
		name, version := FilenameIdentity(tt.filename)
		if name != tt.name || version != tt.version {
			t.Errorf("FilenameIdentity(%q) = (%q, %q), want (%q, %q)", tt.filename, name, version, tt.name, tt.version)
		}
	}
}

func TestFilenameIdentityLengthBound(t *testing.T) {
	name := strings.Repeat("a", maxFilenameLength-len("-1.0.gem"))
	if n, v := FilenameIdentity(name + "-1.0.gem"); n != name || v != "1.0" {
		t.Errorf("FilenameIdentity at the bound = (%q, %q)", n, v)
	}
	if n, v := FilenameIdentity(name + "a-1.0.gem"); n != "" || v != "" {
		t.Errorf("FilenameIdentity past the bound = (%q, %q), want empty", n, v)
	}
}
