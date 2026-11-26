package chef

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/git-pkgs/cooldown"
)

// Ecosystem is the ecosystem cooldown is evaluated for.
const Ecosystem = "chef"

// CooldownPolicy evaluates version age at the caller's supplied time.
type CooldownPolicy interface {
	Enabled() bool
	Evaluate(ecosystem, packagePURL string, publishedAt, evaluatedAt time.Time) cooldown.Decision
}

// CooldownOptions supplies the policy and publish times for Withheld.
// EvaluatedAt is used as given; the transformation never reads the clock.
type CooldownOptions struct {
	Cooldown    CooldownPolicy
	EvaluatedAt time.Time
	// PublishTime returns a version's publish time, the zero time when it
	// is unknown, which lets the version through. It is called from the
	// newest version back, only as far as versions can be in cooldown, so
	// a caller can look times up lazily. An error ends the walk.
	PublishTime func(version string) (time.Time, error)
	// OnWithheld is called synchronously for each version cooldown holds
	// back, with its publish time.
	OnWithheld func(version string, publishedAt time.Time)
}

// PublishedAt reads published_at from a cookbook version's document, the
// zero time when it has none.
func PublishedAt(body []byte) (time.Time, error) {
	var document struct {
		PublishedAt string `json:"published_at"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		return time.Time{}, fmt.Errorf("%w: cookbook version: %w", ErrDocument, err)
	}
	return parseTime(document.PublishedAt)
}

func parseTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: time %q: %w", ErrDocument, value, err)
	}
	return t, nil
}

// Cookbook reads a cookbook document's updated_at, the zero time when it
// has none, and the versions it lists.
func Cookbook(body []byte) (time.Time, []string, error) {
	var document struct {
		UpdatedAt string            `json:"updated_at"`
		Versions  []json.RawMessage `json:"versions"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		return time.Time{}, nil, fmt.Errorf("%w: cookbook: %w", ErrDocument, err)
	}
	updated, err := parseTime(document.UpdatedAt)
	if err != nil {
		return time.Time{}, nil, err
	}
	versions := make([]string, 0, len(document.Versions))
	for _, raw := range document.Versions {
		if v, ok := versionOfURL(raw); ok {
			versions = append(versions, v)
		}
	}
	return updated, versions, nil
}

// UniverseVersions returns the versions /universe lists for each of names
// it has.
func UniverseVersions(body []byte, names []string) (map[string][]string, error) {
	var universe map[string]map[string]json.RawMessage
	if err := json.Unmarshal(body, &universe); err != nil {
		return nil, fmt.Errorf("%w: universe: %w", ErrDocument, err)
	}
	listed := map[string][]string{}
	for _, name := range names {
		for v := range universe[name] {
			listed[name] = append(listed[name], v)
		}
	}
	return listed, nil
}

// Withheld returns the versions of a cookbook in cooldown. updated is the
// cookbook's updated_at, which Supermarket sets when a version is
// published, so when it is out of cooldown nothing is looked up. Otherwise
// versions are checked from the highest down to the first that is old
// enough; a version without a time is let through and the walk goes on.
func Withheld(packagePURL string, updated time.Time, versions []string, o CooldownOptions) (map[string]bool, error) {
	if o.Cooldown == nil || !o.Cooldown.Enabled() {
		return nil, nil
	}
	allowed := func(published time.Time) bool {
		return o.Cooldown.Evaluate(Ecosystem, packagePURL, published, o.EvaluatedAt).Allowed
	}
	if !updated.IsZero() && allowed(updated) {
		return nil, nil
	}

	sorted := make([]string, 0, len(versions))
	for _, v := range versions {
		if v, ok := NormalizeVersion(v); ok {
			sorted = append(sorted, v)
		}
	}
	slices.SortFunc(sorted, func(a, b string) int { return compareVersions(b, a) })

	withheld := map[string]bool{}
	for _, v := range sorted {
		var published time.Time
		if o.PublishTime != nil {
			var err error
			if published, err = o.PublishTime(v); err != nil {
				return nil, fmt.Errorf("reading time of %s: %w", v, err)
			}
		}
		if published.IsZero() {
			continue
		}
		if allowed(published) {
			break
		}
		withheld[v] = true
		if o.OnWithheld != nil {
			o.OnWithheld(v, published)
		}
	}
	return withheld, nil
}

// versionOfURL returns the version a cookbook version URL, a JSON string
// ending in /versions/{version}, names.
func versionOfURL(raw json.RawMessage) (string, bool) {
	var versionURL string
	if err := json.Unmarshal(raw, &versionURL); err != nil {
		return "", false
	}
	parsed, err := url.Parse(versionURL)
	if err != nil {
		return "", false
	}
	return NormalizeVersion(path.Base(parsed.Path))
}

// compareVersions compares two dotted cookbook versions part by part, a
// missing part counting as 0.
func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := range max(len(as), len(bs)) {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if c := cmp.Compare(x, y); c != 0 {
			return c
		}
	}
	return 0
}

// RecentCookbooks reads the cookbook names from a page of
// /api/v1/cookbooks?order=recently_updated, in its order, and the total
// number of cookbooks the listing has.
func RecentCookbooks(body []byte) ([]string, int, error) {
	var page struct {
		Total int `json:"total"`
		Items []struct {
			Name string `json:"cookbook_name"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, 0, fmt.Errorf("%w: cookbook list: %w", ErrDocument, err)
	}
	names := make([]string, 0, len(page.Items))
	for _, item := range page.Items {
		if item.Name != "" {
			names = append(names, item.Name)
		}
	}
	return names, page.Total, nil
}
