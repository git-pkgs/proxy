package maven

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

var now = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// windowPolicy holds back versions published within window of evaluatedAt.
type windowPolicy struct {
	window   time.Duration
	disabled bool
	purls    []string
	at       []time.Time
}

func (p *windowPolicy) Enabled() bool { return !p.disabled }

func (p *windowPolicy) Evaluate(eco, packagePURL string, publishedAt, evaluatedAt time.Time) cooldown.Decision {
	if eco != "maven" {
		panic("ecosystem " + eco)
	}
	p.purls = append(p.purls, packagePURL)
	p.at = append(p.at, evaluatedAt)
	available := publishedAt.Add(p.window)
	return cooldown.Decision{Allowed: !evaluatedAt.Before(available), AvailableAt: available}
}

func metadataDoc(lastUpdated time.Time, latest, release string, versions ...string) string {
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<metadata>\n  <groupId>com.example</groupId>\n  <artifactId>lib</artifactId>\n  <versioning>\n")
	b.WriteString("    <latest>" + latest + "</latest>\n    <release>" + release + "</release>\n    <versions>\n")
	for _, v := range versions {
		b.WriteString("      <version>" + v + "</version>\n")
	}
	b.WriteString("    </versions>\n    <lastUpdated>" + lastUpdated.UTC().Format(LastUpdatedLayout) + "</lastUpdated>\n  </versioning>\n</metadata>\n")
	return b.String()
}

func versionsOf(t *testing.T, body []byte) (versions []string, latest, release string) {
	t.Helper()
	doc, err := ParseMetadata(body)
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, body)
	}
	for _, v := range doc.Versions {
		versions = append(versions, v.Value)
	}
	if doc.Latest != nil {
		latest = doc.Latest.Value
	}
	if doc.Release != nil {
		release = doc.Release.Value
	}
	return versions, latest, release
}

type timeSource struct {
	times  map[string]time.Time
	asked  []string
	errFor string
}

func (s *timeSource) lookup(version string) (time.Time, error) {
	s.asked = append(s.asked, version)
	if version == s.errFor {
		return time.Time{}, errors.New("rate limited")
	}
	return s.times[version], nil
}

func TestFilterRemovesNewestVersionsInCooldown(t *testing.T) {
	src := &timeSource{times: map[string]time.Time{
		"1.0": now.Add(-30 * 24 * time.Hour),
		"1.1": now.Add(-10 * 24 * time.Hour),
		"1.2": now.Add(-2 * 24 * time.Hour),
		"1.3": now.Add(-time.Hour),
	}}
	policy := &windowPolicy{window: 3 * 24 * time.Hour}
	var withheld []string
	body := metadataDoc(now.Add(-time.Hour), "1.3", "1.3", "1.0", "1.1", "1.2", "1.3")
	out, err := Filter("com/example/lib", []byte(body), Options{
		Cooldown:    policy,
		EvaluatedAt: now,
		PublishTime: src.lookup,
		OnWithheld: func(version string, published time.Time) {
			if !published.Equal(src.times[version]) {
				t.Errorf("OnWithheld(%s) published = %v", version, published)
			}
			withheld = append(withheld, version)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	versions, latest, release := versionsOf(t, out)
	if !slices.Equal(versions, []string{"1.0", "1.1"}) || latest != "1.1" || release != "1.1" {
		t.Errorf("versions %v latest %q release %q", versions, latest, release)
	}
	if !slices.Equal(withheld, []string{"1.3", "1.2"}) {
		t.Errorf("withheld %v", withheld)
	}
	// Lookups stop at the first version old enough.
	if !slices.Equal(src.asked, []string{"1.3", "1.2", "1.1"}) {
		t.Errorf("asked %v", src.asked)
	}
	for _, purl := range policy.purls {
		if purl != "pkg:maven/com.example/lib" {
			t.Errorf("evaluated for %q", purl)
		}
	}
	for _, at := range policy.at {
		if !at.Equal(now) {
			t.Errorf("evaluated at %v, want EvaluatedAt", at)
		}
	}
	if strings.Contains(string(out), "<version>1.2</version>") || !strings.Contains(string(out), "<groupId>com.example</groupId>") {
		t.Errorf("unexpected output:\n%s", out)
	}
}

func TestFilterSkipsLookupsWhenLastUpdatedIsOld(t *testing.T) {
	src := &timeSource{}
	body := metadataDoc(now.Add(-10*24*time.Hour), "1.1", "1.1", "1.0", "1.1")
	out, err := Filter("com/example/lib", []byte(body), Options{
		Cooldown: &windowPolicy{window: 3 * 24 * time.Hour}, EvaluatedAt: now, PublishTime: src.lookup,
	})
	if err != nil || string(out) != body {
		t.Fatalf("got %q, %v", out, err)
	}
	if len(src.asked) != 0 {
		t.Errorf("asked %v", src.asked)
	}
}

func TestFilterReadsTimesWhenLastUpdatedIsUnreadable(t *testing.T) {
	src := &timeSource{times: map[string]time.Time{"1.1": now}}
	body := strings.Replace(metadataDoc(now, "1.1", "1.1", "1.0", "1.1"), now.Format(LastUpdatedLayout), "soon", 1)
	out, err := Filter("com/example/lib", []byte(body), Options{
		Cooldown: &windowPolicy{window: time.Hour}, EvaluatedAt: now, PublishTime: src.lookup,
	})
	if err != nil {
		t.Fatal(err)
	}
	if versions, _, _ := versionsOf(t, out); !slices.Equal(versions, []string{"1.0"}) {
		t.Errorf("versions %v", versions)
	}
}

func TestFilterLeavesSnapshotsAndUnknownTimes(t *testing.T) {
	src := &timeSource{times: map[string]time.Time{
		"1.0": now.Add(-time.Hour),
		"2.0": now.Add(-time.Hour),
	}}
	body := metadataDoc(now, "2.1-SNAPSHOT", "2.0", "1.0", "1.1", "2.0", "2.1-SNAPSHOT")
	out, err := Filter("com/example/lib", []byte(body), Options{
		Cooldown: &windowPolicy{window: 24 * time.Hour}, EvaluatedAt: now, PublishTime: src.lookup,
	})
	if err != nil {
		t.Fatal(err)
	}
	versions, latest, release := versionsOf(t, out)
	if !slices.Equal(versions, []string{"1.1", "2.1-SNAPSHOT"}) {
		t.Errorf("versions %v", versions)
	}
	if latest != "2.1-SNAPSHOT" || release != "1.1" {
		t.Errorf("latest %q release %q", latest, release)
	}
	// 1.1 has no time and is let through without ending the walk.
	if !slices.Equal(src.asked, []string{"2.0", "1.1", "1.0"}) {
		t.Errorf("asked %v", src.asked)
	}
}

func TestFilterLookupError(t *testing.T) {
	src := &timeSource{errFor: "1.1"}
	body := metadataDoc(now, "1.1", "1.1", "1.0", "1.1")
	_, err := Filter("com/example/lib", []byte(body), Options{
		Cooldown: &windowPolicy{window: time.Hour}, EvaluatedAt: now, PublishTime: src.lookup,
	})
	if err == nil || !strings.Contains(err.Error(), "reading time of 1.1: rate limited") {
		t.Fatalf("err = %v", err)
	}
}

func TestFilterUnchanged(t *testing.T) {
	recent := metadataDoc(now, "1.0", "1.0", "1.0")
	allNew := func(string) (time.Time, error) { return now, nil }
	plugins := `<metadata><plugins><plugin><prefix>x</prefix></plugin></plugins></metadata>`
	for _, tc := range []struct {
		name, dir, body string
		o               Options
	}{
		{"no policy", "com/example/lib", recent, Options{EvaluatedAt: now, PublishTime: allNew}},
		{"disabled policy", "com/example/lib", recent, Options{Cooldown: &windowPolicy{window: time.Hour, disabled: true}, EvaluatedAt: now, PublishTime: allNew}},
		{"not an artifact directory", "lib", recent, Options{Cooldown: &windowPolicy{window: time.Hour}, EvaluatedAt: now, PublishTime: allNew}},
		{"trailing slash", "com/example/", recent, Options{Cooldown: &windowPolicy{window: time.Hour}, EvaluatedAt: now, PublishTime: allNew}},
		{"no version list", "com/example", plugins, Options{Cooldown: &windowPolicy{window: time.Hour}, EvaluatedAt: now, PublishTime: allNew}},
		{"no time source", "com/example/lib", recent, Options{Cooldown: &windowPolicy{window: time.Hour}, EvaluatedAt: now}},
		{"all old enough", "com/example/lib", recent, Options{Cooldown: &windowPolicy{window: time.Hour}, EvaluatedAt: now.Add(2 * time.Hour), PublishTime: allNew}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Filter(tc.dir, []byte(tc.body), tc.o)
			if err != nil || string(out) != tc.body {
				t.Errorf("got %q, %v", out, err)
			}
		})
	}
}

func TestFilterRefusesMalformedMetadata(t *testing.T) {
	if _, err := Filter("com/example/lib", []byte("<metadata><versioning>"), Options{}); err == nil {
		t.Error("want an error")
	}
	if _, err := ParseMetadata([]byte("<metadata></versioning>")); err == nil {
		t.Error("want an error")
	}
}

func TestWithoutDropsLatestAndReleaseWhenNothingIsLeft(t *testing.T) {
	in := "<metadata>\r\n<versioning>\r\n\t<latest>1</latest>\r\n\t<release>1</release>\r\n\t<versions>\r\n\t\t<version>1</version>\r\n\t</versions>\r\n</versioning>\r\n</metadata>"
	doc, err := ParseMetadata([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	got := string(doc.Without([]byte(in), map[string]bool{"1": true}))
	want := "<metadata>\r\n<versioning>\r\n\t<versions>\r\n\t</versions>\r\n</versioning>\r\n</metadata>"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestWithoutKeepsLatestAndReleaseNotRemoved(t *testing.T) {
	in := `<metadata><versioning><latest>3</latest><release>2</release><versions><version>1</version><version>2</version><version>3</version><version>4</version></versions></versioning></metadata>`
	doc, err := ParseMetadata([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	got := string(doc.Without([]byte(in), map[string]bool{"4": true}))
	want := `<metadata><versioning><latest>3</latest><release>2</release><versions><version>1</version><version>2</version><version>3</version></versions></versioning></metadata>`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestParseMetadataIgnoresElementsElsewhere(t *testing.T) {
	in := `<metadata><version>9</version><versioning><versions><version> 1 </version></versions><lastUpdated>20260101000000</lastUpdated></versioning><plugins><plugin><latest>x</latest></plugin></plugins></metadata>`
	doc, err := ParseMetadata([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Versions) != 1 || doc.Versions[0].Value != "1" || doc.Latest != nil || doc.LastUpdated != "20260101000000" {
		t.Errorf("doc = %+v", doc)
	}
	if got := in[doc.Versions[0].Start:doc.Versions[0].End]; got != "<version> 1 </version>" {
		t.Errorf("offsets cover %q", got)
	}
}

func TestParseMetadataReadsAnyCharset(t *testing.T) {
	in := `<?xml version="1.0" encoding="ISO-8859-1"?><metadata><versioning><versions><version>1</version></versions></versioning></metadata>`
	doc, err := ParseMetadata([]byte(in))
	if err != nil || len(doc.Versions) != 1 {
		t.Fatalf("doc %+v, err %v", doc, err)
	}
}

func TestLineStart(t *testing.T) {
	for _, tc := range []struct {
		body  string
		start int
		want  int
	}{
		{"a\n  <v>", 4, 1},
		{"a\r\n\t<v>", 4, 1},
		{"a <v>", 2, 2},
		{"  <v>", 2, 2},
		{"\n<v>", 1, 0},
	} {
		if got := lineStart([]byte(tc.body), tc.start); got != tc.want {
			t.Errorf("lineStart(%q, %d) = %d, want %d", tc.body, tc.start, got, tc.want)
		}
	}
}

func TestCoordinates(t *testing.T) {
	for _, tc := range []struct {
		dir, group, artifact string
		ok                   bool
	}{
		{"com/google/guava/guava", "com.google.guava", "guava", true},
		{"junit/junit", "junit", "junit", true},
		{"guava", "", "", false},
		{"/guava", "", "", false},
		{"com/google/", "", "", false},
	} {
		group, artifact, ok := Coordinates(tc.dir)
		if group != tc.group || artifact != tc.artifact || ok != tc.ok {
			t.Errorf("Coordinates(%q) = %q, %q, %v", tc.dir, group, artifact, ok)
		}
	}
	if got := PackagePURL("com.google.guava", "guava"); got != "pkg:maven/com.google.guava/guava" {
		t.Errorf("PackagePURL = %q", got)
	}
}

func TestUsableVersion(t *testing.T) {
	for v, want := range map[string]bool{
		"1.0": true, "1.0-SNAPSHOT": true, "": false, ".": false, "..": false,
		"1/2": false, `1\2`: false, "1?": false, "1#": false, "1%2F": false,
	} {
		if got := UsableVersion(v); got != want {
			t.Errorf("UsableVersion(%q) = %v", v, got)
		}
	}
}
