// Package gem transforms RubyGems metadata without storage or network
// access.
package gem

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/proxy/internal/packageurl"
)

const ecosystem = "gem"

// CooldownPolicy evaluates version age at the caller's supplied time.
type CooldownPolicy interface {
	Enabled() bool
	Evaluate(ecosystem, packagePURL string, publishedAt, evaluatedAt time.Time) cooldown.Decision
}

// Options supplies the policy and the publish times for a filter. The
// compact index carries no times, so the caller reads them from the
// versions API (see PublishTimes). EvaluatedAt is used as given; the
// transformation never reads the clock.
type Options struct {
	Cooldown    CooldownPolicy
	EvaluatedAt time.Time
	// Published maps each version, as the compact index writes it, to its
	// publish time. A version with the zero time is kept.
	Published map[string]time.Time
	// WithholdUnknown holds back the versions Published doesn't have, for
	// a caller whose times may be older than the index. They are kept
	// otherwise.
	WithholdUnknown bool
	// OnWithheld is called synchronously for each version cooldown holds
	// back, with its publish time: the zero time for a version held back
	// because Published doesn't have it.
	OnWithheld func(name, version string, publishedAt time.Time)
}

// IndexVersions lists the versions in a compact index (/info/{name}), as
// the versions API names them.
func IndexVersions(index []byte) []string {
	var versions []string
	for rest := index; len(rest) > 0; {
		line, tail, _ := bytes.Cut(rest, []byte("\n"))
		rest = tail
		if version := lineVersion(line); listsVersion(version) {
			versions = append(versions, string(version))
		}
	}
	return versions
}

// FilterIndex returns the compact index of the gem name without the lines
// of the versions cooldown holds back, and the versions it kept. Without an
// enabled policy every version is kept.
func FilterIndex(name string, index []byte, o Options) ([]byte, []string) {
	packagePURL := packageurl.MakeString(ecosystem, name, "")
	var kept []string
	var out bytes.Buffer
	out.Grow(len(index))
	for rest := index; len(rest) > 0; {
		line, tail, _ := bytes.Cut(rest, []byte("\n"))
		rest = tail

		version := lineVersion(line)
		if listsVersion(version) {
			if o.withheld(packagePURL, name, string(version)) {
				continue
			}
			kept = append(kept, string(version))
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	return out.Bytes(), kept
}

// withheld reports whether cooldown holds a version back, telling
// o.OnWithheld when it does.
func (o Options) withheld(packagePURL, name, version string) bool {
	if o.Cooldown == nil || !o.Cooldown.Enabled() {
		return false
	}
	publishedAt, known := o.Published[version]
	switch {
	case !known:
		if !o.WithholdUnknown {
			return false
		}
	case publishedAt.IsZero():
		return false
	case o.Cooldown.Evaluate("gem", packagePURL, publishedAt, o.EvaluatedAt).Allowed:
		return false
	}
	if o.OnWithheld != nil {
		o.OnWithheld(name, version, publishedAt)
	}
	return true
}

// lineVersion returns the version a compact index line starts with.
func lineVersion(line []byte) []byte {
	if spaceIdx := bytes.IndexByte(line, ' '); spaceIdx > 0 {
		return line[:spaceIdx]
	}
	return line
}

// listsVersion reports whether a compact index line's first field is a
// version, rather than the "---" that starts the file or an empty line.
func listsVersion(field []byte) bool {
	return len(field) > 0 && string(field) != "---"
}

// Version is a version entry from the RubyGems versions API.
type Version struct {
	Number    string `json:"number"`
	Platform  string `json:"platform"`
	CreatedAt string `json:"created_at"`
}

// PublishTimes reads the publish times from a versions API response
// (/api/v1/versions/{name}.json), keyed by version as the compact index
// writes it: a platform build's version carries its platform. A time that
// doesn't parse is the zero time.
func PublishTimes(versionsJSON []byte) (map[string]time.Time, error) {
	var versions []Version
	if err := json.Unmarshal(versionsJSON, &versions); err != nil {
		return nil, err
	}
	times := make(map[string]time.Time, len(versions))
	for _, v := range versions {
		version := v.Number
		if v.Platform != "" && v.Platform != "ruby" {
			version = v.Number + "-" + v.Platform
		}
		createdAt, err := time.Parse(time.RFC3339, v.CreatedAt)
		if err != nil {
			createdAt = time.Time{}
		}
		times[version] = createdAt
	}
	return times, nil
}
