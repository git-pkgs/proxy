// Package maven transforms Maven repository metadata without storage or
// network access.
package maven

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/proxy/internal/packageurl"
)

const ecosystem = "maven"

const (
	// MetadataFile is the name of an artifact's version list.
	MetadataFile = "maven-metadata.xml"
	// Snapshot ends the versions cooldown leaves alone: their files change
	// after publishing.
	Snapshot = "-SNAPSHOT"
	// LastUpdatedLayout is the UTC timestamp format of <lastUpdated>.
	LastUpdatedLayout = "20060102150405"
)

// CooldownPolicy evaluates version age at the caller's supplied time.
type CooldownPolicy interface {
	Enabled() bool
	Evaluate(ecosystem, packagePURL string, publishedAt, evaluatedAt time.Time) cooldown.Decision
}

// Options supplies the policy and publish times for a filter. EvaluatedAt
// is used as given; the transformation never reads the clock.
type Options struct {
	Cooldown    CooldownPolicy
	EvaluatedAt time.Time
	// PublishTime returns a version's publish time, the zero time when it
	// is unknown, which lets the version through. It is called from the
	// newest version back, only as far as versions can be in cooldown, so
	// a caller can look times up lazily. An error ends the filter.
	PublishTime func(version string) (time.Time, error)
	// OnWithheld is called synchronously for each version cooldown holds
	// back, with its publish time.
	OnWithheld func(version string, publishedAt time.Time)
}

// PackagePURL is the package PURL cooldown is evaluated for.
func PackagePURL(group, artifact string) string {
	return packageurl.MakeString(ecosystem, group+":"+artifact, "")
}

// Coordinates returns the group and artifact of an artifact directory such
// as com/google/guava/guava.
func Coordinates(artifactDir string) (group, artifact string, ok bool) {
	i := strings.LastIndex(artifactDir, "/")
	if i <= 0 || i == len(artifactDir)-1 {
		return "", "", false
	}
	return strings.ReplaceAll(artifactDir[:i], "/", "."), artifactDir[i+1:], true
}

// UsableVersion reports whether version can be put in a URL path as one
// segment. Versions come from upstream metadata.
func UsableVersion(version string) bool {
	return version != "" && version != "." && version != ".." && !strings.ContainsAny(version, "/\\?#%")
}

// Filter removes versions in cooldown from an artifact's maven-metadata.xml
// and points <latest> and <release> at the newest versions left. Metadata
// without a version list, such as a group's plugin list or a SNAPSHOT's
// build list, and metadata for a directory that isn't an artifact's, is
// returned unchanged, as is everything when no enabled policy is set. The
// document is edited in place rather than encoded again, so everything
// else in it stays as upstream sent it.
//
// No version can be newer than the metadata listing it, so when
// <lastUpdated> is out of cooldown no time is looked up. Otherwise versions
// are checked from the end of the list back to the first that is old
// enough; SNAPSHOT versions are skipped.
func Filter(artifactDir string, body []byte, o Options) ([]byte, error) {
	doc, err := ParseMetadata(body)
	if err != nil {
		return nil, err
	}
	group, artifact, ok := Coordinates(artifactDir)
	if !ok || len(doc.Versions) == 0 || o.Cooldown == nil || !o.Cooldown.Enabled() {
		return body, nil
	}
	packagePURL := PackagePURL(group, artifact)
	if lastUpdated, err := time.Parse(LastUpdatedLayout, doc.LastUpdated); err == nil && o.allowed(packagePURL, lastUpdated) {
		return body, nil
	}

	removed := map[string]bool{}
	for i := len(doc.Versions) - 1; i >= 0; i-- {
		v := doc.Versions[i].Value
		if strings.HasSuffix(v, Snapshot) {
			continue
		}
		var published time.Time
		if o.PublishTime != nil {
			if published, err = o.PublishTime(v); err != nil {
				return nil, fmt.Errorf("reading time of %s: %w", v, err)
			}
		}
		if published.IsZero() {
			continue
		}
		if o.allowed(packagePURL, published) {
			break
		}
		removed[v] = true
		if o.OnWithheld != nil {
			o.OnWithheld(v, published)
		}
	}
	if len(removed) == 0 {
		return body, nil
	}
	return doc.Without(body, removed), nil
}

func (o Options) allowed(packagePURL string, published time.Time) bool {
	return o.Cooldown.Evaluate(ecosystem, packagePURL, published, o.EvaluatedAt).Allowed
}

// Element is an element of maven-metadata.xml: its text and where it
// starts and ends in the document.
type Element struct {
	Value      string
	Start, End int
}

// Metadata is what ParseMetadata reads from a maven-metadata.xml.
type Metadata struct {
	Versions        []Element
	Latest, Release *Element
	LastUpdated     string
}

// ParseMetadata reads the version list, <latest>, <release> and
// <lastUpdated> of a maven-metadata.xml, with their offsets.
func ParseMetadata(body []byte) (*Metadata, error) {
	dec := xml.NewDecoder(bytes.NewReader(body))
	// Versions are ASCII, so any declared charset can be read as bytes.
	dec.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil }

	doc := &Metadata{}
	var stack []string
	var current *Element
	for {
		offset := int(dec.InputOffset())
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parsing maven metadata: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			stack = append(stack, t.Name.Local)
			switch strings.Join(stack, "/") {
			case "metadata/versioning/versions/version",
				"metadata/versioning/latest",
				"metadata/versioning/release",
				"metadata/versioning/lastUpdated":
				current = &Element{Start: offset}
			}
		case xml.CharData:
			if current != nil {
				current.Value += string(t)
			}
		case xml.EndElement:
			if current != nil {
				current.Value = strings.TrimSpace(current.Value)
				current.End = int(dec.InputOffset())
				switch t.Name.Local {
				case "version":
					doc.Versions = append(doc.Versions, *current)
				case "latest":
					doc.Latest = current
				case "release":
					doc.Release = current
				case "lastUpdated":
					doc.LastUpdated = current.Value
				}
				current = nil
			}
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	return doc, nil
}

// Without returns body with the removed versions taken out of the version
// list, and <latest> and <release> set to the last versions left in list
// order, which is how the repository chose them. Either is dropped when no
// version is left for it.
func (doc *Metadata) Without(body []byte, removed map[string]bool) []byte {
	type edit struct {
		start, end int
		text       string
	}
	var edits []edit
	drop := func(e Element) {
		edits = append(edits, edit{start: lineStart(body, e.Start), end: e.End})
	}
	var latest, release string
	for _, v := range doc.Versions {
		if removed[v.Value] {
			drop(v)
			continue
		}
		latest = v.Value
		if !strings.HasSuffix(v.Value, Snapshot) {
			release = v.Value
		}
	}
	for _, field := range []struct {
		el   *Element
		name string
		next string
	}{{doc.Latest, "latest", latest}, {doc.Release, "release", release}} {
		if field.el == nil || !removed[field.el.Value] {
			continue
		}
		if field.next == "" {
			drop(*field.el)
			continue
		}
		edits = append(edits, edit{field.el.Start, field.el.End, "<" + field.name + ">" + field.next + "</" + field.name + ">"})
	}

	// Edits don't overlap; apply them in document order.
	slices.SortFunc(edits, func(a, b edit) int { return a.start - b.start })
	out := make([]byte, 0, len(body))
	pos := 0
	for _, e := range edits {
		out = append(out, body[pos:e.start]...)
		out = append(out, e.text...)
		pos = e.end
	}
	return append(out, body[pos:]...)
}

// lineStart moves an element's start back over the indentation and line
// break before it, so removing the element removes its line.
func lineStart(body []byte, start int) int {
	i := start
	for i > 0 && (body[i-1] == ' ' || body[i-1] == '\t') {
		i--
	}
	if i > 0 && body[i-1] == '\n' {
		i--
		if i > 0 && body[i-1] == '\r' {
			i--
		}
		return i
	}
	return start
}
