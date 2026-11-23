// Package gem transforms RubyGems metadata without storage or network
// access.
package gem

import (
	"bytes"
	"encoding/json"
	"time"
)

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

// FilterIndex returns the compact index without the lines of the versions
// withheld reports, and the versions it kept. onWithheld, if set, is
// called for each version left out.
func FilterIndex(index []byte, withheld func(version string) bool, onWithheld func(version string)) ([]byte, []string) {
	var kept []string
	var out bytes.Buffer
	out.Grow(len(index))
	for rest := index; len(rest) > 0; {
		line, tail, _ := bytes.Cut(rest, []byte("\n"))
		rest = tail

		version := lineVersion(line)
		if listsVersion(version) {
			if withheld(string(version)) {
				if onWithheld != nil {
					onWithheld(string(version))
				}
				continue
			}
			kept = append(kept, string(version))
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	return out.Bytes(), kept
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
