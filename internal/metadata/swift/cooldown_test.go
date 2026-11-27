package swift

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

const testPackagePURL = "pkg:swift/apple/example"

var evaluatedAt = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

func testCooldown() *cooldown.Config { return &cooldown.Config{Default: "3d"} }

func TestPublishedAt(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       time.Time
		wantErr    bool
	}{
		{"set", `{"version":"1.0.0","publishedAt":"2026-11-20T08:30:00Z"}`, time.Date(2026, 11, 20, 8, 30, 0, 0, time.UTC), false},
		{"fractional seconds", `{"publishedAt":"2026-11-20T08:30:00.250+01:00"}`, time.Date(2026, 11, 20, 7, 30, 0, 250e6, time.UTC), false},
		{"missing", `{"version":"1.0.0","metadata":{"originalPublicationTime":"2026-11-20T08:30:00Z"}}`, time.Time{}, false},
		{"empty", `{"publishedAt":""}`, time.Time{}, false},
		{"not a time", `{"publishedAt":"yesterday"}`, time.Time{}, true},
		{"not JSON", `nope`, time.Time{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PublishedAt([]byte(tc.body))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !got.Equal(tc.want) {
				t.Errorf("PublishedAt = %v, want %v", got, tc.want)
			}
		})
	}
}

func releaseList(versions ...string) []byte {
	releases := map[string]any{}
	for _, v := range versions {
		releases[v] = map[string]any{"url": "https://registry.example/apple/example/" + v}
	}
	body, _ := json.Marshal(map[string]any{"releases": releases})
	return body
}

func listedVersions(t *testing.T, body []byte) []string {
	t.Helper()
	var doc struct {
		Releases map[string]json.RawMessage `json:"releases"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	var versions []string
	for v := range doc.Releases {
		versions = append(versions, v)
	}
	slices.Sort(versions)
	return versions
}

// timesFrom returns a PublishTime looking versions up in ages and recording
// the order it was asked in.
func timesFrom(ages map[string]time.Duration, asked *[]string) func(string) (time.Time, error) {
	return func(v string) (time.Time, error) {
		*asked = append(*asked, v)
		age, ok := ages[v]
		if !ok {
			return time.Time{}, nil
		}
		return evaluatedAt.Add(-age), nil
	}
}

func TestFilterReleasesWithholdsReleasesInCooldown(t *testing.T) {
	day := 24 * time.Hour
	ages := map[string]time.Duration{
		"1.0.0":        30 * day,
		"1.1.0":        10 * day,
		"1.2.0":        day,
		"2.0.0-beta.1": 2 * day,
	}
	var asked, withheld []string
	body, removed, err := FilterReleases(testPackagePURL, releaseList("1.0.0", "1.1.0", "1.2.0", "2.0.0-beta.1"), CooldownOptions{
		Cooldown:    testCooldown(),
		EvaluatedAt: evaluatedAt,
		PublishTime: timesFrom(ages, &asked),
		OnWithheld:  func(v string, _ time.Time) { withheld = append(withheld, v) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := listedVersions(t, body), []string{"1.0.0", "1.1.0"}; !slices.Equal(got, want) {
		t.Errorf("listed = %v, want %v", got, want)
	}
	if want := []string{"2.0.0-beta.1", "1.2.0", "1.1.0"}; !slices.Equal(asked, want) {
		t.Errorf("asked = %v, want %v: newest first, stopping at the first old enough", asked, want)
	}
	if want := []string{"2.0.0-beta.1", "1.2.0"}; !slices.Equal(withheld, want) {
		t.Errorf("withheld = %v, want %v", withheld, want)
	}
	if !removed["1.2.0"] || !removed["2.0.0-beta.1"] || len(removed) != 2 {
		t.Errorf("removed = %v", removed)
	}
}

// TestFilterReleasesLooksPastAnOldPrerelease checks that a prerelease out
// of cooldown above the stable line doesn't end the walk: the stable
// releases below it are still checked, down to the first that is old enough.
func TestFilterReleasesLooksPastAnOldPrerelease(t *testing.T) {
	day := 24 * time.Hour
	ages := map[string]time.Duration{
		"3.0.0-alpha.1": 90 * day,
		"2.5.1":         time.Hour,
		"2.5.0":         30 * day,
		"2.4.0":         60 * day,
	}
	var asked []string
	body, removed, err := FilterReleases(testPackagePURL, releaseList("2.4.0", "2.5.0", "2.5.1", "3.0.0-alpha.1"), CooldownOptions{
		Cooldown:    testCooldown(),
		EvaluatedAt: evaluatedAt,
		PublishTime: timesFrom(ages, &asked),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := listedVersions(t, body), []string{"2.4.0", "2.5.0", "3.0.0-alpha.1"}; !slices.Equal(got, want) {
		t.Errorf("listed = %v, want %v", got, want)
	}
	if want := []string{"3.0.0-alpha.1", "2.5.1", "2.5.0"}; !slices.Equal(asked, want) {
		t.Errorf("asked = %v, want %v: past the old prerelease, stopping at the first old stable release", asked, want)
	}
	if !removed["2.5.1"] || len(removed) != 1 {
		t.Errorf("removed = %v, want 2.5.1", removed)
	}
}

func TestFilterReleasesStopsAtAReleaseWithoutTime(t *testing.T) {
	var asked []string
	in := releaseList("1.0.0", "1.1.0", "1.2.0")
	body, removed, err := FilterReleases(testPackagePURL, in, CooldownOptions{
		Cooldown:    testCooldown(),
		EvaluatedAt: evaluatedAt,
		PublishTime: timesFrom(map[string]time.Duration{"1.1.0": time.Hour}, &asked),
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(in) || len(removed) != 0 {
		t.Errorf("body changed: %s, removed %v", body, removed)
	}
	if want := []string{"1.2.0"}; !slices.Equal(asked, want) {
		t.Errorf("asked = %v, want %v: an upstream that records no time for the newest release records none", asked, want)
	}
}

func TestFilterReleasesSkipsUnavailableAndNonSemverReleases(t *testing.T) {
	doc := map[string]any{"releases": map[string]any{
		"1.0.0":  map[string]any{"url": "u"},
		"1.1.0":  map[string]any{"url": "u"},
		"3.0.0":  map[string]any{"problem": map[string]any{"status": 410, "detail": "removed"}},
		"latest": map[string]any{"url": "u"},
	}}
	in, _ := json.Marshal(doc)
	var asked []string
	body, _, err := FilterReleases(testPackagePURL, in, CooldownOptions{
		Cooldown:    testCooldown(),
		EvaluatedAt: evaluatedAt,
		PublishTime: timesFrom(map[string]time.Duration{"1.0.0": 30 * 24 * time.Hour, "1.1.0": time.Hour}, &asked),
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"1.1.0", "1.0.0"}; !slices.Equal(asked, want) {
		t.Errorf("asked = %v, want %v", asked, want)
	}
	if got, want := listedVersions(t, body), []string{"1.0.0", "3.0.0", "latest"}; !slices.Equal(got, want) {
		t.Errorf("listed = %v, want %v", got, want)
	}
}

func TestFilterReleasesLeavesTheDocumentAlone(t *testing.T) {
	in := releaseList("1.0.0")
	fail := func(string) (time.Time, error) { t.Error("PublishTime called"); return time.Time{}, nil }
	for name, o := range map[string]CooldownOptions{
		"no policy":       {PublishTime: fail},
		"policy disabled": {Cooldown: &cooldown.Config{}, PublishTime: fail},
	} {
		body, removed, err := FilterReleases(testPackagePURL, in, o)
		if err != nil || string(body) != string(in) || len(removed) != 0 {
			t.Errorf("%s: body %s, removed %v, err %v", name, body, removed, err)
		}
	}
	body, _, err := FilterReleases(testPackagePURL, []byte(`{"other":1}`), CooldownOptions{Cooldown: testCooldown(), PublishTime: fail})
	if err != nil || string(body) != `{"other":1}` {
		t.Errorf("document without releases: %s, %v", body, err)
	}
}

func TestFilterReleasesFailsWhenATimeCantBeRead(t *testing.T) {
	boom := errors.New("upstream down")
	_, _, err := FilterReleases(testPackagePURL, releaseList("1.0.0"), CooldownOptions{
		Cooldown:    testCooldown(),
		EvaluatedAt: evaluatedAt,
		PublishTime: func(string) (time.Time, error) { return time.Time{}, boom },
	})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want %v", err, boom)
	}
	if _, _, err := FilterReleases(testPackagePURL, []byte(`nope`), CooldownOptions{Cooldown: testCooldown()}); err == nil {
		t.Error("invalid JSON: no error")
	}
}

func TestWithoutLatestVersion(t *testing.T) {
	removed := map[string]bool{"1.2.0": true}
	for _, tc := range []struct{ in, want string }{
		{
			`<https://r.example/apple/example/1.2.0>; rel="latest-version", <https://r.example/apple/example?page=2>; rel="next"`,
			`<https://r.example/apple/example?page=2>; rel="next"`,
		},
		{
			`<https://r.example/apple/example/1.1.0>; rel="latest-version", <https://r.example/apple/example?page=2>; rel="next"`,
			`<https://r.example/apple/example/1.1.0>; rel="latest-version", <https://r.example/apple/example?page=2>; rel="next"`,
		},
		{`<https://r.example/apple/example/1.2.0>; rel="canonical"`, `<https://r.example/apple/example/1.2.0>; rel="canonical"`},
		{`<https://r.example/apple/example/1.2.0>; rel=latest-version`, ``},
		{`<https://r.example/apple/example/1.2.0.json>; rel="latest-version"`, ``},
		{`<https://r.example/apple/example/1.2.0?x=1>; rel="latest-version"`, ``},
		{`<https://r.example/apple/example/1.1.0.json>; rel="latest-version"`, `<https://r.example/apple/example/1.1.0.json>; rel="latest-version"`},
		{``, ``},
	} {
		if got := WithoutLatestVersion(tc.in, removed); got != tc.want {
			t.Errorf("WithoutLatestVersion(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
