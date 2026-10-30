package handler

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"
)

// Maven cooldown measures a version by the Last-Modified header of its .pom.
// On Maven Central that is the time the version was deployed. The header is
// read once per version and stored in the versions table, since release
// files don't change.
//
// maven-metadata.xml has a single <lastUpdated> for the whole artifact, so
// the proxy only looks at versions that can still be in cooldown: none when
// <lastUpdated> is older than the window, and otherwise the versions from
// the end of the list back to the first one that is old enough. Maven
// Central blocks clients that send bursts of requests, so the proxy doesn't
// read every version's time.
//
// SNAPSHOT versions are left alone: their files change after publishing.

const errMavenCooldown = "version is in cooldown"

// mavenTimeUserAgent is the User-Agent of publish-time requests, the one
// the artifact fetcher sends.
const mavenTimeUserAgent = "git-pkgs-proxy/1.0"

const (
	mavenMetadataFile = "maven-metadata.xml"
	mavenSnapshot     = "-SNAPSHOT"
	// mavenLastUpdatedLayout is the UTC timestamp format of <lastUpdated>.
	mavenLastUpdatedLayout = "20060102150405"
)

// mavenChecksums are the checksum files served beside maven-metadata.xml.
// With cooldown on they are computed from the metadata as served.
var mavenChecksums = map[string]func() hash.Hash{
	".md5":    md5.New,
	".sha1":   sha1.New,
	".sha256": sha256.New,
	".sha512": sha512.New,
}

func (h *MavenHandler) cooldownEnabled() bool {
	return h.proxy.Cooldown != nil && h.proxy.Cooldown.Enabled()
}

func mavenPackagePURL(group, artifact string) string {
	return canonicalPackagePURL("maven", group+":"+artifact)
}

// mavenCoordinates returns the group and artifact of an artifact directory
// such as com/google/guava/guava.
func mavenCoordinates(artifactDir string) (group, artifact string, ok bool) {
	i := strings.LastIndex(artifactDir, "/")
	if i <= 0 || i == len(artifactDir)-1 {
		return "", "", false
	}
	return strings.ReplaceAll(artifactDir[:i], "/", "."), artifactDir[i+1:], true
}

// usableMavenVersion reports whether version can be put in a URL path as
// one segment. Versions come from upstream metadata.
func usableMavenVersion(version string) bool {
	return version != "" && version != "." && version != ".." && !strings.ContainsAny(version, "/\\?#%")
}

// mavenTimeKey returns the versions-table PURLs a version's time is stored
// under. A named repository's keys carry its upstream as repository_url:
// the same coordinates there can have been published at another time than
// in the default repository.
func (h *MavenHandler) mavenTimeKey(source mavenSource, group, artifact, version string) (versionPURL, packagePURL string, ok bool) {
	if h.proxy.DB == nil {
		return "", "", false
	}
	name := group + ":" + artifact
	versionPURL, packagePURL = canonicalVersionPURL("maven", name, version), canonicalPackagePURL("maven", name)
	if source.name != "" {
		qualifier := "?repository_url=" + url.QueryEscape(source.upstreamURL)
		versionPURL += qualifier
		packagePURL += qualifier
	}
	return versionPURL, packagePURL, true
}

// publishTime returns a version's publish time from the versions table, or
// from the Last-Modified of its .pom, which is then stored. A version
// without a .pom, or whose .pom has no Last-Modified, gets the zero time and
// is let through, as in the other ecosystems. A failed request is an error,
// so a rate-limited upstream can't let a version through.
func (h *MavenHandler) publishTime(ctx context.Context, source mavenSource, artifactDir, version string) (time.Time, error) {
	group, artifact, ok := mavenCoordinates(artifactDir)
	if !ok || !usableMavenVersion(version) {
		return time.Time{}, nil
	}
	versionPURL, packagePURL, store := h.mavenTimeKey(source, group, artifact, version)
	if store {
		if ver, err := h.proxy.DB.GetVersionByPURL(versionPURL); err == nil && ver != nil && ver.PublishedAt.Valid {
			return ver.PublishedAt.Time, nil
		}
	}

	pomPath := artifactDir + "/" + version + "/" + artifact + "-" + version + ".pom"
	published, found, err := h.pomLastModified(ctx, source.upstreamURL+"/"+pomPath)
	if err == nil && !found && source.fallbackURL != "" {
		published, _, err = h.pomLastModified(ctx, source.fallbackURL+"/"+pomPath)
	}
	if err != nil {
		return time.Time{}, err
	}
	if store && !published.IsZero() {
		if err := h.proxy.DB.SetVersionPublishedAt(versionPURL, packagePURL, published); err != nil {
			h.proxy.Logger.Warn("cooldown: could not store maven publish time",
				"group", group, "artifact", artifact, "version", version, "error", err)
		}
	}
	return published, nil
}

// cooldownFromDownload returns a context that stores the publish time of a
// version from the download of its .pom or Gradle .module, and whether the
// cooldown check is to run after the download. It does so only for those
// files of a version whose time is not stored yet; for anything else the
// check runs before. Both files are deployed with the version, and
// whichever a client fetches first gives its time.
func (h *MavenHandler) cooldownFromDownload(ctx context.Context, source mavenSource, urlPath, version string) (context.Context, bool) {
	if !h.cooldownEnabled() || strings.HasSuffix(version, mavenSnapshot) {
		return ctx, false
	}
	artifactDir := path.Dir(path.Dir(urlPath))
	group, artifact, ok := mavenCoordinates(artifactDir)
	if base := artifact + "-" + version; !ok || !usableMavenVersion(version) ||
		(path.Base(urlPath) != base+".pom" && path.Base(urlPath) != base+".module") {
		return ctx, false
	}
	versionPURL, packagePURL, store := h.mavenTimeKey(source, group, artifact, version)
	if !store {
		return ctx, false
	}
	if ver, err := h.proxy.DB.GetVersionByPURL(versionPURL); err == nil && ver != nil && ver.PublishedAt.Valid {
		return ctx, false
	}
	return WithUpstreamLastModified(ctx, func(published time.Time) {
		if err := h.proxy.DB.SetVersionPublishedAt(versionPURL, packagePURL, published); err != nil {
			h.proxy.Logger.Warn("cooldown: could not store maven publish time",
				"group", group, "artifact", artifact, "version", version, "error", err)
		}
	}), true
}

// pomLastModified sends a HEAD request for a .pom and returns its
// Last-Modified. found is false when upstream doesn't have the file. Only a
// 2xx response's header is used: an error page's date says nothing about the
// version.
func (h *MavenHandler) pomLastModified(ctx context.Context, pomURL string) (published time.Time, found bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, pomURL, nil)
	if err != nil {
		return time.Time{}, false, err
	}
	// Name the proxy as its artifact downloads do; Maven Central asks
	// tools not to send an HTTP library's default.
	req.Header.Set("User-Agent", mavenTimeUserAgent)
	resp, err := h.proxy.HTTPClient.Do(req)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("reading pom time: %w", err)
	}
	_ = resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return time.Time{}, false, nil
	case resp.StatusCode == http.StatusTooManyRequests && h.proxy.RateLimits != nil:
		// The gate has just noted the upstream's Retry-After; pass it on.
		if err := h.proxy.RateLimits.Held(pomURL); err != nil {
			return time.Time{}, false, err
		}
		return time.Time{}, false, fmt.Errorf("reading pom time: upstream returned %d", resp.StatusCode)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return time.Time{}, false, fmt.Errorf("reading pom time: upstream returned %d", resp.StatusCode)
	}
	published, err = http.ParseTime(resp.Header.Get(headerLastModified))
	if err != nil {
		return time.Time{}, true, nil
	}
	return published, true, nil
}

// checkCooldown answers a request for a file of a version in cooldown, or
// whose time couldn't be read, and reports whether it did. The answer is
// 403, as for a version on the denylist.
func (h *MavenHandler) checkCooldown(w http.ResponseWriter, r *http.Request, source mavenSource, urlPath, version string) bool {
	if !h.cooldownEnabled() || strings.HasSuffix(version, mavenSnapshot) {
		return false
	}
	artifactDir := path.Dir(path.Dir(urlPath))
	group, artifact, ok := mavenCoordinates(artifactDir)
	if !ok || h.proxy.Cooldown.For("maven", mavenPackagePURL(group, artifact)) == 0 {
		return false
	}
	published, err := h.publishTime(r.Context(), source, artifactDir, version)
	if err != nil {
		if writeUpstreamHeld(w, err) {
			return true
		}
		h.proxy.Logger.Warn("cooldown: could not read maven version time",
			"group", group, "artifact", artifact, "version", version, "error", err)
		http.Error(w, "failed to check cooldown", http.StatusBadGateway)
		return true
	}
	if !h.proxy.Cooldown.IsAllowed("maven", mavenPackagePURL(group, artifact), published) {
		h.proxy.Logger.Info("cooldown: withholding maven version",
			"group", group, "artifact", artifact, "version", version)
		http.Error(w, errMavenCooldown+": "+canonicalVersionPURL("maven", group+":"+artifact, version), http.StatusForbidden)
		return true
	}
	return false
}

// serveCooldownMetadata serves maven-metadata.xml without versions in
// cooldown, and its checksum files computed from what is served. It reports
// false for other files beside the metadata, such as a signature, when the
// metadata is served unchanged, so they are proxied as usual.
func (h *MavenHandler) serveCooldownMetadata(w http.ResponseWriter, r *http.Request, source mavenSource, urlPath string) bool {
	filename := path.Base(urlPath)
	suffix, ok := strings.CutPrefix(filename, mavenMetadataFile)
	if !ok {
		return false
	}
	metadataPath := path.Join(path.Dir(urlPath), mavenMetadataFile)
	body, contentType, cacheKey, err := h.fetchMetadata(r.Context(), source, metadataPath)
	if err != nil {
		h.writeMetadataError(w, err)
		return true
	}

	m, err := h.proxy.cachedRewrite(r.Context(), "maven", h.proxyURL, cacheKey, body, "", func(in []byte) ([]byte, []string, error) {
		// The rewrite is shared by requests waiting on it and cached, so it
		// finishes even if the request that started it goes away.
		out, err := h.filterMetadata(context.WithoutCancel(r.Context()), source, path.Dir(urlPath), in)
		return out, nil, err
	})
	if err != nil {
		if r.Context().Err() != nil {
			return true
		}
		if writeUpstreamHeld(w, err) {
			return true
		}
		h.proxy.Logger.Warn("cooldown: could not filter maven metadata", "path", metadataPath, "error", err)
		http.Error(w, "failed to check cooldown", http.StatusBadGateway)
		return true
	}

	switch {
	case suffix == "":
		// The metadata cache records JSON when an upstream sent no type,
		// and maven-metadata.xml is always XML.
		if contentType == "" || contentType == contentTypeJSON {
			contentType = "text/xml"
		}
		markStale(w, h.proxy.lookupCachedMeta("maven", cacheKey).stale)
		serveRewrittenMetadataAs(w, r, m, contentType)
	case mavenChecksums[suffix] != nil:
		sum := mavenChecksums[suffix]()
		_, _ = sum.Write(m.content())
		w.Header().Set(headerContentType, "text/plain")
		_, _ = io.WriteString(w, hex.EncodeToString(sum.Sum(nil)))
	case bytes.Equal(m.content(), body):
		return false
	default:
		// A signature of the upstream metadata doesn't match what is served.
		http.Error(w, "not found", http.StatusNotFound)
	}
	return true
}

// filterMetadata removes versions in cooldown from an artifact's
// maven-metadata.xml and points <latest> and <release> at the newest
// versions left. Metadata without a version list, such as a group's plugin
// list or a SNAPSHOT's build list, is returned unchanged. The document is
// edited in place rather than encoded again, so everything else in it stays
// as upstream sent it.
func (h *MavenHandler) filterMetadata(ctx context.Context, source mavenSource, artifactDir string, body []byte) ([]byte, error) {
	doc, err := parseMavenMetadata(body)
	if err != nil {
		return nil, err
	}
	group, artifact, ok := mavenCoordinates(artifactDir)
	if !ok || len(doc.versions) == 0 {
		return body, nil
	}
	packagePURL := mavenPackagePURL(group, artifact)
	// No version can be newer than the metadata listing it.
	if lastUpdated, err := time.Parse(mavenLastUpdatedLayout, doc.lastUpdated); err == nil &&
		h.proxy.Cooldown.IsAllowed("maven", packagePURL, lastUpdated) {
		return body, nil
	}

	removed := map[string]bool{}
	for i := len(doc.versions) - 1; i >= 0; i-- {
		v := doc.versions[i].value
		if strings.HasSuffix(v, mavenSnapshot) {
			continue
		}
		published, err := h.publishTime(ctx, source, artifactDir, v)
		if err != nil {
			return nil, fmt.Errorf("reading time of %s: %w", v, err)
		}
		if published.IsZero() {
			continue
		}
		if h.proxy.Cooldown.IsAllowed("maven", packagePURL, published) {
			break
		}
		h.proxy.Logger.Info("cooldown: filtering maven version", "group", group, "artifact", artifact, "version", v)
		removed[v] = true
	}
	if len(removed) == 0 {
		return body, nil
	}
	return doc.without(body, removed), nil
}

// mavenElement is an element of maven-metadata.xml: its text and where it
// starts and ends in the document.
type mavenElement struct {
	value      string
	start, end int
}

type mavenMetadata struct {
	versions        []mavenElement
	latest, release *mavenElement
	lastUpdated     string
}

// parseMavenMetadata reads the version list, <latest>, <release> and
// <lastUpdated> of a maven-metadata.xml, with their offsets.
func parseMavenMetadata(body []byte) (*mavenMetadata, error) {
	dec := xml.NewDecoder(bytes.NewReader(body))
	// Versions are ASCII, so any declared charset can be read as bytes.
	dec.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil }

	doc := &mavenMetadata{}
	var stack []string
	var current *mavenElement
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
				current = &mavenElement{start: offset}
			}
		case xml.CharData:
			if current != nil {
				current.value += string(t)
			}
		case xml.EndElement:
			if current != nil {
				current.value = strings.TrimSpace(current.value)
				current.end = int(dec.InputOffset())
				switch t.Name.Local {
				case "version":
					doc.versions = append(doc.versions, *current)
				case "latest":
					doc.latest = current
				case "release":
					doc.release = current
				case "lastUpdated":
					doc.lastUpdated = current.value
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

// without returns body with the removed versions taken out of the version
// list, and <latest> and <release> set to the last versions left in list
// order, which is how the repository chose them. Either is dropped when no
// version is left for it.
func (doc *mavenMetadata) without(body []byte, removed map[string]bool) []byte {
	type edit struct {
		start, end int
		text       string
	}
	var edits []edit
	drop := func(e mavenElement) {
		edits = append(edits, edit{start: lineStart(body, e.start), end: e.end})
	}
	var latest, release string
	for _, v := range doc.versions {
		if removed[v.value] {
			drop(v)
			continue
		}
		latest = v.value
		if !strings.HasSuffix(v.value, mavenSnapshot) {
			release = v.value
		}
	}
	for _, field := range []struct {
		el   *mavenElement
		name string
		next string
	}{{doc.latest, "latest", latest}, {doc.release, "release", release}} {
		if field.el == nil || !removed[field.el.value] {
			continue
		}
		if field.next == "" {
			drop(*field.el)
			continue
		}
		edits = append(edits, edit{field.el.start, field.el.end, "<" + field.name + ">" + field.next + "</" + field.name + ">"})
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
