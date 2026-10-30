package npm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"

	"github.com/git-pkgs/proxy/internal/jsonstream"
)

// ErrNotStreamable is returned by RewriteStream for a document it leaves to
// Rewrite: one without a versions object, such as a single version's
// metadata, or a rewrite that lists pushed versions.
var ErrNotStreamable = errors.New("npm metadata can't be rewritten as a stream")

// Scan is what a first pass over a packument learns: the version names, and
// the time map and dist-tags the filters work on. It holds no version
// entries, so it stays small whatever the size of the packument.
type Scan struct {
	versions       []string
	versionsObject bool
	time           any
	distTags       any
	// occurrences counts each filtered key, so the second pass can drop the
	// earlier duplicates, as Rewrite does.
	occurrences map[string]int
	hasModified bool
}

// Abbreviated reports whether the scanned document is an abbreviated
// packument, as the package function Abbreviated does for a document in
// memory.
func (s *Scan) Abbreviated() bool {
	return s.hasModified && s.occurrences["time"] == 0
}

// ScanPackument reads a packument once, keeping only what RewriteStream
// needs to decide which versions remain. Version entries are read one at a
// time and dropped.
func ScanPackument(r io.Reader) (*Scan, error) {
	s := &Scan{occurrences: map[string]int{}}
	dec := json.NewDecoder(r)
	if err := expectDelim(dec, '{'); err != nil {
		return nil, err
	}
	for dec.More() {
		key, err := objectKey(dec)
		if err != nil {
			return nil, err
		}
		switch key {
		case "versions":
			s.occurrences[key]++
			s.versions, s.versionsObject, err = objectKeys(dec)
		case "time":
			s.occurrences[key]++
			s.time = nil
			err = dec.Decode(&s.time)
		case "dist-tags":
			s.occurrences[key]++
			s.distTags = nil
			err = dec.Decode(&s.distTags)
		default:
			if key == "modified" {
				s.hasModified = true
			}
			err = skipValue(dec)
		}
		if err != nil {
			return nil, err
		}
	}
	if err := expectDelim(dec, '}'); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			err = errors.New("unexpected data after the top-level object")
		}
		return nil, err
	}
	return s, nil
}

// RewriteStream is Rewrite for a packument too large to hold in memory. scan
// is the result of ScanPackument over the same bytes r reads again; the
// caller checks they are the same. The rewritten document is written to w
// with the bytes of untouched values copied as they are, and its output is
// the same as Rewrite's for the same document and options. It returns the
// versions it kept. For a document without a versions object, or options
// listing pushed versions, it returns ErrNotStreamable before writing.
func RewriteStream(packageName string, scan *Scan, r io.Reader, w io.Writer, options Options) ([]string, error) {
	if !scan.versionsObject || len(options.LocalVersions) > 0 {
		return nil, ErrNotStreamable
	}
	h := &transformer{options}
	f := scan.filter(h, packageName)
	sw := &streamWriter{
		transformer: h,
		packageName: packageName,
		scan:        scan,
		filtered:    f,
		seen:        map[string]int{},
	}
	if err := jsonstream.CopyObject(json.NewDecoder(r), w, sw.member); err != nil {
		return nil, err
	}
	h.tarballsRewritten(sw.rewritten)
	return slices.Collect(maps.Keys(f.versions)), nil
}

// filtered is what cooldown and the denylist leave of a scanned packument.
type filtered struct {
	versions                 map[string]any
	timeMap, distTags        map[string]any
	timeChanged, tagsChanged bool
}

// filter runs the filters Rewrite runs over what scan collected. The scan is
// left as it is, so it can be filtered again.
func (s *Scan) filter(h *transformer, packageName string) filtered {
	versions := make(map[string]any, len(s.versions))
	for _, v := range s.versions {
		versions[v] = nil
	}
	metadata := map[string]any{}
	timeMap := s.objectValue(metadata, "time", s.time)
	distTags := s.objectValue(metadata, "dist-tags", s.distTags)
	timeEntries := len(timeMap)
	originalTags := maps.Clone(distTags)

	h.applyCooldownFiltering(metadata, versions, packageName)
	h.applyDenylistFiltering(metadata, versions)

	return filtered{
		versions:    versions,
		timeMap:     timeMap,
		distTags:    distTags,
		timeChanged: timeMap != nil && len(timeMap) != timeEntries,
		tagsChanged: distTags != nil && !reflect.DeepEqual(distTags, originalTags),
	}
}

// streamWriter writes the second pass of RewriteStream from the filtered
// state the first pass left.
type streamWriter struct {
	*transformer
	filtered
	packageName string
	scan        *Scan
	seen        map[string]int
	entry       bytes.Buffer
	rewritten   int
}

// member writes one top-level member. Members the filters don't touch are
// left unconsumed, so CopyObject copies them as they are.
func (s *streamWriter) member(m *jsonstream.Member) error {
	total, filtered := s.scan.occurrences[m.Key]
	if !filtered {
		return nil
	}
	s.seen[m.Key]++
	if s.seen[m.Key] < total {
		_, err := m.Raw() // an earlier duplicate, dropped
		return err
	}
	switch {
	case m.Key == "versions":
		return m.Object(s.version)
	case m.Key == "time" && s.timeChanged:
		return m.Object(s.time)
	case m.Key == "dist-tags" && s.tagsChanged:
		if _, err := m.Raw(); err != nil {
			return err
		}
		encoded, err := json.Marshal(s.distTags)
		if err != nil {
			return err
		}
		return m.Write(encoded)
	}
	return nil
}

// version writes a kept version with its tarball pointing at the proxy.
func (s *streamWriter) version(v *jsonstream.Member) error {
	raw, err := v.Raw()
	if err != nil {
		return err
	}
	if _, keep := s.versions[v.Key]; !keep {
		return nil
	}
	s.entry.Reset()
	if s.writeNPMVersion(&s.entry, s.packageName, v.Key, raw) {
		s.rewritten++
	}
	return v.Write(s.entry.Bytes())
}

// time writes a time entry the filters kept.
func (s *streamWriter) time(t *jsonstream.Member) error {
	if _, keep := s.timeMap[t.Key]; keep {
		return t.Copy()
	}
	_, err := t.Raw()
	return err
}

// objectValue records value under name in metadata, as Rewrite's
// decodeObject does, and returns it when it is an object. An object is
// copied, since the filters edit it.
func (s *Scan) objectValue(metadata map[string]any, name string, value any) map[string]any {
	if s.occurrences[name] == 0 {
		return nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		metadata[name] = value
		return nil
	}
	object = maps.Clone(object)
	metadata[name] = object
	return object
}

func expectDelim(dec *json.Decoder, want json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok != want {
		return fmt.Errorf("npm metadata: expected %v, got %v", want, tok)
	}
	return nil
}

func objectKey(dec *json.Decoder) (string, error) {
	tok, err := dec.Token()
	if err != nil {
		return "", err
	}
	key, ok := tok.(string)
	if !ok {
		return "", fmt.Errorf("npm metadata: object key is %T, not a string", tok)
	}
	return key, nil
}

// objectKeys reads a value and returns its keys when it is an object.
func objectKeys(dec *json.Decoder) ([]string, bool, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, false, err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil, false, nil // a scalar, read whole by Token
	}
	var keys []string
	for dec.More() {
		if delim == '{' {
			key, err := objectKey(dec)
			if err != nil {
				return nil, false, err
			}
			keys = append(keys, key)
		}
		if err := skipValue(dec); err != nil {
			return nil, false, err
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, false, err
	}
	return keys, delim == '{', nil
}

// skipValue reads the next value without keeping it.
func skipValue(dec *json.Decoder) error {
	var raw json.RawMessage
	return dec.Decode(&raw)
}

// VersionStream is Version for a packument too large to hold in memory. scan
// is the result of ScanPackument over the bytes r reads again; the second
// pass only looks for the one version entry it serves. For a document
// without a versions object it returns ErrNotStreamable.
func VersionStream(packageName, specifier string, scan *Scan, r io.Reader, options Options) ([]byte, error) {
	if !scan.versionsObject || len(options.LocalVersions) > 0 {
		return nil, ErrNotStreamable
	}
	h := &transformer{options}
	f := scan.filter(h, packageName)

	target := specifier
	if tag, ok := f.distTags[specifier].(string); ok {
		target = tag
	}
	if options.DeniedVersions[target] {
		return nil, ErrVersionDenied
	}
	if _, kept := f.versions[target]; !kept {
		return nil, ErrVersionNotFound
	}
	entry, err := LookupVersion(r, target)
	if err != nil {
		return nil, err
	}
	if entry.Entry == nil {
		return nil, ErrVersionNotFound
	}
	var out bytes.Buffer
	h.writeNPMVersion(&out, packageName, target, entry.Entry)
	return out.Bytes(), nil
}

// VersionEntry is what a packument holds for one version.
type VersionEntry struct {
	// Entry is versions[version], or nil when there is none.
	Entry json.RawMessage
	// Published is time[version], or "" when it is missing or not a string.
	Published string
	// Abbreviated is set for an abbreviated packument (see Abbreviated).
	Abbreviated bool
}

// LookupVersion reads a packument once and returns what it holds for
// version, keeping no other version's entry. Of repeated keys the last one
// counts, as with JSON.parse.
func LookupVersion(r io.Reader, version string) (VersionEntry, error) {
	var e VersionEntry
	var hasTime, hasModified bool
	dec := json.NewDecoder(r)
	if err := expectDelim(dec, '{'); err != nil {
		return e, err
	}
	for dec.More() {
		key, err := objectKey(dec)
		if err != nil {
			return e, err
		}
		switch key {
		case "versions":
			e.Entry, err = objectMember(dec, version)
		case "time":
			hasTime = true
			var raw json.RawMessage
			raw, err = objectMember(dec, version)
			e.Published = ""
			if err == nil && raw != nil {
				_ = json.Unmarshal(raw, &e.Published)
			}
		default:
			hasModified = hasModified || key == "modified"
			err = skipValue(dec)
		}
		if err != nil {
			return e, err
		}
	}
	if err := expectDelim(dec, '}'); err != nil {
		return e, err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			err = errors.New("unexpected data after the top-level object")
		}
		return e, err
	}
	e.Abbreviated = hasModified && !hasTime
	return e, nil
}

// objectMember reads a value and returns its member key when it is an object
// that has one, the last if repeated, or nil.
func objectMember(dec *json.Decoder, key string) (json.RawMessage, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil, nil
	}
	var found json.RawMessage
	for dec.More() {
		var k string
		if delim == '{' {
			if k, err = objectKey(dec); err != nil {
				return nil, err
			}
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		if delim == '{' && k == key {
			found = raw
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return found, nil
}
