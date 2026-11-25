// Package pub transforms pub.dev package metadata without storage or
// network access.
package pub

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/proxy/internal/packageurl"
)

const ecosystem = "pub"

// CooldownPolicy evaluates version age at the caller's supplied time.
type CooldownPolicy interface {
	Enabled() bool
	Evaluate(ecosystem, packagePURL string, publishedAt, evaluatedAt time.Time) cooldown.Decision
}

// Options supplies the public URL and policy for a rewrite. EvaluatedAt is
// used as given; the transformation never reads the clock.
type Options struct {
	ProxyURL    string
	Cooldown    CooldownPolicy
	EvaluatedAt time.Time
	// OnCooldownFilter is called synchronously for each version cooldown
	// holds back, with its publish time.
	OnCooldownFilter func(name, version string, publishedAt time.Time)
}

// ArchiveURL is where the proxy serves a version's archive.
func ArchiveURL(proxyURL, name, version string) string {
	return fmt.Sprintf("%s/pub/packages/%s/versions/%s.tar.gz", proxyURL, name, version)
}

// Rewrite points each version's archive_url at the proxy and leaves out the
// versions cooldown holds back. A document without a versions list is
// returned unchanged. latest follows the versions kept (see updateLatest).
func Rewrite(name string, body []byte, o Options) ([]byte, error) {
	var metadata map[string]any
	if err := json.Unmarshal(body, &metadata); err != nil {
		return nil, err
	}

	versions, ok := metadata["versions"].([]any)
	if !ok {
		return body, nil
	}

	kept := filterVersions(name, versions, o)
	metadata["versions"] = kept
	updateLatest(metadata, kept)

	return json.Marshal(metadata)
}

// filterVersions keeps the entries with a version that cooldown doesn't
// hold back, with their archive_url pointing at the proxy.
func filterVersions(name string, versions []any, o Options) []any {
	packagePURL := packageurl.MakeString(ecosystem, name, "")
	kept := versions[:0]
	for _, vdata := range versions {
		vmap, ok := vdata.(map[string]any)
		if !ok {
			continue
		}
		version, _ := vmap["version"].(string)
		if version == "" {
			continue
		}
		if heldBack(packagePURL, name, version, vmap, o) {
			continue
		}
		vmap["archive_url"] = ArchiveURL(o.ProxyURL, name, version)
		kept = append(kept, vdata)
	}
	return kept
}

// heldBack reports whether cooldown holds a version back. A version without
// an RFC 3339 published time is let through.
func heldBack(packagePURL, name, version string, vmap map[string]any, o Options) bool {
	if o.Cooldown == nil || !o.Cooldown.Enabled() {
		return false
	}
	published, ok := vmap["published"].(string)
	if !ok {
		return false
	}
	publishedAt, err := time.Parse(time.RFC3339, published)
	if err != nil {
		return false
	}
	if o.Cooldown.Evaluate("pub", packagePURL, publishedAt, o.EvaluatedAt).Allowed {
		return false
	}
	if o.OnCooldownFilter != nil {
		o.OnCooldownFilter(name, version, publishedAt)
	}
	return true
}

// updateLatest points latest at the kept entry for its version, so its
// archive_url is the proxy's too. When cooldown removed that version,
// latest becomes the newest stable version left, or the newest version left
// if all are prereleases, and is dropped when none is left.
func updateLatest(metadata map[string]any, kept []any) {
	latest, ok := metadata["latest"].(map[string]any)
	if !ok {
		return
	}
	latestVer, _ := latest["version"].(string)

	var newest, newestStable *semver.Version
	var newestEntry, newestStableEntry any
	for _, vdata := range kept {
		vmap, ok := vdata.(map[string]any)
		if !ok {
			continue
		}
		version, _ := vmap["version"].(string)
		if version == latestVer {
			latest["archive_url"] = vmap["archive_url"]
			return
		}
		parsed, err := semver.NewVersion(version)
		if err != nil {
			continue
		}
		if newest == nil || parsed.GreaterThan(newest) {
			newest, newestEntry = parsed, vdata
		}
		if parsed.Prerelease() == "" && (newestStable == nil || parsed.GreaterThan(newestStable)) {
			newestStable, newestStableEntry = parsed, vdata
		}
	}

	switch {
	case newestStableEntry != nil:
		metadata["latest"] = newestStableEntry
	case newestEntry != nil:
		metadata["latest"] = newestEntry
	default:
		delete(metadata, "latest")
	}
}
