// Package pypi transforms PyPI metadata without storage or network access.
package pypi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/proxy/internal/packageurl"
	"golang.org/x/net/html"
)

const minSubmatchParts = 2 // full match + first capture group

// CooldownPolicy evaluates release age at the caller's supplied time.
type CooldownPolicy interface {
	Enabled() bool
	Evaluate(ecosystem, packagePURL string, publishedAt, evaluatedAt time.Time) cooldown.Decision
}

// Options supplies the public and download URLs, the policy and the callbacks
// for a rewrite. EvaluatedAt is used as given; the transformation never reads
// the clock.
type Options struct {
	// ProxyURL is the proxy's public URL; DownloadURL is the host files are
	// served from upstream, such as https://files.pythonhosted.org.
	ProxyURL    string
	DownloadURL string
	Cooldown    CooldownPolicy
	EvaluatedAt time.Time
	// Denied reports whether the denylist names a version of a package.
	Denied func(name, version string) bool
	// Optional callbacks report changes synchronously. OnFiltered is called
	// for each release the JSON API rewrite leaves out, OnWithheld for each
	// of those that cooldown held back, with its newest upload time.
	OnFiltered func(name, version string)
	OnWithheld func(name, version string, publishedAt time.Time)
}

var (
	// simpleLinkRe matches a whole anchor tag: <a ...href="...">filename</a>.
	simpleLinkRe = regexp.MustCompile(`<a[^>]+href="[^"]*"[^>]*>[^<]+</a>`)
	// simpleLinkTextRe captures an anchor's text.
	simpleLinkTextRe = regexp.MustCompile(`>([^<]+)</a>`)
)

// RewriteSimpleHTML rewrites package URLs in simple API HTML to point at the
// proxy. Links for the versions in filteredVersions are removed entirely.
func RewriteSimpleHTML(body []byte, filteredVersions map[string]bool, o Options) []byte {
	if len(filteredVersions) > 0 {
		filteredVersions = canonicalVersions(filteredVersions)
		body = simpleLinkRe.ReplaceAllFunc(body, func(match []byte) []byte {
			innerMatch := simpleLinkTextRe.FindSubmatch(match)
			if len(innerMatch) < minSubmatchParts {
				return match
			}
			_, version := FilenameIdentity(strings.TrimSpace(string(innerMatch[1])))
			if version != "" && filteredVersions[CanonicalVersion(version)] {
				return nil
			}
			return match
		})
	}
	return RewriteDownloadHrefs(body, o)
}

// RewriteDownloadHrefs points href attributes for files on the download host
// at the proxy. It makes the same replacements as the regular expression
// `href="(<download URL>/packages/[^"]+)"` would: an href whose URL starts
// with the download host's /packages/ and has at least one more character
// before the closing quote. Finding the literal prefix is many times faster
// than running the regular expression over every link.
func RewriteDownloadHrefs(body []byte, o Options) []byte {
	from := []byte(`href="` + o.DownloadURL + `/packages/`)
	// The download route is /pypi/packages/{path...} and keeps the path
	// from the download host, /packages/ included.
	to := []byte(`href="` + o.ProxyURL + `/pypi/packages/packages/`)

	var out []byte
	done := 0 // body[:done] is in out already
	for search := 0; ; {
		i := bytes.Index(body[search:], from)
		if i < 0 {
			break
		}
		start := search + i
		rest := start + len(from)
		end := bytes.IndexByte(body[rest:], '"')
		if end <= 0 {
			// No closing quote, or nothing before it: not a match. The
			// regular expression would go on looking from the next byte.
			search = start + 1
			continue
		}
		if out == nil {
			out = make([]byte, 0, len(body)+len(body)/8)
		}
		out = append(out, body[done:start]...)
		out = append(out, to...)
		done = rest
		search = rest + end + 1
	}
	if out == nil {
		return body
	}
	return append(out, body[done:]...)
}

// FilterSimpleHTMLLinks removes the links to files of the given versions,
// read from their hrefs rather than their display text. Tokenization handles
// single quotes, entity escaping and nested link text without reserializing
// the rest of the index.
func FilterSimpleHTMLLinks(body []byte, versions map[string]bool) []byte {
	versions = canonicalVersions(versions)
	if len(versions) == 0 {
		return body
	}
	var result bytes.Buffer
	z := html.NewTokenizer(bytes.NewReader(body))
	skip := false
	for {
		tokenType := z.Next()
		if tokenType == html.ErrorToken {
			return result.Bytes()
		}
		raw := append([]byte(nil), z.Raw()...)
		if tokenType == html.StartTagToken || tokenType == html.SelfClosingTagToken {
			token := z.Token()
			if token.Data == "a" {
				skip = deniedSimpleLink(token, versions)
			}
		} else if tokenType == html.EndTagToken && z.Token().Data == "a" {
			if skip {
				skip = false
				continue
			}
		}
		if !skip {
			result.Write(raw)
		}
	}
}

func deniedSimpleLink(token html.Token, versions map[string]bool) bool {
	for _, attr := range token.Attr {
		if attr.Key != "href" {
			continue
		}
		u, err := url.Parse(attr.Val)
		if err != nil {
			continue
		}
		_, version := FilenameIdentity(path.Base(u.Path))
		if versions[CanonicalVersion(version)] {
			return true
		}
	}
	return false
}

// RewriteSimpleJSON rewrites a PEP 691 simple API document: file URLs point
// at the proxy, and the files and versions of filteredVersions are left out.
func RewriteSimpleJSON(body []byte, filteredVersions map[string]bool, o Options) ([]byte, error) {
	var metadata map[string]any
	if err := json.Unmarshal(body, &metadata); err != nil {
		return nil, err
	}

	files, ok := metadata["files"].([]any)
	if !ok {
		return nil, errors.New("pypi simple json response has no files array")
	}
	filteredVersions = canonicalVersions(filteredVersions)

	rewrittenFiles := make([]any, 0, len(files))
	for _, file := range files {
		entry, ok := file.(map[string]any)
		if !ok {
			rewrittenFiles = append(rewrittenFiles, file)
			continue
		}

		if filename, ok := entry["filename"].(string); ok {
			_, version := FilenameIdentity(filename)
			if version != "" && filteredVersions[CanonicalVersion(version)] {
				continue
			}
		}

		rewriteURLEntry(entry, o)
		rewrittenFiles = append(rewrittenFiles, entry)
	}

	metadata["files"] = rewrittenFiles

	// The versions list (PEP 700) loses the filtered versions too, whose
	// files were all left out above.
	if versions, ok := metadata["versions"].([]any); ok && len(filteredVersions) > 0 {
		keptVersions := make([]any, 0, len(versions))
		for _, v := range versions {
			if version, ok := v.(string); ok && filteredVersions[CanonicalVersion(version)] {
				continue
			}
			keptVersions = append(keptVersions, v)
		}
		metadata["versions"] = keptVersions
	}
	return json.Marshal(metadata)
}

// RewriteJSON rewrites a JSON API document, a package's or one version's:
// releases the denylist names or cooldown holds back are left out, and file
// URLs point at the proxy. It also returns what it kept: the releases, and
// the current version's files unless they were dropped.
func RewriteJSON(body []byte, o Options) ([]byte, []string, error) {
	var metadata map[string]any
	if err := json.Unmarshal(body, &metadata); err != nil {
		return nil, nil, err
	}

	packageName, _ := documentName(metadata)
	packagePURL := ""
	if packageName != "" {
		packagePURL = packageurl.MakeString("pypi", packageName, "")
	}

	filterAndRewriteReleases(metadata, packageName, packagePURL, o)
	filterAndRewriteURLs(metadata, packagePURL, o)

	var kept []string
	if releases, ok := metadata["releases"].(map[string]any); ok {
		kept = slices.Collect(maps.Keys(releases))
	}
	if urls, ok := metadata["urls"].([]any); ok && len(urls) > 0 {
		// Not a valid release key, so it can't be confused with one.
		kept = append(kept, "\x00urls")
	}

	out, err := json.Marshal(metadata)
	return out, kept, err
}

// filterAndRewriteReleases applies the denylist and cooldown to the releases
// map and rewrites the URLs of the files it keeps.
func filterAndRewriteReleases(metadata map[string]any, packageName, packagePURL string, o Options) {
	releases, ok := metadata["releases"].(map[string]any)
	if !ok {
		return
	}

	for version, files := range releases {
		denied := o.denied(packageName, version)
		held, publishedAt := false, time.Time{}
		if !denied {
			held, publishedAt = heldBack(packagePURL, files, o)
		}
		if denied || held {
			if o.OnFiltered != nil {
				o.OnFiltered(packageName, version)
			}
			if held && o.OnWithheld != nil {
				o.OnWithheld(packageName, version, publishedAt)
			}
			delete(releases, version)
			continue
		}

		rewriteFileEntries(files, o)
	}
}

// filterAndRewriteURLs applies the denylist and cooldown to the urls array,
// the current version's files, and rewrites the URLs it keeps.
func filterAndRewriteURLs(metadata map[string]any, packagePURL string, o Options) {
	urls, ok := metadata["urls"].([]any)
	if !ok {
		return
	}

	name, version := documentNameVersion(metadata)
	if o.denied(name, version) {
		metadata["urls"] = []any{}
	} else if held, publishedAt := heldBack(packagePURL, urls, o); held {
		if o.OnWithheld != nil {
			o.OnWithheld(name, version, publishedAt)
		}
		metadata["urls"] = []any{}
	}

	if urls, ok := metadata["urls"].([]any); ok {
		for _, u := range urls {
			if umap, ok := u.(map[string]any); ok {
				rewriteURLEntry(umap, o)
			}
		}
	}
}

func (o Options) denied(name, version string) bool {
	return o.Denied != nil && o.Denied(name, version)
}

// heldBack reports whether cooldown holds a release back, and the newest
// upload time that decided it.
func heldBack(packagePURL string, files any, o Options) (bool, time.Time) {
	if o.Cooldown == nil || !o.Cooldown.Enabled() || packagePURL == "" {
		return false, time.Time{}
	}
	filesArr, ok := files.([]any)
	if !ok {
		return false, time.Time{}
	}
	publishedAt := newestUploadTime(filesArr)
	return !publishedAt.IsZero() && !o.Cooldown.Evaluate("pypi", packagePURL, publishedAt, o.EvaluatedAt).Allowed, publishedAt
}

func rewriteFileEntries(files any, o Options) {
	filesArr, ok := files.([]any)
	if !ok {
		return
	}
	for _, f := range filesArr {
		if fmap, ok := f.(map[string]any); ok {
			rewriteURLEntry(fmap, o)
		}
	}
}

// documentName returns the package name a JSON API document gives.
func documentName(metadata map[string]any) (string, bool) {
	info, ok := metadata["info"].(map[string]any)
	if !ok {
		return "", false
	}
	name, ok := info["name"].(string)
	return name, ok
}

func documentNameVersion(metadata map[string]any) (name, version string) {
	info, _ := metadata["info"].(map[string]any)
	name, _ = info["name"].(string)
	version, _ = info["version"].(string)
	return name, version
}

// newestUploadTime returns the most recent upload_time_iso_8601 of a list of
// file entries.
func newestUploadTime(files []any) time.Time {
	var newest time.Time
	for _, f := range files {
		fmap, ok := f.(map[string]any)
		if !ok {
			continue
		}
		ts, ok := fmap["upload_time_iso_8601"].(string)
		if !ok {
			continue
		}
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			continue
		}
		if t.After(newest) {
			newest = t
		}
	}
	return newest
}

// rewriteURLEntry points a file entry's URL on the download host at the proxy.
func rewriteURLEntry(entry map[string]any, o Options) {
	urlStr, ok := entry["url"].(string)
	if !ok {
		return
	}
	if strings.HasPrefix(urlStr, o.DownloadURL+"/packages/") {
		entry["url"] = o.ProxyURL + "/pypi/packages" + strings.TrimPrefix(urlStr, o.DownloadURL)
	}
}

// releaseFiles is the part of a JSON API document the release times are
// read from. Decoding into it skips every other field, where a generic
// map would hold the whole document, several times its size, in memory.
type releaseFiles struct {
	Releases map[string][]struct {
		UploadTime string `json:"upload_time_iso_8601"`
	} `json:"releases"`
}

// ReleaseTimes returns each release's newest upload time from a JSON API
// document, zero for a release without one.
func ReleaseTimes(body []byte) (map[string]time.Time, error) {
	var metadata releaseFiles
	// A value of another type than expected is skipped and the rest is
	// still decoded, so a file entry without a usable time doesn't count.
	var mismatch *json.UnmarshalTypeError
	if err := json.Unmarshal(body, &metadata); err != nil && !errors.As(err, &mismatch) {
		return nil, fmt.Errorf("parsing pypi json metadata: %w", err)
	}
	if metadata.Releases == nil {
		return nil, errors.New("pypi json metadata has no releases")
	}

	times := make(map[string]time.Time, len(metadata.Releases))
	for version, files := range metadata.Releases {
		var newest time.Time
		for _, file := range files {
			if t, err := time.Parse(time.RFC3339, file.UploadTime); err == nil && t.After(newest) {
				newest = t
			}
		}
		times[version] = newest
	}
	return times, nil
}
