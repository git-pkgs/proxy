// Package composer transforms Composer repository metadata without storage or
// network access.
package composer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/proxy/internal/jsonscan"
	"github.com/git-pkgs/proxy/internal/packageurl"
)

const (
	// Minified is the "minified" value of Composer 2 metadata, whose
	// version lists only carry the fields that change.
	Minified = "composer/2.0"
	// DevReset starts over the expanded fields in a minified list, before
	// the dev versions.
	DevReset           = "~dev"
	unset              = "__unset"
	vendorPackageParts = 2
)

var unsetJSON = []byte(`"` + unset + `"`)

// CooldownPolicy evaluates version age at the caller's supplied time.
type CooldownPolicy interface {
	Enabled() bool
	Evaluate(ecosystem, packagePURL string, publishedAt, evaluatedAt time.Time) cooldown.Decision
}

// LocalVersion is a pushed version as Composer metadata lists it.
type LocalVersion struct {
	Version string
	Entry   json.RawMessage
}

// Options supplies the public URL, policy and pushed versions for a rewrite.
// EvaluatedAt is used as given; the transformation never reads the clock.
type Options struct {
	ProxyURL    string
	Cooldown    CooldownPolicy
	EvaluatedAt time.Time
	// LocalPackage and LocalVersions are the pushed versions to list.
	LocalPackage  string
	LocalVersions []LocalVersion
	// Routed reports whether a package is served from a route rather than
	// the default repository; its versions don't report installs to the
	// default repository.
	Routed func(packageName string) bool
	// OnCooldownFilter is called synchronously for each version cooldown
	// holds back, with its publish time.
	OnCooldownFilter func(packageName, version string, publishedAt time.Time)
}

// Dist is the upstream archive of a version: its URL, type and shasum, and
// the raw publish time the cooldown is checked against.
type Dist struct {
	URL, Type, Shasum string
	Published         []byte
}

// Rewrite filters the versions cooldown holds back and points each dist at
// the proxy, returning the kept package names and versions without expanding
// minified lists. The pushed versions in o.LocalVersions are listed under
// o.LocalPackage in place of any upstream entry with the same version;
// cooldown only filters upstream versions. A document that does not list
// o.LocalPackage at all is replaced by one holding only the pushed versions.
func Rewrite(body []byte, o Options) ([]byte, []string, error) {
	localPackage, local := o.LocalPackage, o.LocalVersions
	if !json.Valid(body) {
		return nil, nil, errors.New("composer metadata is not valid JSON")
	}
	format, _, err := jsonscan.LookupString(body, "minified")
	if err != nil {
		return nil, nil, err
	}
	minified := format == Minified

	var out bytes.Buffer
	var kept []string
	listed := false
	out.Grow(len(body) + len(body)/8)
	err = jsonscan.RewriteMembers(&out, body, func(out *bytes.Buffer, m jsonscan.Member) (bool, error) {
		packages := m.Value(body)
		if !jsonscan.KeyIs(m.Key(body), "packages") || packages[0] != '{' {
			return false, nil
		}
		return true, jsonscan.RewriteMembers(out, packages, func(out *bytes.Buffer, p jsonscan.Member) (bool, error) {
			versions := p.Value(packages)
			if versions[0] != '[' {
				return false, nil
			}
			packageName, err := jsonscan.Key(p.Key(packages))
			if err != nil {
				return false, err
			}
			var pushed []LocalVersion
			if packageName == localPackage {
				pushed, listed = local, true
			}
			return true, o.writeVersions(out, packageName, versions, minified, &kept, pushed)
		})
	})
	if err != nil {
		return nil, nil, err
	}
	if len(local) > 0 && !listed {
		document, err := LocalDocument(localPackage, local)
		return document, nil, err
	}
	return out.Bytes(), kept, nil
}

// writeVersions writes one package's version list without the versions in
// cooldown and with each dist pointing at this proxy, and appends each version
// it writes to kept as its package name and version.
//
// Minified lists stay minified. Each version written carries the fields that
// differ from what the client has expanded so far, which is the upstream
// entry itself unless filtered versions were dropped before it; then it also
// carries the changes those versions made. Every version gets its own dist,
// since the proxied URL names the version.
//
// The pushed versions in local follow the upstream ones and take the place of
// upstream entries with the same version.
func (o Options) writeVersions(out *bytes.Buffer, packageName string, versions []byte, minified bool, kept *[]string, local []LocalVersion) error {
	packagePURL := packageurl.MakeString("composer", packageName, "")
	upstream, written := newFields(), newFields()
	devReset, first := false, true
	routed := o.Routed != nil && o.Routed(packageName)

	out.WriteByte('[')
	err := jsonscan.EachElement(versions, func(entry []byte) error {
		if minified && jsonscan.StringIs(entry, DevReset) {
			upstream.reset()
			written.reset()
			devReset = true
			return nil
		}
		if entry[0] != '{' {
			return nil
		}
		if !minified {
			upstream.reset()
			written.reset()
		}
		if err := upstream.apply(entry, minified); err != nil {
			return err
		}

		version := upstream.string("version")
		if PushedVersion(local, version) || o.heldBack(packagePURL, packageName, version, upstream.get("time")) {
			return nil
		}

		if !first {
			out.WriteByte(',')
		}
		first = false
		if devReset {
			out.WriteString(`"` + DevReset + `",`)
			devReset = false
		}
		if routed {
			disableDefaultNotification(upstream)
		}
		o.writeVersion(out, packageName, version, upstream, written)
		*kept = append(*kept, packageName+"\x00"+version)
		return nil
	})
	if err != nil {
		return err
	}
	for _, e := range local {
		pushed := newFields()
		if err := pushed.apply(e.Entry, false); err != nil {
			return err
		}
		if !minified {
			written.reset()
		}
		if !first {
			out.WriteByte(',')
		}
		first = false
		if devReset {
			out.WriteString(`"` + DevReset + `",`)
			devReset = false
		}
		o.writeVersion(out, packageName, e.Version, pushed, written)
		*kept = append(*kept, packageName+"\x00"+e.Version)
	}
	out.WriteByte(']')
	return nil
}

// writeVersion writes the fields of upstream that differ from written, with
// dist pointing at this proxy, and "__unset" for those written has and
// upstream lacks. written is then what the client expands this version to.
func (o Options) writeVersion(out *bytes.Buffer, packageName, version string, upstream, written *fields) {
	out.WriteByte('{')
	first := true
	writeMember := func(key, value []byte) {
		if !first {
			out.WriteByte(',')
		}
		first = false
		out.Write(key)
		out.WriteByte(':')
		out.Write(value)
	}

	for _, f := range upstream.fields {
		value := f.value
		if value == nil {
			continue
		}
		if f.name == "dist" {
			value = o.rewriteDist(packageName, version, value)
		}
		if bytes.Equal(value, written.get(f.name)) {
			continue
		}
		writeMember(f.key, value)
		written.set(f.name, f.key, value)
	}
	for _, f := range written.fields {
		if f.value != nil && upstream.get(f.name) == nil {
			writeMember(f.key, unsetJSON)
			written.set(f.name, f.key, nil)
		}
	}
	out.WriteByte('}')
}

// heldBack reports whether cooldown holds a version back.
func (o Options) heldBack(packagePURL, packageName, version string, published []byte) bool {
	if o.Cooldown == nil || !o.Cooldown.Enabled() {
		return false
	}
	publishedAt, ok := PublishTime(published)
	if !ok || o.Cooldown.Evaluate("composer", packagePURL, publishedAt, o.EvaluatedAt).Allowed {
		return false
	}
	if o.OnCooldownFilter != nil {
		o.OnCooldownFilter(packageName, version, publishedAt)
	}
	return true
}

// PublishTime parses the raw "time" field of a version, reporting false when
// it is missing or not an RFC 3339 string.
func PublishTime(published []byte) (time.Time, bool) {
	var timeStr string
	if err := json.Unmarshal(published, &timeStr); err != nil {
		return time.Time{}, false
	}
	publishedAt, err := time.Parse(time.RFC3339, timeStr)
	return publishedAt, err == nil
}

// disableDefaultNotification stops Composer from reporting installs of a routed
// package to the default repository's notify-batch URL, which would disclose
// the package name to it. Composer only falls back to notify-batch when a
// version has no notification-url of its own, and skips an empty one. Setting
// it on the expanded fields keeps it set for the versions that inherit them.
// Composer reads a null notification-url as absent too.
func disableDefaultNotification(fields *fields) {
	if v := fields.get("notification-url"); v == nil || string(v) == "null" {
		fields.set("notification-url", []byte(`"notification-url"`), []byte(`""`))
	}
}

// rewriteDist returns a version's dist object with its url pointing at this
// proxy. A dist without a string url is returned unchanged.
func (o Options) rewriteDist(packageName, version string, dist []byte) []byte {
	urlMember, ok, err := jsonscan.FindMember(dist, "url")
	if err != nil || !ok || dist[urlMember.ValStart] != '"' {
		return dist
	}
	var upstreamURL string
	if err := json.Unmarshal(urlMember.Value(dist), &upstreamURL); err != nil {
		return dist
	}
	distType, _, _ := jsonscan.LookupString(dist, "type")

	proxyURL, ok := ProxyDistURL(o.ProxyURL, packageName, version, upstreamURL, distType)
	if !ok {
		return dist
	}
	encoded, err := json.Marshal(proxyURL)
	if err != nil {
		return dist
	}
	rewritten := make([]byte, 0, len(dist)-len(urlMember.Value(dist))+len(encoded))
	rewritten = append(rewritten, dist[:urlMember.ValStart]...)
	rewritten = append(rewritten, encoded...)
	return append(rewritten, dist[urlMember.ValEnd:]...)
}

// ProxyDistURL returns the URL the proxy at proxyURL serves a version's
// archive at, or false when the upstream URL or package name gives nothing to
// build it from.
func ProxyDistURL(proxyURL, packageName, version, upstreamURL, distType string) (string, bool) {
	if upstreamURL == "" {
		return "", false
	}
	parts := strings.SplitN(packageName, "/", vendorPackageParts)
	if len(parts) != vendorPackageParts {
		return "", false
	}
	return fmt.Sprintf("%s/composer/files/%s/%s/%s/%s",
		proxyURL, parts[0], parts[1], url.PathEscape(version), DistFilename(upstreamURL, distType)), true
}

// DistFilename returns the file name the proxy serves an archive under: the
// last segment of its upstream URL.
func DistFilename(upstreamURL, distType string) string {
	filename := "package.zip"
	if idx := strings.LastIndex(upstreamURL, "/"); idx >= 0 {
		filename = upstreamURL[idx+1:]
	}

	// GitHub zipball URLs end with a bare commit hash (no extension).
	// Append .zip so the archives library can detect the format.
	if path.Ext(filename) == "" && distType == "zip" {
		filename += ".zip"
	}
	return filename
}

// commitPattern matches a full git commit hash, SHA-1 or SHA-256.
var commitPattern = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// IsCommit reports whether s is a full git commit hash, SHA-1 or SHA-256.
func IsCommit(s string) bool {
	return commitPattern.MatchString(s)
}

// UpstreamDistURL returns the upstream URL of the archive served as filename,
// given the dist URL the version's metadata lists now. A branch version only
// lists its current head while lock files pin older commits, so for a branch
// a dist URL ending in a commit hash is rebuilt for the requested commit. A
// tagged release has to ask for the listed name. Any other name gives false:
// fetching the listed archive would cache it under a name it does not have.
func UpstreamDistURL(listedURL, distType, filename string, branch bool) (string, bool) {
	listed := DistFilename(listedURL, distType)
	// Proxy versions before the .zip suffix handed out bare commit names,
	// and lock files written back then still ask for them.
	if bare := DistFilename(listedURL, ""); listed != bare && path.Ext(filename) == "" {
		filename += listed[len(bare):]
	}
	if filename == listed {
		return listedURL, true
	}
	if commit, ok := ShaQueryCommit(listed); ok {
		requested, ok := ShaQueryCommit(filename)
		listedSha, requestedSha := "sha="+commit, "sha="+requested
		idx := strings.LastIndex(listed, listedSha)
		if !branch || !ok || idx < 0 || listed[:idx]+requestedSha+listed[idx+len(listedSha):] != filename {
			return "", false
		}
		idx = strings.LastIndex(listedURL, listedSha)
		return listedURL[:idx] + requestedSha + listedURL[idx+len(listedSha):], true
	}
	ext := path.Ext(listed)
	commit := strings.TrimSuffix(listed, ext)
	requested, ok := strings.CutSuffix(filename, ext)
	if !branch || !ok || !commitPattern.MatchString(commit) || !commitPattern.MatchString(requested) {
		return "", false
	}
	idx := strings.LastIndex(listedURL, commit)
	return listedURL[:idx] + requested + listedURL[idx+len(commit):], true
}

// ShaQueryCommit returns the commit an archive name passes in a sha query
// parameter. GitLab names the archive of every commit archive.zip and picks
// the commit that way.
func ShaQueryCommit(filename string) (string, bool) {
	_, query, ok := strings.Cut(filename, "?")
	if !ok {
		return "", false
	}
	values, err := url.ParseQuery(query)
	if err != nil {
		return "", false
	}
	commit := values.Get("sha")
	return commit, commitPattern.MatchString(commit)
}

// FindDist returns the upstream dist of version from Composer metadata, with
// an empty URL when the package has no such version or it has no dist URL. It
// walks the version list in place, expanding only as much of the minified
// format as it needs, since it runs on each archive the proxy
// has not cached yet.
func FindDist(body []byte, packageName, version string) (Dist, error) {
	format, _, err := jsonscan.LookupString(body, "minified")
	if err != nil {
		return Dist{}, err
	}
	versions, err := jsonscan.Lookup(body, "packages", packageName)
	if err != nil || len(versions) == 0 || versions[0] != '[' {
		return Dist{}, err
	}
	minified := format == Minified

	fields := newFields()
	var dist Dist
	err = jsonscan.EachElement(versions, func(entry []byte) error {
		if minified && jsonscan.StringIs(entry, DevReset) {
			fields.reset()
			return nil
		}
		if entry[0] != '{' {
			return nil
		}
		if !minified {
			fields.reset()
		}
		if err := fields.apply(entry, minified); err != nil {
			return err
		}
		if !jsonscan.StringIs(fields.get("version"), version) {
			return nil
		}
		distRaw := fields.get("dist")
		dist.URL, _, _ = jsonscan.LookupString(distRaw, "url")
		if dist.URL != "" {
			dist.Type, _, _ = jsonscan.LookupString(distRaw, "type")
			dist.Shasum, _, _ = jsonscan.LookupString(distRaw, "shasum")
			dist.Published = fields.get("time")
			return jsonscan.ErrStop
		}
		return nil
	})
	if errors.Is(err, jsonscan.ErrStop) {
		err = nil
	}
	return dist, err
}

// PushedVersion reports whether version is one of the pushed ones. Tags are
// listed with and without a leading v, and both name one version.
func PushedVersion(local []LocalVersion, version string) bool {
	for _, e := range local {
		if strings.TrimPrefix(e.Version, "v") == strings.TrimPrefix(version, "v") {
			return true
		}
	}
	return false
}

// LocalDocument is a metadata document listing only the pushed versions.
func LocalDocument(packageName string, local []LocalVersion) ([]byte, error) {
	entries := make([]json.RawMessage, 0, len(local))
	for _, e := range local {
		entries = append(entries, e.Entry)
	}
	return json.Marshal(map[string]any{"packages": map[string]any{packageName: entries}})
}

// fields holds one version's fields as Composer sees them after
// expanding the minified format: raw values under their decoded names, in the
// order the names first appeared. A removed field keeps its slot with a nil
// value, so the order stays stable when it comes back.
type fields struct {
	fields []field
	index  map[string]int
}

type field struct {
	name       string
	key, value []byte
}

func newFields() *fields {
	return &fields{index: map[string]int{}}
}

func (f *fields) reset() {
	f.fields = f.fields[:0]
	clear(f.index)
}

// get returns the raw value of the named field, or nil when it is not set.
func (f *fields) get(name string) []byte {
	if i, ok := f.index[name]; ok {
		return f.fields[i].value
	}
	return nil
}

// string returns the named field when it is a string, or "".
func (f *fields) string(name string) string {
	raw := f.get(name)
	if len(raw) == 0 || raw[0] != '"' {
		return ""
	}
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

// set stores value under name, written with the quoted key. A nil value
// removes the field.
func (f *fields) set(name string, key, value []byte) {
	if i, ok := f.index[name]; ok {
		f.fields[i].key, f.fields[i].value = key, value
		return
	}
	f.index[name] = len(f.fields)
	f.fields = append(f.fields, field{name: name, key: key, value: value})
}

// apply overlays the members of a version entry. In minified metadata the
// value "__unset" removes the field instead.
func (f *fields) apply(entry []byte, minified bool) error {
	return jsonscan.EachMember(entry, func(m jsonscan.Member) error {
		key, value := m.Key(entry), m.Value(entry)
		if minified && jsonscan.StringIs(value, unset) {
			value = nil
		}
		if bytes.IndexByte(key, '\\') < 0 {
			if i, ok := f.index[string(key[1:len(key)-1])]; ok {
				f.fields[i].key, f.fields[i].value = key, value
				return nil
			}
		}
		name, err := jsonscan.Key(key)
		if err != nil {
			return err
		}
		f.set(name, key, value)
		return nil
	})
}
