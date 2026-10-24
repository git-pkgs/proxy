package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/git-pkgs/proxy/internal/retention"
	"github.com/git-pkgs/purl"
)

const (
	mavenCentralUpstream       = "https://repo1.maven.org/maven2"
	gradlePluginPortalUpstream = "https://plugins.gradle.org/m2"
	minMavenParts              = 4 // group path segments + artifact + version + filename
)

// Maven files are stored under pkg:maven/{group}/{artifact}, built from the
// group:artifact name, which is what the default key mapping produces from
// a package rule. Names keep their case, as Maven coordinates do.
func init() {
	retention.Default.Register(retention.Spec{Key: "maven", CanonicalPackage: mavenRetentionKey})
}

// mavenRetentionKey maps a package rule to the stored package PURL. A key
// without a group, or with the group written as a path (com/google/guava)
// instead of dotted, parses fine but can never match a stored package, so
// it is refused rather than accepted as a rule that does nothing.
func mavenRetentionKey(p *purl.PURL) (string, bool) {
	if p == nil || p.Namespace == "" || strings.Contains(p.Namespace, "/") {
		return "", false
	}
	return retention.DefaultCanonical("maven")(p)
}

// MavenHandler handles Maven repository protocol requests. The default
// repository is served at /maven/ and falls back to the Gradle Plugin Portal;
// each named repository is served at /maven/{name}/ from its own upstream
// alone.
type MavenHandler struct {
	proxy                   *Proxy
	upstreamURL             string
	pluginPortalUpstreamURL string
	proxyURL                string
	repositories            map[string]mavenSource
}

// mavenSource is the repository a request is served from.
type mavenSource struct {
	// name is the repository name, or "" for the default repository.
	name        string
	upstreamURL string
	// fallbackURL is tried when upstreamURL doesn't have a file. Only the
	// default repository has one, the Gradle Plugin Portal.
	fallbackURL string
	// cachePrefix is prepended to cached filenames: empty for the default
	// repository, {name}/{hash of upstreamURL}/ for a named one.
	cachePrefix string
}

// WithRepositories adds named repositories, served at /maven/{name}/.
func (h *MavenHandler) WithRepositories(repositories map[string]string) *MavenHandler {
	h.repositories = make(map[string]mavenSource, len(repositories))
	for name, upstreamURL := range repositories {
		upstreamURL = strings.TrimSuffix(upstreamURL, "/")
		digest := sha256.Sum256([]byte(upstreamURL))
		h.repositories[name] = mavenSource{
			name:        name,
			upstreamURL: upstreamURL,
			cachePrefix: name + "/" + hex.EncodeToString(digest[:mavenUpstreamHashBytes]) + "/",
		}
	}
	return h
}

// NewMavenHandler creates a new Maven repository handler.
func NewMavenHandler(proxy *Proxy, proxyURL, upstreamURL, pluginPortalUpstreamURL string) *MavenHandler {
	if strings.TrimSpace(upstreamURL) == "" {
		upstreamURL = mavenCentralUpstream
	}
	if strings.TrimSpace(pluginPortalUpstreamURL) == "" {
		pluginPortalUpstreamURL = gradlePluginPortalUpstream
	}

	return &MavenHandler{
		proxy:                   proxy,
		upstreamURL:             strings.TrimSuffix(upstreamURL, "/"),
		pluginPortalUpstreamURL: strings.TrimSuffix(pluginPortalUpstreamURL, "/"),
		proxyURL:                strings.TrimSuffix(proxyURL, "/"),
	}
}

// Routes returns the HTTP handler for Maven requests.
func (h *MavenHandler) Routes() http.Handler {
	mux := http.NewServeMux()

	// Maven repository layout: /{group}/{artifact}/{version}/{filename}
	// e.g., /com/google/guava/guava/32.1.3-jre/guava-32.1.3-jre.jar
	mux.HandleFunc("GET /", h.handleRequest)

	return rejectQueryChars(mux)
}

// handleRequest routes Maven requests based on the path.
func (h *MavenHandler) handleRequest(w http.ResponseWriter, r *http.Request) {
	urlPath := strings.TrimPrefix(r.URL.Path, "/")
	if urlPath == "" {
		http.NotFound(w, r)
		return
	}

	source := mavenSource{upstreamURL: h.upstreamURL, fallbackURL: h.pluginPortalUpstreamURL}
	if name, rest, ok := strings.Cut(urlPath, "/"); ok {
		if named, found := h.repositories[name]; found {
			source = named
			urlPath = rest
		}
	} else if _, found := h.repositories[urlPath]; found {
		http.NotFound(w, r)
		return
	}
	if urlPath == "" {
		http.NotFound(w, r)
		return
	}

	filename := path.Base(urlPath)

	if h.isVersionListingFile(filename) {
		h.handleMetadata(w, r, source, urlPath)
		return
	}

	if h.isArtifactFile(filename) || h.isChecksumFile(filename) || h.isCoordinateFile(urlPath) {
		h.handleDownload(w, r, source, urlPath)
		return
	}

	// Proxy everything else (directory listings, catalogs etc.)
	h.proxyUpstream(w, r, source, urlPath)
}

// metadataCacheKey is the metadata cache key of urlPath. The default
// repository's keys are hashed from its upstream URLs and the path. A named
// repository's keys are hashed from its name, upstream URL and the path, so
// repositories never share entries and repointing one drops what it cached.
func (s mavenSource) metadataCacheKey(urlPath string) string {
	if s.name == "" {
		return hashedCacheKey(s.upstreamURL, s.fallbackURL, urlPath)
	}
	digest := sha256.Sum256([]byte(s.name + "\x00" + s.upstreamURL + "\x00" + urlPath))
	return "repo_" + hex.EncodeToString(digest[:])
}

// cacheFilename is the name a file is cached under. A named repository's
// files are prefixed with its name and a hash of its upstream URL, so the
// same coordinates in two repositories, which may hold different bytes, are
// cached apart, and repointing a repository stops serving what the old
// upstream sent.
func (s mavenSource) cacheFilename(filename string) string {
	return s.cachePrefix + filename
}

// mavenUpstreamHashBytes is how much of the upstream URL's hash a named
// repository's cache filenames carry: enough to tell its upstreams apart.
const mavenUpstreamHashBytes = 8

func (h *MavenHandler) handleMetadata(w http.ResponseWriter, r *http.Request, source mavenSource, urlPath string) {
	if h.cooldownEnabled() && h.serveCooldownMetadata(w, r, source, urlPath) {
		return
	}

	body, contentType, cacheKey, err := h.fetchMetadata(r.Context(), source, urlPath)
	if err != nil {
		h.writeMetadataError(w, err)
		return
	}

	h.proxy.writeMetadataCachedResponse(w, r, "maven", cacheKey, body, contentType)
}

// fetchMetadata fetches a metadata file from the source's upstream, or from
// its fallback when the upstream doesn't have it, and returns it with the
// key it is cached under.
func (h *MavenHandler) fetchMetadata(ctx context.Context, source mavenSource, urlPath string) (body []byte, contentType, cacheKey string, err error) {
	cacheKey = source.metadataCacheKey(urlPath)
	upstreamURL := fmt.Sprintf("%s/%s", source.upstreamURL, urlPath)

	body, contentType, err = h.proxy.FetchOrCacheMetadata(ctx, "maven", cacheKey, upstreamURL, "*/*")
	if err != nil && source.fallbackURL != "" {
		if errors.Is(err, ErrUpstreamNotFound) {
			pluginPortalURL := fmt.Sprintf("%s/%s", source.fallbackURL, urlPath)
			h.proxy.Logger.Info("maven metadata unavailable in primary upstream, trying Gradle Plugin Portal",
				"path", urlPath)
			body, contentType, err = h.proxy.FetchOrCacheMetadata(ctx, "maven", cacheKey, pluginPortalURL, "*/*")
		}
	}
	return body, contentType, cacheKey, err
}

func (h *MavenHandler) writeMetadataError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrUpstreamNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if writeUpstreamHeld(w, err) {
		return
	}
	h.proxy.Logger.Error("metadata fetch failed", "error", err)
	http.Error(w, "failed to fetch from upstream", http.StatusBadGateway)
}

// handleDownload serves an artifact file, fetching and caching from upstream if needed.
func (h *MavenHandler) handleDownload(w http.ResponseWriter, r *http.Request, source mavenSource, urlPath string) {
	// Parse Maven path: group/artifact/version/filename
	// e.g., com/google/guava/guava/32.1.3-jre/guava-32.1.3-jre.jar
	group, artifact, version, filename := h.parsePath(urlPath)
	if artifact == "" {
		h.proxyUpstream(w, r, source, urlPath)
		return
	}

	// The .pom of a version whose time isn't known yet carries it in its
	// own Last-Modified, so it is checked once fetched, without a request
	// of its own. Maven fetches the .pom before a version's other files.
	ctx, checkAfter := h.cooldownFromDownload(r.Context(), source, urlPath, version)
	if !checkAfter && h.checkCooldown(w, r, source, urlPath, version) {
		return
	}

	// Maven uses group:artifact as the package name
	name := fmt.Sprintf("%s:%s", group, artifact)

	h.proxy.Logger.Info("maven download request", "repository", source.name,
		"group", group, "artifact", artifact, "version", version, "filename", filename)

	upstreamURL := fmt.Sprintf("%s/%s", source.upstreamURL, urlPath)
	cacheFilename := source.cacheFilename(filename)

	result, err := h.proxy.GetOrFetchArtifactFromURL(ctx, "maven", name, version, cacheFilename, upstreamURL)
	if err != nil && source.fallbackURL != "" {
		if errors.Is(err, ErrUpstreamNotFound) {
			pluginPortalURL := fmt.Sprintf("%s/%s", source.fallbackURL, urlPath)
			h.proxy.Logger.Info("maven artifact not found in primary upstream, trying Gradle Plugin Portal",
				"group", group, "artifact", artifact, "version", version, "filename", filename)
			result, err = h.proxy.GetOrFetchArtifactFromURL(ctx, "maven", name, version, cacheFilename, pluginPortalURL)
		}
	}
	if err != nil {
		h.proxy.serveArtifactError(w, err, "failed to fetch artifact")
		return
	}
	if checkAfter && h.checkCooldown(w, r, source, urlPath, version) {
		_ = result.Reader.Close()
		return
	}

	ServeArtifactRequest(w, r, result)
}

// parsePath extracts Maven coordinates from a URL path.
// e.g., "com/google/guava/guava/32.1.3-jre/guava-32.1.3-jre.jar"
// -> ("com.google.guava", "guava", "32.1.3-jre", "guava-32.1.3-jre.jar")
func (h *MavenHandler) parsePath(urlPath string) (group, artifact, version, filename string) {
	parts := strings.Split(urlPath, "/")
	if len(parts) < minMavenParts {
		return "", "", "", ""
	}

	filename = parts[len(parts)-1]
	version = parts[len(parts)-2]
	artifact = parts[len(parts)-3]
	groupParts := parts[:len(parts)-3]
	group = strings.Join(groupParts, ".")

	return group, artifact, version, filename
}

// isArtifactFile returns true if the filename looks like a Maven artifact.
func (h *MavenHandler) isArtifactFile(filename string) bool {
	// Common artifact extensions, and the archives distributions such as
	// apache-maven-{version}-bin.tar.gz are published as.
	extensions := []string{
		".jar", ".war", ".ear", ".pom", ".aar", ".klib", ".module",
		".zip", ".tar.gz", ".tgz", ".tar.bz2", ".tar.xz",
	}
	for _, ext := range extensions {
		if strings.HasSuffix(filename, ext) {
			return true
		}
	}
	return false
}

// isCoordinateFile reports whether urlPath is a file named after the
// artifact and version of its directory, {artifact}-{version}..., as Maven
// names every file it publishes, whatever the extension. A SNAPSHOT's
// files carry a timestamp in place of "SNAPSHOT".
func (h *MavenHandler) isCoordinateFile(urlPath string) bool {
	_, artifact, version, filename := h.parsePath(urlPath)
	if artifact == "" || version == "" {
		return false
	}
	return strings.HasPrefix(filename, artifact+"-"+strings.TrimSuffix(version, "SNAPSHOT"))
}

// isVersionListingFile matches maven-metadata.xml and its checksums. The
// listing changes with every release, so it is cached for the metadata TTL
// rather than kept like an artifact.
func (h *MavenHandler) isVersionListingFile(filename string) bool {
	return strings.HasPrefix(filename, "maven-metadata.xml")
}

// isChecksumFile matches the checksum and signature files published beside
// an artifact. They are as immutable as the artifact, so they are cached
// with it rather than refetched on every request.
func (h *MavenHandler) isChecksumFile(filename string) bool {
	for _, ext := range []string{".sha1", ".sha256", ".sha512", ".md5", ".asc"} {
		if strings.HasSuffix(filename, ext) {
			return true
		}
	}
	return false
}

// proxyUpstream forwards a request to the source's upstream without caching.
func (h *MavenHandler) proxyUpstream(w http.ResponseWriter, r *http.Request, source mavenSource, urlPath string) {
	h.proxy.ProxyUpstream(w, r, source.upstreamURL+"/"+urlPath, nil)
}
