// Package cargo transforms Cargo sparse index metadata without storage or
// network access.
package cargo

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/proxy/internal/packageurl"
)

const (
	indexLen1 = 1
	indexLen2 = 2
	indexLen3 = 3
)

// CooldownPolicy evaluates version age at the caller's supplied time.
type CooldownPolicy interface {
	Enabled() bool
	Evaluate(ecosystem, packagePURL string, publishedAt, evaluatedAt time.Time) cooldown.Decision
}

// Options supplies the policy and callbacks for a filter. EvaluatedAt is
// used as given; the transformation never reads the clock.
type Options struct {
	Cooldown    CooldownPolicy
	EvaluatedAt time.Time
	// Denied reports whether the denylist names a version of a crate.
	Denied func(name, version string) bool
	// Optional callbacks report changes synchronously. OnWithheld is called
	// for each version cooldown holds back, with its publish time;
	// OnInvalidEntry for each index line that isn't a JSON entry, which is
	// left out.
	OnWithheld     func(name, version string, publishedAt time.Time)
	OnInvalidEntry func(err error)
}

// Entry is the part of a sparse index line the proxy reads.
type Entry struct {
	Name        string `json:"name"`
	Version     string `json:"vers"`
	PublishTime string `json:"pubtime,omitempty"`
}

// Filters reports whether Filter would change anything under o: with
// neither an enabled cooldown nor a denylist, the index is served as is.
func (o Options) Filters() bool {
	return o.cooldownEnabled() || o.Denied != nil
}

func (o Options) cooldownEnabled() bool {
	return o.Cooldown != nil && o.Cooldown.Enabled()
}

// Filter leaves out of a sparse index the entries the denylist names and
// those cooldown holds back, by their pubtime. Entries without a readable
// pubtime are kept. Each kept line ends in a newline. Lines that aren't JSON
// entries are left out and reported to o.OnInvalidEntry. The error is one
// reading the index; the lines read before it are returned.
func Filter(body []byte, o Options) ([]byte, error) {
	if !o.Filters() {
		return body, nil
	}
	cooldownEnabled := o.cooldownEnabled()
	out := make([]byte, 0, len(body))
	scanner := bufio.NewScanner(bytes.NewReader(body))
	// Entries with many dependencies can exceed Scanner's default token
	// limit; no line is longer than the body.
	scanner.Buffer(nil, len(body)+1)
	for scanner.Scan() {
		line := scanner.Bytes()
		var entry Entry
		if err := json.Unmarshal(line, &entry); err != nil {
			if o.OnInvalidEntry != nil {
				o.OnInvalidEntry(err)
			}
			continue
		}
		if o.Denied != nil && o.Denied(entry.Name, entry.Version) {
			continue
		}
		if cooldownEnabled && o.withheld(entry) {
			continue
		}
		out = append(out, line...)
		out = append(out, '\n')
	}
	return out, scanner.Err()
}

// withheld reports whether cooldown holds back entry, telling
// o.OnWithheld when it does.
func (o Options) withheld(entry Entry) bool {
	// An entry without a pubtime, or with one that doesn't parse, is kept.
	publishedAt, err := time.Parse(time.RFC3339, entry.PublishTime)
	if err != nil {
		return false
	}
	packagePURL := packageurl.MakeString("cargo", entry.Name, "")
	if o.Cooldown.Evaluate("cargo", packagePURL, publishedAt, o.EvaluatedAt).Allowed {
		return false
	}
	if o.OnWithheld != nil {
		o.OnWithheld(entry.Name, entry.Version, publishedAt)
	}
	return true
}

// PublishTime reads a version's pubtime from a sparse index. listed
// reports whether the index has the version at all; ok whether its pubtime
// is readable, which entries published before crates.io recorded pubtime
// aren't. A line that isn't a JSON entry is an error.
func PublishTime(body []byte, version string) (published time.Time, ok, listed bool, err error) {
	for line := range bytes.SplitSeq(body, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var entry Entry
		if err := json.Unmarshal(line, &entry); err != nil {
			return time.Time{}, false, false, err
		}
		if entry.Version != version {
			continue
		}
		published, err := time.Parse(time.RFC3339, entry.PublishTime)
		return published, err == nil, true, nil
	}
	return time.Time{}, false, false, nil
}

// IndexPath is a crate's path in the sparse index, by the length of its
// lowercased name: 1/{name}, 2/{name}, 3/{a}/{name} or {ab}/{cd}/{name}.
func IndexPath(name string) string {
	name = strings.ToLower(name)
	switch len(name) {
	case indexLen1:
		return "1/" + name
	case indexLen2:
		return "2/" + name
	case indexLen3:
		return fmt.Sprintf("3/%c/%s", name[0], name)
	default:
		return fmt.Sprintf("%s/%s/%s", name[0:2], name[2:4], name)
	}
}

// DownloadTemplate is the config.json dl template that points crate
// downloads at the proxy.
func DownloadTemplate(proxyURL string) string {
	return proxyURL + "/cargo/crates/{crate}/{version}/download"
}
