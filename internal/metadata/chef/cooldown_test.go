package chef

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

const testPackagePURL = "pkg:chef/nginx"

var evaluatedAt = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

func testCooldown() *cooldown.Config { return &cooldown.Config{Default: "3d"} }

func TestPublishedAt(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       time.Time
		wantErr    bool
	}{
		{"set", `{"version":"12.0.0","published_at":"2021-05-07T19:40:08Z"}`, time.Date(2021, 5, 7, 19, 40, 8, 0, time.UTC), false},
		{"fractional", `{"published_at":"2021-05-07T19:40:08.123Z"}`, time.Date(2021, 5, 7, 19, 40, 8, 123e6, time.UTC), false},
		{"missing", `{"version":"12.0.0"}`, time.Time{}, false},
		{"not a time", `{"published_at":"soon"}`, time.Time{}, true},
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

func withheldIn(doc []byte, o CooldownOptions) (map[string]bool, error) {
	info, err := Cookbook(doc)
	if err != nil {
		return nil, err
	}
	return Withheld(testPackagePURL, info.UpdatedAt, info.Versions, o)
}

// cookbookDoc is a cookbook document listing versions, newest first as
// Supermarket lists them, updated at updated.
func cookbookDoc(updated time.Time, versions ...string) []byte {
	urls := make([]string, len(versions))
	for i, v := range versions {
		urls[i] = fmt.Sprintf("%q", "https://supermarket.chef.io/api/v1/cookbooks/nginx/versions/"+v)
	}
	latest := "null"
	if len(urls) > 0 {
		latest = urls[0]
	}
	return fmt.Appendf(nil, `{"name":"nginx","latest_version":%s,"updated_at":%q,"versions":[%s]}`,
		latest, updated.Format(time.RFC3339), strings.Join(urls, ","))
}

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

func TestWithheldWalksFromTheNewestVersion(t *testing.T) {
	day := 24 * time.Hour
	ages := map[string]time.Duration{"12.0.0": 30 * day, "12.1.0": 10 * day, "12.2.0": day, "12.10.0": time.Hour}
	var asked, reported []string
	// Listed out of order, to check versions are compared, not taken as listed.
	doc := cookbookDoc(evaluatedAt.Add(-time.Hour), "12.2.0", "12.10.0", "12.0.0", "12.1.0")
	withheld, err := withheldIn(doc, CooldownOptions{
		Cooldown:    testCooldown(),
		EvaluatedAt: evaluatedAt,
		PublishTime: timesFrom(ages, &asked),
		OnWithheld:  func(v string, _ time.Time) { reported = append(reported, v) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"12.10.0", "12.2.0", "12.1.0"}; !slices.Equal(asked, want) {
		t.Errorf("asked = %v, want %v", asked, want)
	}
	if want := []string{"12.10.0", "12.2.0"}; !slices.Equal(reported, want) {
		t.Errorf("reported = %v, want %v", reported, want)
	}
	if len(withheld) != 2 || !withheld["12.10.0"] || !withheld["12.2.0"] {
		t.Errorf("withheld = %v", withheld)
	}
}

func TestWithheldSkipsVersionsWithoutTime(t *testing.T) {
	var asked []string
	doc := cookbookDoc(evaluatedAt.Add(-time.Hour), "1.2.0", "1.1.0", "1.0.0")
	withheld, err := withheldIn(doc, CooldownOptions{
		Cooldown:    testCooldown(),
		EvaluatedAt: evaluatedAt,
		PublishTime: timesFrom(map[string]time.Duration{"1.1.0": time.Hour, "1.0.0": 30 * 24 * time.Hour}, &asked),
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"1.2.0", "1.1.0", "1.0.0"}; !slices.Equal(asked, want) {
		t.Errorf("asked = %v, want %v", asked, want)
	}
	if len(withheld) != 1 || !withheld["1.1.0"] {
		t.Errorf("withheld = %v, want only 1.1.0", withheld)
	}
}

func TestWithheldShortCircuitsOnUpdatedAt(t *testing.T) {
	fail := func(string) (time.Time, error) { t.Error("PublishTime called"); return time.Time{}, nil }
	doc := cookbookDoc(evaluatedAt.Add(-10*24*time.Hour), "1.0.0")
	for name, o := range map[string]CooldownOptions{
		"old enough":      {Cooldown: testCooldown(), EvaluatedAt: evaluatedAt, PublishTime: fail},
		"no policy":       {PublishTime: fail},
		"policy disabled": {Cooldown: &cooldown.Config{}, PublishTime: fail},
	} {
		if withheld, err := withheldIn(doc, o); err != nil || len(withheld) != 0 {
			t.Errorf("%s: withheld %v, err %v", name, withheld, err)
		}
	}
}

func TestWithheldErrors(t *testing.T) {
	boom := errors.New("upstream down")
	doc := cookbookDoc(evaluatedAt, "1.0.0")
	if _, err := withheldIn(doc, CooldownOptions{
		Cooldown: testCooldown(), EvaluatedAt: evaluatedAt,
		PublishTime: func(string) (time.Time, error) { return time.Time{}, boom },
	}); !errors.Is(err, boom) {
		t.Errorf("err = %v, want %v", err, boom)
	}
	if _, err := Cookbook([]byte(`nope`)); !errors.Is(err, ErrDocument) {
		t.Errorf("invalid JSON: err = %v, want ErrDocument", err)
	}
	if _, err := Cookbook([]byte(`{"updated_at":"soon"}`)); !errors.Is(err, ErrDocument) {
		t.Errorf("invalid updated_at: err = %v, want ErrDocument", err)
	}
}

func TestRewriteCookbookLeavesOutWithheldVersions(t *testing.T) {
	doc := cookbookDoc(evaluatedAt, "1.2.0", "1.1.0", "1.0.0")
	withheld := map[string]bool{"1.2.0": true}
	out, err := RewriteCookbook("nginx", doc, Options{ProxyURL: proxy, Withheld: func(name, v string) bool { return name == "nginx" && withheld[v] }})
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, out)
	if got, want := m["latest_version"], proxy+"/chef/api/v1/cookbooks/nginx/versions/1.1.0"; got != want {
		t.Errorf("latest_version = %v, want %v", got, want)
	}
	versions := m["versions"].([]any)
	if len(versions) != 2 || versions[0] != proxy+"/chef/api/v1/cookbooks/nginx/versions/1.1.0" {
		t.Errorf("versions = %v", versions)
	}

	withheld = map[string]bool{"1.2.0": true, "1.1.0": true, "1.0.0": true}
	out, err = RewriteCookbook("nginx", doc, Options{ProxyURL: proxy, Withheld: func(_, v string) bool { return withheld[v] }})
	if err != nil {
		t.Fatal(err)
	}
	m = decode(t, out)
	if m["latest_version"] != nil || len(m["versions"].([]any)) != 0 {
		t.Errorf("all withheld: latest_version = %v, versions = %v", m["latest_version"], m["versions"])
	}
}

func TestRewriteUniverseLeavesOutWithheldVersions(t *testing.T) {
	body := []byte(`{"nginx":{"1.0.0":{"location_path":"x","download_url":"y","dependencies":{}},"1.1.0":{"location_path":"x","download_url":"y","dependencies":{}}},"apt":{"1.1.0":{"location_path":"x","download_url":"y","dependencies":{}}}}`)
	out, err := RewriteUniverse(body, Options{ProxyURL: proxy, Withheld: func(name, v string) bool { return name == "nginx" && v == "1.1.0" }})
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, out)
	nginx := m["nginx"].(map[string]any)
	if _, ok := nginx["1.1.0"]; ok || len(nginx) != 1 {
		t.Errorf("nginx = %v, want only 1.0.0", nginx)
	}
	if _, ok := m["apt"].(map[string]any)["1.1.0"]; !ok {
		t.Error("apt 1.1.0 left out, though only nginx's is withheld")
	}
}

func TestWithheldWithoutUpdatedAtChecksVersions(t *testing.T) {
	var asked []string
	withheld, err := Withheld(testPackagePURL, time.Time{}, []string{"1.0.0", "1.1"}, CooldownOptions{
		Cooldown: testCooldown(), EvaluatedAt: evaluatedAt,
		PublishTime: timesFrom(map[string]time.Duration{"1.1": time.Hour, "1.0.0": 30 * 24 * time.Hour}, &asked),
	})
	if err != nil || len(withheld) != 1 || !withheld["1.1"] {
		t.Errorf("withheld = %v, %v; want 1.1", withheld, err)
	}
}

func TestCookbookDeprecated(t *testing.T) {
	for body, want := range map[string]bool{
		`{"name":"legacy","deprecated":true,"versions":[]}`: true,
		`{"name":"nginx","deprecated":false,"versions":[]}`: false,
		`{"name":"nginx","deprecated":null,"versions":[]}`:  false,
		`{"name":"nginx","versions":[]}`:                    false,
	} {
		info, err := Cookbook([]byte(body))
		if err != nil || info.Deprecated != want {
			t.Errorf("Cookbook(%s) deprecated = %v, %v; want %v", body, info.Deprecated, err, want)
		}
	}
}

func TestUniverseVersions(t *testing.T) {
	got, err := UniverseVersions([]byte(`{"nginx":{"1.0.0":{},"1.1.0":null},"apt":{"2.0.0":{}}}`), []string{"nginx", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	versions := got["nginx"]
	slices.Sort(versions)
	if len(got) != 1 || !slices.Equal(versions, []string{"1.0.0", "1.1.0"}) {
		t.Errorf("UniverseVersions = %v", got)
	}
	if _, err := UniverseVersions([]byte(`[]`), nil); !errors.Is(err, ErrDocument) {
		t.Errorf("err = %v, want ErrDocument", err)
	}
}

func TestRecentCookbooks(t *testing.T) {
	names, total, err := RecentCookbooks([]byte(`{"start":0,"total":4083,"items":[{"cookbook_name":"redisio","cookbook":"https://supermarket.chef.io/api/v1/cookbooks/redisio"},{"cookbook_name":"podman"}]}`))
	if err != nil || total != 4083 || !slices.Equal(names, []string{"redisio", "podman"}) {
		t.Errorf("RecentCookbooks = %v, %d, %v", names, total, err)
	}
	if _, _, err := RecentCookbooks([]byte(`[]`)); !errors.Is(err, ErrDocument) {
		t.Errorf("err = %v, want ErrDocument", err)
	}
	// A body without a total is not a listing: reading it as an empty one
	// would let every version through.
	for _, body := range []string{`{}`, `null`, `{"items":[]}`} {
		if _, _, err := RecentCookbooks([]byte(body)); !errors.Is(err, ErrDocument) {
			t.Errorf("%s: err = %v, want ErrDocument", body, err)
		}
	}
	if names, total, err := RecentCookbooks([]byte(`{"total":0,"items":[]}`)); err != nil || total != 0 || len(names) != 0 {
		t.Errorf("empty listing = %v, %d, %v", names, total, err)
	}
}

func TestCompareVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"1.10.0", "1.9.0", 1},
		{"1.2", "1.2.0", 0},
		{"1.2", "1.2.1", -1},
		{"2.0.0", "10.0.0", -1},
	} {
		if got := compareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("compareVersions(%s, %s) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
