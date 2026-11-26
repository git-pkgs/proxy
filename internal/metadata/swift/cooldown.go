package swift

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/git-pkgs/cooldown"
)

// Ecosystem is the ecosystem cooldown is evaluated for.
const Ecosystem = "swift"

// CooldownPolicy evaluates version age at the caller's supplied time.
type CooldownPolicy interface {
	Enabled() bool
	Evaluate(ecosystem, packagePURL string, publishedAt, evaluatedAt time.Time) cooldown.Decision
}

// CooldownOptions supplies the policy and publish times for FilterReleases.
// EvaluatedAt is used as given; the transformation never reads the clock.
type CooldownOptions struct {
	Cooldown    CooldownPolicy
	EvaluatedAt time.Time
	// PublishTime returns a release's publish time, the zero time when the
	// upstream records none. It is called from the newest release back,
	// only as far as releases can be in cooldown, so a caller can look
	// times up lazily. An error ends the filter.
	PublishTime func(version string) (time.Time, error)
	// OnWithheld is called synchronously for each release cooldown holds
	// back, with its publish time.
	OnWithheld func(version string, publishedAt time.Time)
}

// PublishedAt reads publishedAt from a release's metadata: the time the
// registry recorded the release, which the specification makes optional.
// It is the zero time when the upstream doesn't send it. The publisher's
// own metadata.originalPublicationTime is not used.
func PublishedAt(body []byte) (time.Time, error) {
	var metadata struct {
		PublishedAt string `json:"publishedAt"`
	}
	if err := json.Unmarshal(body, &metadata); err != nil {
		return time.Time{}, fmt.Errorf("parsing release metadata: %w", err)
	}
	if metadata.PublishedAt == "" {
		return time.Time{}, nil
	}
	published, err := time.Parse(time.RFC3339, metadata.PublishedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing publishedAt: %w", err)
	}
	return published, nil
}

// FilterReleases removes releases in cooldown from a package's release
// list and returns the versions it removed. A document without a releases
// object, and everything when no enabled policy is set, is returned
// unchanged.
//
// The list has no times, so releases are checked from the highest version
// down to the first that is old enough. A release without a time also ends
// the walk: an upstream that doesn't record the newest release's time
// records none. Releases marked with a problem and versions that aren't
// semantic versions are left as they are.
func FilterReleases(packagePURL string, body []byte, o CooldownOptions) ([]byte, map[string]bool, error) {
	var metadata map[string]any
	if err := json.Unmarshal(body, &metadata); err != nil {
		return nil, nil, err
	}
	releases, ok := metadata["releases"].(map[string]any)
	if !ok || o.Cooldown == nil || !o.Cooldown.Enabled() {
		return body, nil, nil
	}

	removed := map[string]bool{}
	for _, v := range newestFirst(releases) {
		var published time.Time
		if o.PublishTime != nil {
			var err error
			if published, err = o.PublishTime(v); err != nil {
				return nil, nil, fmt.Errorf("reading time of %s: %w", v, err)
			}
		}
		if published.IsZero() || o.Cooldown.Evaluate(Ecosystem, packagePURL, published, o.EvaluatedAt).Allowed {
			break
		}
		removed[v] = true
		if o.OnWithheld != nil {
			o.OnWithheld(v, published)
		}
	}
	if len(removed) == 0 {
		return body, nil, nil
	}
	for v := range removed {
		delete(releases, v)
	}
	filtered, err := json.Marshal(metadata)
	if err != nil {
		return nil, nil, err
	}
	return filtered, removed, nil
}

// newestFirst returns the available releases whose versions are semantic
// versions, highest first.
func newestFirst(releases map[string]any) []string {
	type release struct {
		name    string
		version *semver.Version
	}
	var sorted []release
	for name, value := range releases {
		if entry, ok := value.(map[string]any); ok && entry["problem"] != nil {
			continue
		}
		if v, err := semver.StrictNewVersion(name); err == nil {
			sorted = append(sorted, release{name, v})
		}
	}
	slices.SortFunc(sorted, func(a, b release) int { return b.version.Compare(a.version) })
	names := make([]string, len(sorted))
	for i, r := range sorted {
		names[i] = r.name
	}
	return names
}

// WithoutLatestVersion drops the latest-version link from a release list's
// Link header value when it points at a removed release. The link is
// optional, and the release list itself still says which releases there
// are.
func WithoutLatestVersion(value string, removed map[string]bool) string {
	if value == "" || len(removed) == 0 {
		return value
	}
	var kept []string
	for _, link := range strings.Split(value, ",") {
		target, params, _ := strings.Cut(strings.TrimSpace(link), ";")
		target = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(target), "<"), ">")
		version := target[strings.LastIndexByte(target, '/')+1:]
		if hasRel(params, "latest-version") && removed[version] {
			continue
		}
		kept = append(kept, strings.TrimSpace(link))
	}
	return strings.Join(kept, ", ")
}

func hasRel(params, rel string) bool {
	for _, param := range strings.Split(params, ";") {
		key, value, ok := strings.Cut(strings.TrimSpace(param), "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "rel") {
			continue
		}
		if slices.Contains(strings.Fields(strings.Trim(strings.TrimSpace(value), `"`)), rel) {
			return true
		}
	}
	return false
}
