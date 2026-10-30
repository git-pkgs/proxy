// Package npm transforms registry metadata without storage or network access.
package npm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/proxy/internal/jsonscan"
	"github.com/git-pkgs/proxy/internal/packageurl"
)

var ErrVersionDenied = errors.New("package version blocked by denylist")
var ErrVersionNotFound = errors.New("version not found")

// CooldownPolicy evaluates version age at the caller's supplied time.
type CooldownPolicy interface {
	Enabled() bool
	Evaluate(ecosystem, packagePURL string, publishedAt, evaluatedAt time.Time) cooldown.Decision
}

// LocalVersion is an already approved hosted version and its publication time.
type LocalVersion struct {
	Version     string
	Entry       json.RawMessage
	PublishedAt string
}

// Options supplies the public URL, policy and hosted versions for a rewrite.
// EvaluatedAt is used as given; the transformation never reads the clock.
type Options struct {
	ProxyURL       string
	Cooldown       CooldownPolicy
	DeniedVersions map[string]bool
	EvaluatedAt    time.Time
	LocalVersions  []LocalVersion
	// Optional callbacks report changes synchronously without retaining events.
	// OnTarballsRewritten is called at most once per document, with how many
	// tarball URLs it pointed at the proxy, and not at all when none.
	OnTarballsRewritten func(count int)
	OnCooldownFilter    func(version, publishedAt string)
}

type transformer struct{ Options }

// Rewrite filters upstream versions and points their tarball and attestation
// URLs at the proxy, preserving untouched JSON values verbatim. Hosted entries
// override upstream versions. The returned version set is unordered. The
// document, denylist and hosted entries are not modified.
func Rewrite(packageName string, body []byte, options Options) ([]byte, []string, error) {
	return (&transformer{options}).rewriteMetadataWithLocal(packageName, body, options.LocalVersions)
}

// Version resolves a version or dist-tag through the filtered packument.
func Version(packageName, specifier string, packument []byte, options Options) ([]byte, error) {
	filtered, _, err := Rewrite(packageName, packument, options)
	if err != nil {
		return nil, err
	}

	target := specifier
	if resolved, ok, err := jsonscan.LookupString(filtered, "dist-tags", specifier); err != nil {
		return nil, err
	} else if ok {
		target = resolved
	}

	if options.DeniedVersions[target] {
		return nil, ErrVersionDenied
	}

	versionRaw, err := jsonscan.Lookup(filtered, "versions", target)
	if err != nil {
		return nil, err
	}
	if len(versionRaw) == 0 {
		return nil, ErrVersionNotFound
	}

	var out bytes.Buffer
	(&transformer{options}).writeNPMVersion(&out, packageName, target, versionRaw)
	return out.Bytes(), nil
}

// Abbreviated reports whether body is an abbreviated packument: one with the
// top-level "modified" field the abbreviated format carries and no "time"
// map, which cooldown needs. A full document without a time map, as some
// registries serve, is not abbreviated, and neither is anything that is not
// a JSON object.
func Abbreviated(body []byte) bool {
	var hasTime, hasModified bool
	err := jsonscan.EachMember(body, func(m jsonscan.Member) error {
		switch key := m.Key(body); {
		case jsonscan.KeyIs(key, "time"):
			hasTime = true
		case jsonscan.KeyIs(key, "modified"):
			hasModified = true
		}
		return nil
	})
	return err == nil && hasModified && !hasTime
}

func (h *transformer) rewriteMetadataWithLocal(packageName string, body []byte, local []LocalVersion) ([]byte, []string, error) {
	doc, err := indexNPMPackument(body)
	if err != nil {
		return nil, nil, err
	}

	versionsRaw := doc.value(doc.versionsAt)
	if len(versionsRaw) == 0 || versionsRaw[0] != '{' {
		version, ok, err := jsonscan.LookupString(body, "version")
		if err != nil {
			return nil, nil, err
		}
		if ok {
			if h.DeniedVersions[version] {
				return nil, nil, ErrVersionDenied
			}
			var out bytes.Buffer
			if h.writeNPMVersion(&out, packageName, version, body) {
				h.tarballsRewritten(1)
			}
			return out.Bytes(), []string{version}, nil
		}
		if len(h.DeniedVersions) != 0 {
			return nil, nil, errors.New("npm metadata has no versions object")
		}
		return body, nil, nil // No versions to rewrite
	}

	// The filters only look at which versions exist, so they run on a map of
	// version names alongside the decoded time map and dist-tags.
	versions := map[string]any{}
	if err := jsonscan.EachMember(versionsRaw, func(m jsonscan.Member) error {
		version, err := jsonscan.Key(m.Key(versionsRaw))
		versions[version] = nil
		return err
	}); err != nil {
		return nil, nil, err
	}
	metadata := map[string]any{}
	timeMap := doc.decodeObject(doc.timeAt, metadata, "time")
	distTags := doc.decodeObject(doc.tagsAt, metadata, "dist-tags")
	timeEntries := len(timeMap)
	originalTags := maps.Clone(distTags)

	h.applyCooldownFiltering(metadata, versions, packageName)
	h.applyDenylistFiltering(metadata, versions)
	for _, e := range local {
		delete(versions, e.Version)
	}

	// Untouched members are copied as they are; the three the filters
	// handled are written from their filtered state, and only when it changed.
	out, err := doc.write(func(out *bytes.Buffer, i int) error {
		switch {
		case i == doc.versionsAt:
			return h.writeNPMVersions(out, packageName, versionsRaw, versions, local)
		case i == doc.timeAt && timeMap != nil && (len(timeMap) != timeEntries || len(local) > 0):
			pushedAt := make([][2]string, 0, len(local))
			for _, e := range local {
				pushedAt = append(pushedAt, [2]string{e.Version, e.PublishedAt})
			}
			return jsonscan.WriteFilteredObject(out, doc.value(i), func(key string) bool {
				_, ok := timeMap[key]
				pushed := slices.ContainsFunc(local, func(e LocalVersion) bool { return e.Version == key })
				return ok && !pushed
			}, pushedAt)
		case i == doc.tagsAt && distTags != nil && !reflect.DeepEqual(distTags, originalTags):
			encoded, err := json.Marshal(distTags)
			out.Write(encoded)
			return err
		default:
			out.Write(doc.value(i))
			return nil
		}
	})
	kept := slices.Collect(maps.Keys(versions))
	for _, e := range local {
		kept = append(kept, e.Version)
	}
	return out, kept, err
}

// npmPackument is a packument's top-level members as offsets into its bytes,
// with the positions of the members rewriteMetadata filters, or -1 for those
// it lacks. A key that repeats is recorded at its last occurrence, the one
// JSON.parse keeps.
type npmPackument struct {
	body                       []byte
	members                    []jsonscan.Member
	versionsAt, timeAt, tagsAt int
}

func indexNPMPackument(body []byte) (*npmPackument, error) {
	if !json.Valid(body) {
		return nil, errors.New("npm metadata is not valid JSON")
	}
	doc := &npmPackument{body: body, versionsAt: -1, timeAt: -1, tagsAt: -1}
	err := jsonscan.EachMember(body, func(m jsonscan.Member) error {
		switch key := m.Key(body); {
		case jsonscan.KeyIs(key, "versions"):
			doc.versionsAt = len(doc.members)
		case jsonscan.KeyIs(key, "time"):
			doc.timeAt = len(doc.members)
		case jsonscan.KeyIs(key, "dist-tags"):
			doc.tagsAt = len(doc.members)
		}
		doc.members = append(doc.members, m)
		return nil
	})
	return doc, err
}

// value returns the raw value of member i, or nil when i is -1.
func (d *npmPackument) value(i int) []byte {
	if i < 0 {
		return nil
	}
	return d.members[i].Value(d.body)
}

// decodeObject decodes member i into metadata[name] and returns it when it is
// an object. The filters edit the returned map in place.
func (d *npmPackument) decodeObject(i int, metadata map[string]any, name string) map[string]any {
	raw := d.value(i)
	if raw == nil {
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	metadata[name] = value
	object, _ := value.(map[string]any)
	return object
}

// write assembles the document in member order, with writeValue writing each
// member's value. Earlier duplicates of the filtered keys are dropped: they
// would carry unfiltered data, and JSON.parse ignores them anyway.
func (d *npmPackument) write(writeValue func(out *bytes.Buffer, i int) error) ([]byte, error) {
	var out bytes.Buffer
	out.Grow(len(d.body) + len(d.body)/8)
	out.WriteByte('{')
	first := true
	for i, m := range d.members {
		if i != d.versionsAt && i != d.timeAt && i != d.tagsAt {
			key := m.Key(d.body)
			if jsonscan.KeyIs(key, "versions") || jsonscan.KeyIs(key, "time") || jsonscan.KeyIs(key, "dist-tags") {
				continue
			}
		}
		if !first {
			out.WriteByte(',')
		}
		first = false
		out.Write(m.Key(d.body))
		out.WriteByte(':')
		if err := writeValue(&out, i); err != nil {
			return nil, err
		}
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

// writeNPMVersions writes the versions object, keeping the versions still in
// keep and pointing each kept version's tarball at this proxy, followed by the
// pushed versions in local.
func (h *transformer) writeNPMVersions(out *bytes.Buffer, packageName string, versionsRaw []byte, keep map[string]any, local []LocalVersion) error {
	out.WriteByte('{')
	first := true
	rewritten := 0
	err := jsonscan.EachMember(versionsRaw, func(m jsonscan.Member) error {
		version, err := jsonscan.Key(m.Key(versionsRaw))
		if err != nil {
			return err
		}
		if _, ok := keep[version]; !ok {
			return nil
		}
		if !first {
			out.WriteByte(',')
		}
		first = false
		out.Write(m.Key(versionsRaw))
		out.WriteByte(':')
		if h.writeNPMVersion(out, packageName, version, m.Value(versionsRaw)) {
			rewritten++
		}
		return nil
	})
	h.tarballsRewritten(rewritten)
	for _, e := range local {
		if !first {
			out.WriteByte(',')
		}
		first = false
		key, _ := json.Marshal(e.Version)
		out.Write(key)
		out.WriteByte(':')
		out.Write(e.Entry)
	}
	out.WriteByte('}')
	return err
}

// writeNPMVersion writes one version entry with its dist.tarball and
// dist.attestations.url pointing at this proxy, and reports whether it
// rewrote the tarball. Members that are missing or not strings are written
// unchanged.
func (h *transformer) writeNPMVersion(out *bytes.Buffer, packageName, version string, raw []byte) bool {
	dist, ok, err := jsonscan.FindMember(raw, "dist")
	if err != nil || !ok {
		out.Write(raw)
		return false
	}
	distRaw := dist.Value(raw)

	var buf [2]urlEdit
	edits := buf[:0]
	tarball, ok := stringMember(distRaw, "tarball")
	if ok {
		tarball.replacement = TarballURL(h.ProxyURL, packageName, version, tarball.old)
		edits = append(edits, tarball.shift(dist.ValStart))
	}
	if attestations, found, err := jsonscan.FindMember(distRaw, "attestations"); err == nil && found {
		if att, ok := stringMember(attestations.Value(distRaw), "url"); ok {
			att.replacement = AttestationsURL(h.ProxyURL, packageName, version)
			edits = append(edits, att.shift(dist.ValStart+attestations.ValStart))
		}
	}
	writeEdits(out, raw, edits)
	return tarball.replacement != ""
}

// urlEdit replaces the string value at raw[start:end], which decodes to old,
// with replacement.
type urlEdit struct {
	start, end  int
	old         string
	replacement string
}

func (e urlEdit) shift(offset int) urlEdit {
	e.start += offset
	e.end += offset
	return e
}

// stringMember finds the string member name in obj.
func stringMember(obj []byte, name string) (urlEdit, bool) {
	m, ok, err := jsonscan.FindMember(obj, name)
	if err != nil || !ok || obj[m.ValStart] != '"' {
		return urlEdit{}, false
	}
	var old string
	if err := json.Unmarshal(m.Value(obj), &old); err != nil {
		return urlEdit{}, false
	}
	return urlEdit{start: m.ValStart, end: m.ValEnd, old: old}, true
}

// writeEdits writes raw with the edits applied. There are at most two, which
// may come in either order.
func writeEdits(out *bytes.Buffer, raw []byte, edits []urlEdit) {
	if len(edits) == 2 && edits[1].start < edits[0].start {
		edits[0], edits[1] = edits[1], edits[0]
	}
	pos := 0
	for _, e := range edits {
		encoded, err := json.Marshal(e.replacement)
		if err != nil {
			continue
		}
		out.Write(raw[pos:e.start])
		out.Write(encoded)
		pos = e.end
	}
	out.Write(raw[pos:])
}

// tarballsRewritten reports once per document how many tarball URLs it
// rewrote. A report per version flooded debug logs: packages such as eslint
// list hundreds of versions, and with cooldown on every request rewrites the
// document again.
func (h *transformer) tarballsRewritten(count int) {
	if count > 0 && h.OnTarballsRewritten != nil {
		h.OnTarballsRewritten(count)
	}
}

// applyCooldownFiltering removes versions that are too recently published,
// and updates dist-tags.latest if the current latest was filtered out.
func (h *transformer) applyCooldownFiltering(metadata map[string]any, versions map[string]any, packageName string) {
	if h.Cooldown == nil || !h.Cooldown.Enabled() {
		return
	}

	timeMap, _ := metadata["time"].(map[string]any)
	if timeMap == nil {
		return
	}

	packagePURL := packageurl.MakeString("npm", packageName, "")

	for version := range versions {
		publishedStr, ok := timeMap[version].(string)
		if !ok {
			continue
		}
		publishedAt, err := time.Parse(time.RFC3339, publishedStr)
		if err != nil {
			continue
		}
		if !h.Cooldown.Evaluate("npm", packagePURL, publishedAt, h.EvaluatedAt).Allowed {
			if h.OnCooldownFilter != nil {
				h.OnCooldownFilter(version, publishedStr)
			}
			delete(versions, version)
			delete(timeMap, version)
		}
	}

	h.updateDistTagsLatest(metadata, versions, timeMap)
}

// updateDistTagsLatest updates the dist-tags.latest field if the current latest
// version was removed by cooldown filtering.
func (h *transformer) updateDistTagsLatest(metadata, versions, timeMap map[string]any) {
	distTags, ok := metadata["dist-tags"].(map[string]any)
	if !ok {
		return
	}

	latest, ok := distTags["latest"].(string)
	if !ok {
		return
	}

	if _, exists := versions[latest]; exists {
		return
	}

	if newLatest := h.findNewestVersion(versions, timeMap); newLatest != "" {
		distTags["latest"] = newLatest
	}
}

// TarballURL returns the proxy URL that serves the tarball upstream
// lists at tarball for this version.
func TarballURL(proxyURL, packageName, version, tarball string) string {
	filename := tarball
	if idx := strings.LastIndex(tarball, "/"); idx >= 0 {
		filename = tarball[idx+1:]
	}
	if VersionFromFilename(packageName, filename) != version {
		_, shortName, scoped := strings.Cut(packageName, "/")
		if !scoped {
			shortName = packageName
		}
		filename = shortName + "-" + version + ".tgz"
	}

	return fmt.Sprintf("%s/npm/%s/-/%s", proxyURL, url.PathEscape(packageName), filename)
}

// AttestationsURL returns the proxy URL that serves the attestations of a
// version, in the form the npm registry uses: the package name with its scope
// slash encoded, "@", and the version.
func AttestationsURL(proxyURL, packageName, version string) string {
	return fmt.Sprintf("%s/npm/-/npm/v1/attestations/%s@%s", proxyURL, url.PathEscape(packageName), url.PathEscape(version))
}

// findNewestVersion returns the version string with the most recent timestamp
// from the remaining versions, using the time map.
func (h *transformer) findNewestVersion(versions map[string]any, timeMap map[string]any) string {
	if timeMap == nil {
		return ""
	}

	type versionTime struct {
		version string
		t       time.Time
	}

	var vts []versionTime
	for v := range versions {
		if ts, ok := timeMap[v].(string); ok {
			if t, err := time.Parse(time.RFC3339, ts); err == nil {
				vts = append(vts, versionTime{v, t})
			}
		}
	}

	if len(vts) == 0 {
		return ""
	}

	sort.Slice(vts, func(i, j int) bool {
		return vts[i].t.After(vts[j].t)
	})

	return vts[0].version
}

// VersionFromFilename extracts a version from an npm tarball filename.
func VersionFromFilename(packageName, filename string) string {
	// Remove .tgz extension
	if !strings.HasSuffix(filename, ".tgz") {
		return ""
	}
	base := strings.TrimSuffix(filename, ".tgz")

	// For scoped packages, the filename uses the short name
	shortName := packageName
	if _, name, scoped := strings.Cut(packageName, "/"); scoped {
		shortName = name
	}

	// Expected format: {shortName}-{version}
	prefix := shortName + "-"
	if !strings.HasPrefix(base, prefix) {
		return ""
	}

	return strings.TrimPrefix(base, prefix)
}
