package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/git-pkgs/proxy/internal/retention"
	"github.com/git-pkgs/purl"
)

const (
	composerUpstream   = "https://packagist.org"
	composerRepo       = "https://repo.packagist.org"
	composerMinified   = "composer/2.0"
	composerUnset      = "__unset"
	composerDevReset   = "~dev"
	composerDevFile    = "~dev" // suffix of the metadata file holding dev versions
	vendorPackageParts = 2
)

var composerUnsetJSON = []byte(`"` + composerUnset + `"`)

// Archives are stored under pkg:composer/{vendor}/{package}, which purl
// lowercases, so a package rule matches whatever case it is written in. The
// spec also claims the "packagist" spelling git-pkgs writes into
// packages.ecosystem.
func init() {
	retention.Default.Register(retention.Spec{Key: "composer", CanonicalPackage: composerRetentionKey})
}

// composerRetentionKey maps a package rule to the stored package PURL. A
// key without a vendor parses fine but can never match, since every stored
// package has one, so it is refused.
func composerRetentionKey(p *purl.PURL) (string, bool) {
	if p == nil || p.Namespace == "" {
		return "", false
	}
	return retention.DefaultCanonical("composer")(p)
}

// ComposerHandler handles Composer/Packagist registry protocol requests.
type ComposerHandler struct {
	proxy       *Proxy
	upstreamURL string
	repoURL     string
	proxyURL    string
	routes      packageRoutes
}

// NewComposerHandler creates a new Composer protocol handler.
func NewComposerHandler(proxy *Proxy, proxyURL string) *ComposerHandler {
	return &ComposerHandler{
		proxy:       proxy,
		upstreamURL: composerUpstream,
		repoURL:     composerRepo,
		proxyURL:    strings.TrimSuffix(proxyURL, "/"),
	}
}

// NewComposerHandlerWithUpstreams creates a Composer handler with custom API
// and repository upstreams.
func NewComposerHandlerWithUpstreams(proxy *Proxy, proxyURL, upstreamURL, repoURL string) *ComposerHandler {
	h := NewComposerHandler(proxy, proxyURL)
	h.upstreamURL = configuredUpstreamURL(upstreamURL, composerUpstream)
	h.repoURL = configuredUpstreamURL(repoURL, composerRepo)
	return h
}

// WithPackageRoutes sends packages matching a pattern to a dedicated Composer
// repository instead of the default one. See packageRoutes.
func (h *ComposerHandler) WithPackageRoutes(routes map[string]string) *ComposerHandler {
	h.routes = newPackageRoutes(routes)
	return h
}

// Routes returns the HTTP handler for Composer requests.
func (h *ComposerHandler) Routes() http.Handler {
	mux := http.NewServeMux()

	// Service index
	mux.HandleFunc("GET /packages.json", h.handleServiceIndex)

	// Package metadata (Composer v2 format) - use prefix since {package}.json isn't allowed
	mux.HandleFunc("GET /p2/", h.handlePackageMetadata)

	// Package downloads
	mux.HandleFunc("GET /files/{vendor}/{package}/{version}/{filename}", h.handleDownload)

	// Search and list (proxy without modification)
	mux.HandleFunc("GET /search.json", h.proxyUpstream)
	mux.HandleFunc("GET /packages/list.json", h.proxyUpstream)

	return mux
}

// handleServiceIndex returns the Composer repository service index.
func (h *ComposerHandler) handleServiceIndex(w http.ResponseWriter, r *http.Request) {
	// Return a minimal service index pointing to our proxy
	index := map[string]any{
		"packages":           map[string]any{},
		"metadata-url":       h.proxyURL + "/composer/p2/%package%.json",
		"notify-batch":       h.upstreamURL + "/downloads/",
		"search":             h.proxyURL + "/composer/search.json?q=%query%&type=%type%",
		"providers-lazy-url": h.proxyURL + "/composer/p2/%package%.json",
	}

	w.Header().Set(headerContentType, "application/json")
	_ = json.NewEncoder(w).Encode(index)
}

// sourceFor returns the repository serving a package and the key its metadata
// is cached under. packageName may carry the ~dev suffix of the file holding
// the package's dev versions: routes match the package without it, so its dev
// versions come from the same repository, while the cache key keeps it so the
// two files stay apart.
func (h *ComposerHandler) sourceFor(packageName string) (repoURL, cacheKey string) {
	if route, routed := h.routes.match(strings.TrimSuffix(packageName, composerDevFile)); routed {
		return route.url, route.cacheKey(packageName)
	}
	return h.repoURL, packageName
}

// handlePackageMetadata proxies and rewrites package metadata.
func (h *ComposerHandler) handlePackageMetadata(w http.ResponseWriter, r *http.Request) {
	// Parse path: /p2/{vendor}/{package}.json
	path := strings.TrimPrefix(r.URL.Path, "/p2/")
	path = strings.TrimSuffix(path, ".json")
	parts := strings.SplitN(path, "/", vendorPackageParts)
	if len(parts) != vendorPackageParts || parts[0] == "" || parts[1] == "" {
		http.Error(w, "invalid package path", http.StatusBadRequest)
		return
	}
	vendor := parts[0]
	pkg := parts[1]
	packageName := vendor + "/" + pkg

	h.proxy.Logger.Info("composer metadata request", "package", packageName)

	repoURL, cacheKey := h.sourceFor(packageName)
	upstreamURL := fmt.Sprintf("%s/p2/%s/%s.json", repoURL, vendor, pkg)

	basePackage := strings.TrimSuffix(packageName, composerDevFile)
	local, err := h.composerLocalEntries(basePackage, basePackage != packageName)
	if err != nil {
		h.proxy.Logger.Error("failed to read pushed composer versions", "package", basePackage, "error", err)
		http.Error(w, "failed to read pushed versions", http.StatusInternalServerError)
		return
	}
	if h.proxy.hostedOnly("composer", basePackage) {
		h.serveHostedOnlyMetadata(w, r, basePackage, cacheKey, local)
		return
	}
	if len(local) > 0 {
		h.serveMetadataWithLocal(w, r, basePackage, upstreamURL, cacheKey, local)
		return
	}

	if stored, ok := h.proxy.storedRewrite("composer", cacheKey, h.proxyURL, cacheKey); ok {
		serveRewrittenMetadata(w, r, stored)
		return
	}

	upstream := h.proxy.fetchMetadataDocument(r.Context(), "composer", cacheKey, upstreamURL, contentTypeJSON, nil)
	if err := upstream.err; err != nil {
		if errors.Is(err, ErrUpstreamNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if writeUpstreamHeld(w, err) {
			return
		}
		h.proxy.Logger.Error("upstream request failed", "error", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}

	rewritten, err := h.proxy.cachedRewrite(r.Context(), "composer", h.proxyURL, cacheKey, upstream.body, upstream.digest, h.rewriteMetadataKeeping)
	if err != nil {
		if r.Context().Err() != nil {
			return // the client left while waiting on a shared rewrite
		}
		if h.proxy.mustFilterMetadata("composer", basePackage) {
			h.proxy.Logger.Error("failed to filter composer metadata", "package", basePackage, "error", err)
			http.Error(w, "failed to filter package metadata", http.StatusBadGateway)
			return
		}
		h.proxy.Logger.Warn("failed to rewrite metadata, proxying original", "error", err)
		markStale(w, upstream.stale)
		w.Header().Set(headerContentType, "application/json")
		_, _ = w.Write(upstream.body)
		return
	}

	markStale(w, upstream.stale)
	serveRewrittenMetadata(w, r, rewritten)
}

// rewriteMetadataKeeping rewrites dist URLs and filters versions in cooldown.
// It returns the kept package names and versions without expanding minified lists.
func (h *ComposerHandler) rewriteMetadataKeeping(body []byte) ([]byte, []string, error) {
	return h.rewriteMetadataWithLocal(body, "", nil)
}

// rewriteMetadataWithLocal is rewriteMetadataKeeping that also lists the
// pushed versions in local under localPackage, in place of any upstream entry
// with the same version. Cooldown only filters upstream versions. A document
// that does not list localPackage at all is replaced by one holding only the
// pushed versions.
func (h *ComposerHandler) rewriteMetadataWithLocal(body []byte, localPackage string, local []composerLocalEntry) ([]byte, []string, error) {
	if !json.Valid(body) {
		return nil, nil, errors.New("composer metadata is not valid JSON")
	}
	format, _, err := lookupJSONString(body, "minified")
	if err != nil {
		return nil, nil, err
	}
	minified := format == composerMinified

	var out bytes.Buffer
	var kept []string
	listed := false
	out.Grow(len(body) + len(body)/8)
	err = rewriteJSONMembers(&out, body, func(out *bytes.Buffer, m jsonMember) (bool, error) {
		packages := m.value(body)
		if !jsonKeyIs(m.key(body), "packages") || packages[0] != '{' {
			return false, nil
		}
		return true, rewriteJSONMembers(out, packages, func(out *bytes.Buffer, p jsonMember) (bool, error) {
			versions := p.value(packages)
			if versions[0] != '[' {
				return false, nil
			}
			packageName, err := jsonKey(p.key(packages))
			if err != nil {
				return false, err
			}
			var pushed []composerLocalEntry
			if packageName == localPackage {
				pushed, listed = local, true
			}
			return true, h.writeVersions(out, packageName, versions, minified, &kept, pushed)
		})
	})
	if err != nil {
		return nil, nil, err
	}
	if len(local) > 0 && !listed {
		document, err := localComposerDocument(localPackage, local)
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
func (h *ComposerHandler) writeVersions(out *bytes.Buffer, packageName string, versions []byte, minified bool, kept *[]string, local []composerLocalEntry) error {
	packagePURL := canonicalPackagePURL("composer", packageName)
	upstream, written := newComposerFields(), newComposerFields()
	devReset, first := false, true
	_, routed := h.routes.match(packageName)

	out.WriteByte('[')
	err := forEachJSONElement(versions, func(entry []byte) error {
		if minified && jsonStringIs(entry, composerDevReset) {
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
		if pushedComposerVersion(local, version) || h.shouldFilterVersion(packagePURL, packageName, version, upstream.get("time")) {
			return nil
		}

		if !first {
			out.WriteByte(',')
		}
		first = false
		if devReset {
			out.WriteString(`"` + composerDevReset + `",`)
			devReset = false
		}
		if routed {
			disableDefaultNotification(upstream)
		}
		h.writeVersion(out, packageName, version, upstream, written)
		*kept = append(*kept, packageName+"\x00"+version)
		return nil
	})
	if err != nil {
		return err
	}
	for _, e := range local {
		pushed := newComposerFields()
		if err := pushed.apply(e.entry, false); err != nil {
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
			out.WriteString(`"` + composerDevReset + `",`)
			devReset = false
		}
		h.writeVersion(out, packageName, e.version, pushed, written)
		*kept = append(*kept, packageName+"\x00"+e.version)
	}
	out.WriteByte(']')
	return nil
}

// writeVersion writes the fields of upstream that differ from written, with
// dist pointing at this proxy, and "__unset" for those written has and
// upstream lacks. written is then what the client expands this version to.
func (h *ComposerHandler) writeVersion(out *bytes.Buffer, packageName, version string, upstream, written *composerFields) {
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
			value = h.rewriteDist(packageName, version, value)
		}
		if bytes.Equal(value, written.get(f.name)) {
			continue
		}
		writeMember(f.key, value)
		written.set(f.name, f.key, value)
	}
	for _, f := range written.fields {
		if f.value != nil && upstream.get(f.name) == nil {
			writeMember(f.key, composerUnsetJSON)
			written.set(f.name, f.key, nil)
		}
	}
	out.WriteByte('}')
}

// shouldFilterVersion returns true if the version should be excluded due to cooldown.
func (h *ComposerHandler) shouldFilterVersion(packagePURL, packageName, version string, published []byte) bool {
	if h.proxy.Cooldown == nil || !h.proxy.Cooldown.Enabled() {
		return false
	}

	publishedAt, ok := composerPublishTime(published)
	if !ok {
		return false
	}

	if !h.proxy.Cooldown.IsAllowed("composer", packagePURL, publishedAt) {
		h.proxy.Logger.Info("cooldown: filtering composer version",
			"package", packageName, "version", version)
		return true
	}

	return false
}

// composerPublishTime parses the raw "time" field of a version, reporting
// false when it is missing or not an RFC 3339 string.
func composerPublishTime(published []byte) (time.Time, bool) {
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
func disableDefaultNotification(fields *composerFields) {
	if fields.get("notification-url") == nil {
		fields.set("notification-url", []byte(`"notification-url"`), []byte(`""`))
	}
}

// rewriteDist returns a version's dist object with its url pointing at this
// proxy. A dist without a string url is returned unchanged.
func (h *ComposerHandler) rewriteDist(packageName, version string, dist []byte) []byte {
	urlMember, ok, err := findJSONMember(dist, "url")
	if err != nil || !ok || dist[urlMember.valStart] != '"' {
		return dist
	}
	var upstreamURL string
	if err := json.Unmarshal(urlMember.value(dist), &upstreamURL); err != nil {
		return dist
	}
	distType, _, _ := lookupJSONString(dist, "type")

	proxyURL, ok := h.proxyDistURL(packageName, version, upstreamURL, distType)
	if !ok {
		return dist
	}
	encoded, err := json.Marshal(proxyURL)
	if err != nil {
		return dist
	}
	rewritten := make([]byte, 0, len(dist)-len(urlMember.value(dist))+len(encoded))
	rewritten = append(rewritten, dist[:urlMember.valStart]...)
	rewritten = append(rewritten, encoded...)
	return append(rewritten, dist[urlMember.valEnd:]...)
}

// proxyDistURL returns the URL this proxy serves a version's archive at, or
// false when the upstream URL or package name gives nothing to build it from.
func (h *ComposerHandler) proxyDistURL(packageName, version, upstreamURL, distType string) (string, bool) {
	if upstreamURL == "" {
		return "", false
	}
	parts := strings.SplitN(packageName, "/", vendorPackageParts)
	if len(parts) != vendorPackageParts {
		return "", false
	}
	return fmt.Sprintf("%s/composer/files/%s/%s/%s/%s",
		h.proxyURL, parts[0], parts[1], url.PathEscape(version), distFilename(upstreamURL, distType)), true
}

// distFilename returns the file name this proxy serves an archive under: the
// last segment of its upstream URL.
func distFilename(upstreamURL, distType string) string {
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

// composerCommitPattern matches a full git commit hash, SHA-1 or SHA-256.
var composerCommitPattern = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// upstreamDistURL returns the upstream URL of the archive served as filename,
// given the dist URL the version's metadata lists now. A branch version only
// lists its current head while lock files pin older commits, so for a branch
// a dist URL ending in a commit hash is rebuilt for the requested commit. A
// tagged release has to ask for the listed name. Any other name gives false:
// fetching the listed archive would cache it under a name it does not have.
func upstreamDistURL(listedURL, distType, filename string, branch bool) (string, bool) {
	listed := distFilename(listedURL, distType)
	// Proxy versions before the .zip suffix handed out bare commit names,
	// and lock files written back then still ask for them.
	if bare := distFilename(listedURL, ""); listed != bare && path.Ext(filename) == "" {
		filename += listed[len(bare):]
	}
	if filename == listed {
		return listedURL, true
	}
	if commit, ok := shaQueryCommit(listed); ok {
		requested, ok := shaQueryCommit(filename)
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
	if !branch || !ok || !composerCommitPattern.MatchString(commit) || !composerCommitPattern.MatchString(requested) {
		return "", false
	}
	idx := strings.LastIndex(listedURL, commit)
	return listedURL[:idx] + requested + listedURL[idx+len(commit):], true
}

// shaQueryCommit returns the commit an archive name passes in a sha query
// parameter. GitLab names the archive of every commit archive.zip and picks
// the commit that way.
func shaQueryCommit(filename string) (string, bool) {
	_, query, ok := strings.Cut(filename, "?")
	if !ok {
		return "", false
	}
	values, err := url.ParseQuery(query)
	if err != nil {
		return "", false
	}
	commit := values.Get("sha")
	return commit, composerCommitPattern.MatchString(commit)
}

// composerFields holds one version's fields as Composer sees them after
// expanding the minified format: raw values under their decoded names, in the
// order the names first appeared. A removed field keeps its slot with a nil
// value, so the order stays stable when it comes back.
type composerFields struct {
	fields []composerField
	index  map[string]int
}

type composerField struct {
	name       string
	key, value []byte
}

func newComposerFields() *composerFields {
	return &composerFields{index: map[string]int{}}
}

func (f *composerFields) reset() {
	f.fields = f.fields[:0]
	clear(f.index)
}

// get returns the raw value of the named field, or nil when it is not set.
func (f *composerFields) get(name string) []byte {
	if i, ok := f.index[name]; ok {
		return f.fields[i].value
	}
	return nil
}

// string returns the named field when it is a string, or "".
func (f *composerFields) string(name string) string {
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
func (f *composerFields) set(name string, key, value []byte) {
	if i, ok := f.index[name]; ok {
		f.fields[i].key, f.fields[i].value = key, value
		return
	}
	f.index[name] = len(f.fields)
	f.fields = append(f.fields, composerField{name: name, key: key, value: value})
}

// apply overlays the members of a version entry. In minified metadata the
// value "__unset" removes the field instead.
func (f *composerFields) apply(entry []byte, minified bool) error {
	return forEachJSONMember(entry, func(m jsonMember) error {
		key, value := m.key(entry), m.value(entry)
		if minified && jsonStringIs(value, composerUnset) {
			value = nil
		}
		if bytes.IndexByte(key, '\\') < 0 {
			if i, ok := f.index[string(key[1:len(key)-1])]; ok {
				f.fields[i].key, f.fields[i].value = key, value
				return nil
			}
		}
		name, err := jsonKey(key)
		if err != nil {
			return err
		}
		f.set(name, key, value)
		return nil
	})
}

// handleDownload serves a package file, fetching and caching from upstream if needed.
func (h *ComposerHandler) handleDownload(w http.ResponseWriter, r *http.Request) {
	vendor := r.PathValue("vendor")
	pkg := r.PathValue("package")
	version := r.PathValue("version")
	filename := r.PathValue("filename")

	packageName := vendor + "/" + pkg

	h.proxy.Logger.Info("composer download request",
		"package", packageName, "version", version, "filename", filename)

	local, err := h.proxy.GetLocalArtifact(r.Context(), "composer", packageName, version, filename)
	if err != nil {
		h.proxy.serveArtifactError(w, err, "failed to read pushed package")
		return
	}
	if local != nil {
		ServeArtifactRequest(w, r, local)
		return
	}
	if h.proxy.hostedOnly("composer", packageName) {
		http.Error(w, "version not found", http.StatusNotFound)
		return
	}

	// The archive is cached under the file name from the path, so fetch the
	// one that name stands for. Compare it as sent, the way rewriteDist handed
	// it out, query included.
	requested := path.Base(r.URL.EscapedPath())
	if r.URL.RawQuery != "" {
		requested += "?" + r.URL.RawQuery
	} else if h.serveCachedRelease(w, r, packageName, version, filename) {
		return
	}
	files := h.metadataFilesForVersion(vendor, pkg, version)
	dist, downloadURL, ok := h.cachedDist(r.Context(), files, packageName, version, requested)
	if !ok {
		if dist, downloadURL, ok = h.upstreamDist(w, r, files, packageName, version, requested); !ok {
			return
		}
	}
	h.recordPublishTime(packageName, version, dist.published)

	// Lock files name archive URLs directly, so a version filtered out of the
	// metadata still has to be withheld here.
	if h.shouldFilterVersion(canonicalPackagePURL("composer", packageName), packageName, version, dist.published) {
		http.Error(w, "version not found", http.StatusNotFound)
		return
	}

	h.proxy.Logger.Debug("resolved download URL",
		"package", packageName, "version", version,
		"download_url", downloadURL)

	// Archives picked by ?sha= share one file name, so the commit goes into
	// the name they are cached under. Proxy versions before that cached every
	// commit under the shared name, which is dropped once a commit is fetched.
	var sharedFilename string
	if commit, ok := shaQueryCommit(requested); ok {
		sharedFilename = h.routes.cacheFilename(packageName, filename)
		ext := path.Ext(filename)
		filename = strings.TrimSuffix(filename, ext) + "-" + commit + ext
	}
	cacheFilename := h.routes.cacheFilename(packageName, filename)
	if commit := strings.TrimSuffix(filename, path.Ext(filename)); isDevVersion(version) && composerCommitPattern.MatchString(commit) {
		h.discardOtherCommit(packageName, version, cacheFilename, commit)
	}
	result, err := h.proxy.GetOrFetchArtifactFromURLWithIntegrity(r.Context(), "composer", packageName, version, cacheFilename, downloadURL,
		composerIntegrity(dist.shasum, version, downloadURL == dist.url))
	if err != nil {
		h.proxy.serveArtifactError(w, err, "failed to fetch package")
		return
	}
	if sharedFilename != "" && !result.Cached {
		if err := h.proxy.ClearCachedArtifact("composer", packageName, version, sharedFilename); err != nil {
			h.proxy.Logger.Warn("failed to discard composer archive cached under a shared name",
				"package", packageName, "version", version, "filename", sharedFilename, "error", err)
		}
	}

	ServeArtifactRequest(w, r, result)
}

// releaseTimeStored reports whether the publish time of version is kept in
// the versions table, so a download can be checked against cooldown without
// the metadata. That holds for tagged releases of packages from the default
// repository. A branch is left out: its archive and time change with every
// push. So are routed packages, as for npm: the versions row is keyed by the
// plain PURL, which does not say which repository the version came from, so
// a time read from the default repository would be trusted for a private
// version of the same name, and the other way round.
func (h *ComposerHandler) releaseTimeStored(packageName, version string) bool {
	if h.proxy.DB == nil || isDevVersion(version) {
		return false
	}
	_, routed := h.routes.match(packageName)
	return !routed
}

// serveCachedRelease serves a tagged release's archive from the cache
// without looking up its dist in the metadata, and reports whether it wrote a
// response. The archive was only cached under its name after that name was
// checked against the metadata, and the cache verifies it against the digest
// recorded then. Cooldown is checked against the publish time recorded when
// the dist was first resolved; without one, as for an archive cached by an
// older proxy, it returns false and the caller takes the metadata path, which
// records it. Requests with a query string, which pick a commit, always take
// that path.
func (h *ComposerHandler) serveCachedRelease(w http.ResponseWriter, r *http.Request, packageName, version, filename string) bool {
	if !h.releaseTimeStored(packageName, version) {
		return false
	}
	if h.proxy.Cooldown != nil && h.proxy.Cooldown.Enabled() {
		ver, err := h.proxy.DB.GetVersionByPURL(canonicalVersionPURL("composer", packageName, version))
		if err != nil || ver == nil || !ver.PublishedAt.Valid {
			return false
		}
		if !h.proxy.Cooldown.IsAllowed("composer", canonicalPackagePURL("composer", packageName), ver.PublishedAt.Time) {
			h.proxy.Logger.Info("cooldown: withholding composer archive",
				"package", packageName, "version", version)
			http.Error(w, "version not found", http.StatusNotFound)
			return true
		}
	}
	cached, err := h.proxy.GetCachedArtifact(r.Context(), "composer", packageName, version, filename)
	if err != nil {
		h.proxy.serveArtifactError(w, err, "failed to read cached package")
		return true
	}
	if cached == nil {
		return false
	}
	ServeArtifactRequest(w, r, cached)
	return true
}

// recordPublishTime stores the publish time a release's metadata lists, for
// serveCachedRelease. It is stored whether or not cooldown is enabled, so
// enabling it later does not send cached archives back to the metadata.
func (h *ComposerHandler) recordPublishTime(packageName, version string, published []byte) {
	if !h.releaseTimeStored(packageName, version) {
		return
	}
	publishedAt, ok := composerPublishTime(published)
	if !ok {
		return
	}
	if err := h.proxy.DB.SetVersionPublishedAt(canonicalVersionPURL("composer", packageName, version),
		canonicalPackagePURL("composer", packageName), publishedAt); err != nil {
		h.proxy.Logger.Warn("failed to store composer publish time",
			"package", packageName, "version", version, "error", err)
	}
}

// discardOtherCommit clears a cached branch archive named for commit that was
// fetched from another commit's URL. Proxy versions before the pinned-commit
// fix cached the branch head under whatever commit a lock file asked for. The
// record is only cleared while it still points at the object checked here.
func (h *ComposerHandler) discardOtherCommit(packageName, version, cacheFilename, commit string) {
	_, versionPURL, err := packagePURLStrings("composer", packageName, version)
	if err != nil {
		return
	}
	record, err := h.proxy.DB.GetArtifact(versionPURL, cacheFilename)
	if err != nil {
		h.proxy.Logger.Warn("failed to read composer cache record",
			"purl", versionPURL, "filename", cacheFilename, "error", err)
		return
	}
	if record == nil || !record.StoragePath.Valid || strings.Contains(record.UpstreamURL, commit) {
		return
	}
	h.proxy.Logger.Warn("cached composer archive is from another commit, discarding",
		"purl", versionPURL, "filename", cacheFilename, "upstream_url", record.UpstreamURL)
	if err := h.proxy.DB.DiscardArtifact(versionPURL, cacheFilename, record.StoragePath.String); err != nil {
		h.proxy.Logger.Warn("failed to discard composer cache record",
			"purl", versionPURL, "filename", cacheFilename, "error", err)
	}
}

// isDevVersion reports whether a Composer version string refers to a
// development (unstable, branch) version rather than a tagged release.
// Composer formats these as either "dev-<branch>" (e.g. "dev-master") or
// "<alias>-dev" (e.g. "3.x-dev").
func isDevVersion(version string) bool {
	return strings.HasPrefix(version, "dev-") || strings.HasSuffix(version, "-dev")
}

// composerMetadataFile is one of the metadata documents a version's dist can
// be listed in: its upstream URL and the key the proxy caches it under.
type composerMetadataFile struct {
	url, cacheKey string
}

// metadataFilesForVersion returns the metadata documents to consult for a
// given version, in priority order. Dev versions are served from the "~dev"
// file, tagged releases from the regular file; the other file is included as a
// fallback so an unexpected classification still resolves. The cache keys are
// the ones handlePackageMetadata stores the documents under.
func (h *ComposerHandler) metadataFilesForVersion(vendor, pkg, version string) []composerMetadataFile {
	file := func(name string) composerMetadataFile {
		repoURL, cacheKey := h.sourceFor(name)
		return composerMetadataFile{url: fmt.Sprintf("%s/p2/%s.json", repoURL, name), cacheKey: cacheKey}
	}
	stable := file(vendor + "/" + pkg)
	dev := file(vendor + "/" + pkg + composerDevFile)

	if isDevVersion(version) {
		return []composerMetadataFile{dev, stable}
	}
	return []composerMetadataFile{stable, dev}
}

// cachedDist looks up the dist of version in the metadata documents the proxy
// caches for Composer, returning it with the URL to download the requested
// file from. While the documents are fresh this costs no upstream request,
// and when upstream fails their cached copies still resolve the archive. ok is
// false when neither document lists a dist the requested file stands for, as
// for a version published after the metadata was cached, so the caller asks
// upstream instead. Without cache_metadata there is nothing to look in.
func (h *ComposerHandler) cachedDist(ctx context.Context, files []composerMetadataFile, packageName, version, requested string) (composerDist, string, bool) {
	if !h.proxy.CacheMetadata || h.proxy.DB == nil {
		return composerDist{}, "", false
	}
	for _, f := range files {
		doc := h.proxy.fetchMetadataDocument(ctx, "composer", f.cacheKey, f.url, contentTypeJSON, nil)
		if doc.err != nil {
			continue
		}
		dist, err := composerDistURL(doc.body, packageName, version)
		if err != nil || dist.url == "" {
			continue
		}
		if downloadURL, ok := upstreamDistURL(dist.url, dist.distType, requested, isDevVersion(version)); ok {
			return dist, downloadURL, true
		}
	}
	return composerDist{}, "", false
}

// upstreamDist looks up the dist of version in the metadata upstream serves
// now, returning it with the URL to download the requested file from. When it
// cannot, it writes the error response and returns false.
func (h *ComposerHandler) upstreamDist(w http.ResponseWriter, r *http.Request, files []composerMetadataFile, packageName, version, requested string) (composerDist, string, bool) {
	h.proxy.Logger.Debug("resolving download URL",
		"package", packageName, "version", version)

	var dist composerDist
	for _, f := range files {
		found, err := h.findDownloadURLFromMetadata(r.Context(), f.url, packageName, version)
		if err != nil {
			if writeUpstreamHeld(w, err) {
				return composerDist{}, "", false
			}
			h.proxy.Logger.Error("failed to fetch metadata", "error", err, "url", f.url)
			http.Error(w, "failed to fetch metadata", http.StatusBadGateway)
			return composerDist{}, "", false
		}
		if found.url != "" {
			dist = found
			break
		}
	}

	if dist.url == "" {
		h.proxy.Logger.Debug("version not found in any metadata source",
			"package", packageName, "version", version)
		http.Error(w, "version not found", http.StatusNotFound)
		return composerDist{}, "", false
	}

	downloadURL, ok := upstreamDistURL(dist.url, dist.distType, requested, isDevVersion(version))
	if !ok {
		h.proxy.Logger.Info("composer file not in upstream metadata",
			"package", packageName, "version", version,
			"filename", requested, "listed_url", dist.url)
		http.Error(w, "file not found", http.StatusNotFound)
		return composerDist{}, "", false
	}
	return dist, downloadURL, true
}

// composerDist is what a version's metadata says about its archive: the dist
// URL, type and shasum, and the raw publish time the cooldown is checked
// against.
type composerDist struct {
	url, distType, shasum string
	published             []byte
}

// findDownloadURLFromMetadata fetches a metadata document and returns the dist
// of the given version, with an empty URL if the version is not present. An
// error is returned only on transport failure; a missing document (non-200)
// or a missing version both yield an empty URL so the caller can fall back.
func (h *ComposerHandler) findDownloadURLFromMetadata(ctx context.Context, metaURL, packageName, version string) (composerDist, error) {
	h.proxy.Logger.Debug("fetching upstream metadata for download lookup",
		"url", metaURL, "package", packageName, "version", version)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metaURL, nil)
	if err != nil {
		return composerDist{}, err
	}

	resp, err := h.proxy.HTTPClient.Do(req)
	if err != nil {
		return composerDist{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	h.proxy.Logger.Debug("upstream metadata response",
		"url", metaURL, "status", resp.StatusCode)

	if resp.StatusCode != http.StatusOK {
		return composerDist{}, nil
	}

	body, err := h.proxy.ReadMetadata(resp.Body)
	if err != nil {
		return composerDist{}, err
	}

	dist, err := composerDistURL(body, packageName, version)
	if err != nil {
		return composerDist{}, err
	}
	h.proxy.Logger.Debug("download URL lookup result",
		"url", metaURL, "package", packageName, "version", version,
		"download_url", dist.url)
	return dist, nil
}

// composerDistURL returns the upstream dist of version from Composer
// metadata, with an empty URL when the package has no such version or it has
// no dist URL. It walks the version list in place, expanding only as much of
// the minified format as it needs, since it runs on each archive the proxy
// has not cached yet.
func composerDistURL(body []byte, packageName, version string) (composerDist, error) {
	format, _, err := lookupJSONString(body, "minified")
	if err != nil {
		return composerDist{}, err
	}
	versions, err := lookupJSON(body, "packages", packageName)
	if err != nil || len(versions) == 0 || versions[0] != '[' {
		return composerDist{}, err
	}
	minified := format == composerMinified

	fields := newComposerFields()
	var dist composerDist
	err = forEachJSONElement(versions, func(entry []byte) error {
		if minified && jsonStringIs(entry, composerDevReset) {
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
		if !jsonStringIs(fields.get("version"), version) {
			return nil
		}
		distRaw := fields.get("dist")
		dist.url, _, _ = lookupJSONString(distRaw, "url")
		if dist.url != "" {
			dist.distType, _, _ = lookupJSONString(distRaw, "type")
			dist.shasum, _, _ = lookupJSONString(distRaw, "shasum")
			dist.published = fields.get("time")
			return errStopScan
		}
		return nil
	})
	if errors.Is(err, errStopScan) {
		err = nil
	}
	return dist, err
}

// composerIntegrity returns the integrity to verify an archive of version
// against: the SHA-1 its dist lists as shasum. A dev branch is left
// unverified: its listed shasum describes only the current head, while every
// commit of the branch is cached under the same version, which holds a single
// recorded integrity, so a cache hit for an older pinned commit would be
// checked against the head's shasum. Composer checks dist.shasum itself
// anyway. A shasum also only describes the listed archive, and Packagist
// lists none for GitHub archives; an unusable one is ignored. Those archives
// are served unverified as before.
func composerIntegrity(shasum, version string, listed bool) string {
	if !listed || isDevVersion(version) {
		return ""
	}
	return sha1Checksum(shasum)
}

// proxyUpstream forwards a request to packagist.org without caching.
func (h *ComposerHandler) proxyUpstream(w http.ResponseWriter, r *http.Request) {
	upstreamURL := h.upstreamURL + r.URL.Path
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	h.proxy.Logger.Debug("proxying to upstream", "url", upstreamURL)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
	}

	resp, err := h.proxy.HTTPClient.Do(req)
	if err != nil {
		if writeUpstreamHeld(w, err) {
			return
		}
		h.proxy.Logger.Error("upstream request failed", "error", err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	h.proxy.relayResponse(w, r, resp, nil)
}
