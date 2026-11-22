package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/git-pkgs/proxy/internal/retention"
	"github.com/git-pkgs/purl"
)

const (
	freebsdEcosystem = "freebsd"
	// defaultFreeBSDRepositoryName is the repository name used when no
	// upstream.freebsd repositories are configured.
	defaultFreeBSDRepositoryName = "freebsd"
	// defaultFreeBSDUpstream is the official FreeBSD package mirror.
	defaultFreeBSDUpstream = "https://pkg.freebsd.org"
	freebsdNamespace       = "freebsd"
	freebsdMatchCount      = 3 // full match + name + version
	freebsdHashedDir       = "Hashed"
	freebsdPackageMedia    = "application/octet-stream"
)

// Packages are stored as pkg:bsd/freebsd/{name}, whichever configured
// repository they came from.
func init() {
	retention.Default.Register(retention.Spec{Key: freebsdEcosystem, CanonicalPackage: freebsdRetentionKey})
}

// freebsdRetentionKey maps pkg:bsd/freebsd/{name} to the stored package PURL.
// Another namespace, or a name with a slash, can never match a stored package
// and is refused.
func freebsdRetentionKey(p *purl.PURL) (string, bool) {
	if p == nil || p.Type != "bsd" || p.Namespace != freebsdNamespace || strings.Contains(p.Name, "/") {
		return "", false
	}
	canonical := purl.MakePURLString(freebsdEcosystem, p.Name, "")
	return canonical, canonical != ""
}

// FreeBSDHandler serves FreeBSD pkg repositories, ports and pkgbase alike.
// Each configured upstream is mounted at /freebsd/{repository}/ and the rest
// of the path mirrors the upstream layout ({ABI}/{repo}/{file}).
//
// Repository catalogues (meta.conf, packagesite.pkg, data.pkg) carry their
// signature inside the archive, so they are served byte for byte through the
// metadata cache. Packages under Hashed/ have the content hash in their name
// and never change, so they go to the artifact cache. Other package files,
// unhashed ones and the bootstrap package under Latest/, can be rebuilt under
// the same name and are passed through after the denylist; while a scanner
// covers freebsd they are refused, since only cached artifacts are scanned.
// Everything else, such as signatures and keys, is passed through.
type FreeBSDHandler struct {
	proxy        *Proxy
	proxyURL     string
	repositories map[string]string
}

// NewFreeBSDHandler creates a FreeBSD pkg repository handler. Without
// repositories it serves the official mirror as "freebsd".
func NewFreeBSDHandler(proxy *Proxy, proxyURL string, repositories map[string]string) *FreeBSDHandler {
	h := &FreeBSDHandler{
		proxy:        proxy,
		proxyURL:     strings.TrimSuffix(proxyURL, "/"),
		repositories: FreeBSDRepositories(repositories),
	}
	return h
}

// FreeBSDRepositories returns the upstream.freebsd repositories by name
// without trailing slashes, or the official FreeBSD mirror as "freebsd"
// when none are configured.
func FreeBSDRepositories(configured map[string]string) map[string]string {
	repositories := make(map[string]string, len(configured))
	for name, repositoryURL := range configured {
		repositories[name] = strings.TrimSuffix(repositoryURL, "/")
	}
	if len(repositories) == 0 {
		repositories[defaultFreeBSDRepositoryName] = defaultFreeBSDUpstream
	}
	return repositories
}

// Routes returns the HTTP handler for pkg requests.
// Mount this at /freebsd on your router.
func (h *FreeBSDHandler) Routes() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		path := strings.TrimPrefix(r.URL.Path, "/")

		// A decoded '?' or '#' would cut the upstream path short and cache one
		// file under another file's name.
		if containsPathTraversal(path) || strings.ContainsAny(path, "?#") {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}

		repository, rest, ok := strings.Cut(path, "/")
		upstreamURL, found := h.repositories[repository]
		if !ok || rest == "" || !found {
			http.NotFound(w, r)
			return
		}

		switch {
		case isFreeBSDCatalogue(rest):
			h.handleMetadata(w, r, repository, upstreamURL, rest)
		case isFreeBSDHashedPackage(rest):
			h.handlePackageDownload(w, r, repository, upstreamURL, rest)
		case isFreeBSDPackageFile(rest):
			h.handleUncachedPackage(w, r, upstreamURL, rest)
		default:
			h.proxyFile(w, r, upstreamURL, rest)
		}
	})
}

// freebsdCataloguePattern matches the files pkg reads to update a repository:
// meta and meta.conf, and the packagesite and data catalogues in the current
// (.pkg) and older (.txz, .tzst) packing formats.
var freebsdCataloguePattern = regexp.MustCompile(`^(?:meta(?:\.conf|\.txz|\.tzst|\.pkg)?|(?:packagesite|data)\.(?:pkg|txz|tzst))$`)

func freebsdBase(path string) string {
	return path[strings.LastIndex(path, "/")+1:]
}

// isFreeBSDCatalogue reports whether path names a repository catalogue. The
// depth isn't checked, so repositories built with poudriere and served under
// another layout work too; package files are always {name}-{version}.pkg and
// can't take these names.
func isFreeBSDCatalogue(path string) bool {
	return freebsdCataloguePattern.MatchString(freebsdBase(path))
}

// freebsdHashedPattern matches {name}-{version}~{hash}.pkg. Port versions
// never contain '-', so the version is what follows the last one, e.g.
// zsh-navigation-tools-2.2.7~2$imxi7k7w.pkg or
// FreeBSD-syslogd-man-14.snap20261019150821~2$k6xtent4.pkg.
var freebsdHashedPattern = regexp.MustCompile(`^(.+)-([^-~/]+)~[^~/]+\.pkg$`)

// isFreeBSDHashedPackage reports whether path names a package in a Hashed/
// directory: All/Hashed/ for ports, Hashed/ for pkgbase.
func isFreeBSDHashedPackage(path string) bool {
	dir := strings.TrimSuffix(path, "/"+freebsdBase(path))
	return freebsdBase(dir) == freebsdHashedDir && freebsdHashedPattern.MatchString(freebsdBase(path))
}

// parseFreeBSDPackage returns the name and version of a hashed package file.
func parseFreeBSDPackage(filename string) (name, version string) {
	matches := freebsdHashedPattern.FindStringSubmatch(filename)
	if len(matches) != freebsdMatchCount {
		return "", ""
	}
	return matches[1], matches[2]
}

// freebsdUnhashedPattern matches {name}-{version} package files in the current
// (.pkg) and older (.txz, .tzst) packing formats, split at the last '-' like
// freebsdHashedPattern.
var freebsdUnhashedPattern = regexp.MustCompile(`^(.+)-([^-~/]+)\.(?:pkg|txz|tzst)$`)

// freebsdPackageFilePattern matches every package file name, including
// Latest/pkg.pkg, which has no version. Catalogues match it too and must be
// routed first.
var freebsdPackageFilePattern = regexp.MustCompile(`\.(?:pkg|txz|tzst)$`)

// isFreeBSDPackageFile reports whether path names a package file.
func isFreeBSDPackageFile(path string) bool {
	return freebsdPackageFilePattern.MatchString(freebsdBase(path))
}

// parseFreeBSDPackageFile returns the name and version of any package file,
// hashed or not, or empty strings for a file without a version such as
// Latest/pkg.pkg.
func parseFreeBSDPackageFile(filename string) (name, version string) {
	if name, version = parseFreeBSDPackage(filename); name != "" {
		return name, version
	}
	matches := freebsdUnhashedPattern.FindStringSubmatch(filename)
	if len(matches) != freebsdMatchCount {
		return "", ""
	}
	return matches[1], matches[2]
}

// errFreeBSDUnscannable refuses package files that can't be cached while
// scanning covers freebsd.
var errFreeBSDUnscannable = errors.New("package file can't be scanned: only packages under Hashed/ are cached and scanned; build the repository with pkg repo --hash")

// handleUncachedPackage passes a package file outside Hashed/ through. Its
// name may be reused for a rebuild with other bytes, which pkg would then
// reject against the catalogue checksum, so it is never cached. The denylist
// applies when the name carries a version. Scanners only see cached
// artifacts, so the file is refused while one covers freebsd.
func (h *FreeBSDHandler) handleUncachedPackage(w http.ResponseWriter, r *http.Request, upstreamURL, path string) {
	name, version := parseFreeBSDPackageFile(freebsdBase(path))
	if name != "" && h.proxy.versionDenied(freebsdEcosystem, name, version) {
		h.proxy.serveArtifactError(w, fmt.Errorf("%w: %s", ErrVersionDenied, canonicalVersionPURL(freebsdEcosystem, name, version)), "")
		return
	}
	if h.proxy.Scanners.Covers(freebsdEcosystem) {
		h.proxy.Logger.Warn("refusing unscannable freebsd package", "path", path)
		JSONError(w, http.StatusForbidden, errFreeBSDUnscannable.Error())
		return
	}
	h.proxyFile(w, r, upstreamURL, path)
}

// handlePackageDownload caches hashed packages. The cache key holds the
// repository and full path, since every ABI and repository has its own
// builds.
func (h *FreeBSDHandler) handlePackageDownload(w http.ResponseWriter, r *http.Request, repository, upstreamURL, path string) {
	name, version := parseFreeBSDPackage(freebsdBase(path))
	if name == "" {
		h.proxyFile(w, r, upstreamURL, path)
		return
	}

	downloadURL := upstreamURL + "/" + path
	cacheFilename := repository + "/" + path

	h.proxy.Logger.Info("freebsd package download",
		"repository", repository, "name", name, "version", version)

	result, err := h.proxy.GetOrFetchArtifactFromURL(
		r.Context(), freebsdEcosystem, name, version, cacheFilename, downloadURL)
	if err != nil {
		h.proxy.serveArtifactError(w, err, "failed to fetch package")
		return
	}

	if result.Artifact.MediaType == "" {
		result.Artifact.MediaType = freebsdPackageMedia
	}
	ServeArtifactRequest(w, r, result)
}

// handleMetadata serves catalogues unchanged through the metadata cache,
// which keeps their embedded signatures valid.
func (h *FreeBSDHandler) handleMetadata(w http.ResponseWriter, r *http.Request, repository, upstreamURL, path string) {
	h.proxy.ProxyCached(w, r, upstreamURL+"/"+path, freebsdEcosystem,
		h.metadataCacheKey(repository, upstreamURL, path), "*/*")
}

// metadataCacheKey hashes repository, upstream URL and path, as APKHandler does.
func (h *FreeBSDHandler) metadataCacheKey(repository, upstreamURL, path string) string {
	identity := repository + "\x00" + upstreamURL + "\x00" + path
	digest := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(digest[:])
}

// proxyFile passes other files through uncached.
func (h *FreeBSDHandler) proxyFile(w http.ResponseWriter, r *http.Request, upstreamURL, path string) {
	h.proxy.ProxyFile(w, r, upstreamURL+"/"+path)
}
