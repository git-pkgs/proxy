package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/git-pkgs/proxy/internal/metrics"
)

const (
	urlEcosystem = "url"

	// urlVersionHexLen is how much of the request path's sha256 identifies a
	// URL within its host: 128 bits, far beyond any collision risk.
	urlVersionHexLen = 32

	// urlDigestSegment introduces the optional expected digest, as in
	// /url/sha256/{hex}/{host}/{path}.
	urlDigestSegment = "sha256"

	// urlDefaultFilename names an artifact whose URL path has no usable
	// last segment.
	urlDefaultFilename = "download"

	// urlWriteDeadlineSlack is added to the fetch timeout when extending the
	// response write deadline, to leave time for storing and responding.
	urlWriteDeadlineSlack = time.Minute
)

var (
	// urlHostPattern accepts lowercase DNS names. Ports, userinfo and IPv6
	// literals are rejected: the route only fetches https on port 443.
	urlHostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

	urlDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

	// urlFilenameUnsafe matches what is replaced in a cached filename, which
	// becomes a storage key segment and part of a public object URL.
	urlFilenameUnsafe = regexp.MustCompile(`[^A-Za-z0-9._+-]`)
)

// URLHandler caches any public https URL as an immutable artifact. A request
// for /url/{host}/{path}?{query} fetches https://{host}/{path}?{query} once,
// stores it in the artifact cache, and serves every later request from there
// without revalidating. /url/sha256/{hex}/{host}/{path} also checks the bytes
// against that digest: a mismatching download is rejected rather than cached,
// and a cached copy with another digest is refetched.
//
// Unlike GenericHandler this is an open proxy for its clients; it should only
// be reachable by trusted networks. Its fetcher must refuse internal targets
// and must not carry configured upstream credentials.
//
// With DirectServe, GET requests are redirected to the stored object (see
// Proxy.directServeURL), so cache hits never stream through the proxy.
type URLHandler struct {
	proxy        *Proxy
	directServe  bool
	fetchTimeout time.Duration

	// upstreamBase maps a host to the URL its paths are appended to.
	// Tests point it at an httptest server.
	upstreamBase func(host string) string
}

// NewURLHandler creates a /url/ handler. proxy.Fetcher is used for upstream
// downloads; fetchTimeout extends the response write deadline to match it.
func NewURLHandler(proxy *Proxy, directServe bool, fetchTimeout time.Duration) *URLHandler {
	return &URLHandler{
		proxy:        proxy,
		directServe:  directServe,
		fetchTimeout: fetchTimeout,
		upstreamBase: func(host string) string { return "https://" + host },
	}
}

// urlRequest is one parsed /url/ request.
type urlRequest struct {
	host        string
	escapedPath string // with a leading slash, as received
	query       string
	digest      string // lowercase hex sha256, or empty
	version     string
	filename    string
}

// parseURLRequest splits a /url/ request path (prefix already stripped) into
// its target and cache identity.
func parseURLRequest(r *http.Request) (urlRequest, bool) {
	escaped := strings.TrimPrefix(r.URL.EscapedPath(), "/")
	if containsPathTraversal(r.URL.Path) || strings.Contains(r.URL.Path, "\\") {
		return urlRequest{}, false
	}

	var req urlRequest
	first, rest, _ := strings.Cut(escaped, "/")
	if first == urlDigestSegment {
		digest, after, _ := strings.Cut(rest, "/")
		digest = strings.ToLower(digest)
		if !urlDigestPattern.MatchString(digest) {
			return urlRequest{}, false
		}
		req.digest = digest
		escaped = after
	}

	host, tail, ok := strings.Cut(escaped, "/")
	host = strings.ToLower(host)
	if !ok || tail == "" || !validURLHost(host) {
		return urlRequest{}, false
	}
	req.host = host
	req.escapedPath = "/" + tail
	req.query = r.URL.RawQuery

	sum := sha256.Sum256([]byte(req.escapedPath + "?" + req.query))
	req.version = hex.EncodeToString(sum[:])[:urlVersionHexLen]
	req.filename = urlFilename(r.URL.Path)
	return req, true
}

func validURLHost(host string) bool {
	if !urlHostPattern.MatchString(host) {
		return false
	}
	// A bare dotted quad also matches the DNS pattern; accept it only as a
	// well-formed IPv4 address. safehttp still refuses internal ones.
	if ip := net.ParseIP(host); ip != nil {
		return ip.To4() != nil
	}
	return strings.Contains(host, ".")
}

// urlFilename derives a storage-safe filename from the last path segment.
func urlFilename(decodedPath string) string {
	if strings.HasSuffix(decodedPath, "/") {
		return urlDefaultFilename
	}
	name := urlFilenameUnsafe.ReplaceAllString(path.Base(decodedPath), "_")
	if name == "" || name == "." || name == ".." || strings.Trim(name, "_") == "" {
		return urlDefaultFilename
	}
	return name
}

func (req urlRequest) upstreamURL(base string) string {
	u := base + req.escapedPath
	if req.query != "" {
		u += "?" + req.query
	}
	return u
}

// Routes returns the HTTP handler for /url/ requests. Mount it at /url.
func (h *URLHandler) Routes() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		req, ok := parseURLRequest(r)
		if !ok {
			http.Error(w, "expected /url/[sha256/{hex}/]{host}/{path}", http.StatusBadRequest)
			return
		}

		// A cold fetch of a large file can outlast the server's write
		// timeout; this route is bounded by its fetch timeout instead.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(h.fetchTimeout + urlWriteDeadlineSlack))

		downloadURL := req.upstreamURL(h.upstreamBase(req.host))
		h.proxy.Logger.Info("url download",
			"host", req.host, "url", downloadURL, "sha256", req.digest)

		if r.Method == http.MethodGet && !h.directServe {
			h.serveStreamed(w, r, req, downloadURL)
			return
		}
		h.serveStored(w, r, req, downloadURL)
	})
}

// serveStreamed streams the artifact through the proxy. The shared pipeline
// already refetches a record whose object is gone.
func (h *URLHandler) serveStreamed(w http.ResponseWriter, r *http.Request, req urlRequest, downloadURL string) {
	result, err := h.proxy.getOrFetchArtifactFromURL(
		r.Context(), urlEcosystem, req.host, req.version, req.filename, downloadURL, nil, req.digest)
	if err != nil {
		h.proxy.serveArtifactError(w, err, "failed to fetch url")
		return
	}
	h.serve(w, r.Method, result)
}

// serveStored answers HEAD, and GET when redirecting, from the cache record
// without reading the object: a redirect to it, or for HEAD just its headers.
// HEAD is never redirected because a presigned GET URL rejects HEAD.
func (h *URLHandler) serveStored(w http.ResponseWriter, r *http.Request, req urlRequest, downloadURL string) {
	ctx := r.Context()
	p := h.proxy
	result, err := p.urlStoredArtifact(ctx, req, downloadURL)
	if err != nil {
		p.serveArtifactError(w, err, "failed to fetch url")
		return
	}
	if result.Reader != nil {
		_ = result.Reader.Close()
		result.Reader = nil
	}

	if r.Method == http.MethodGet {
		redirect, err := p.directServeURL(ctx, result.storagePath)
		if err != nil {
			p.Logger.Warn("cannot redirect to stored url artifact, streaming it",
				"path", result.storagePath, "error", err)
			streamed, err := p.openStoredArtifact(ctx, result.Artifact, result.storagePath)
			if err != nil {
				p.serveArtifactError(w, err, "failed to read cached url")
				return
			}
			result = streamed
		} else {
			result.RedirectURL = redirect
		}
	}
	h.serve(w, r.Method, result)
}

func (h *URLHandler) serve(w http.ResponseWriter, method string, result *CacheResult) {
	if result.Artifact.MediaType == "" {
		result.Artifact.MediaType = contentTypeOctetStream
	}
	serveArtifact(w, method, result)
}

// urlStoredArtifact returns a cache record for req whose object is known to
// exist, fetching the artifact first on a miss. The result's Reader, if any,
// is for the caller to close.
//
// A record whose object is gone (expired by a bucket lifecycle rule, say) is
// a miss. The shared fetch then finds it cannot open the stored object and
// refetches, replacing the record; if that fetch fails too the error is
// returned, so there is never more than one refetch per request.
func (p *Proxy) urlStoredArtifact(ctx context.Context, req urlRequest, downloadURL string) (*CacheResult, error) {
	if p.versionDenied(urlEcosystem, req.host, req.version) {
		return nil, fmt.Errorf("%w: %s", ErrVersionDenied, canonicalVersionPURL(urlEcosystem, req.host, req.version))
	}
	pkgPURL, versionPURL, err := packagePURLStrings(urlEcosystem, req.host, req.version)
	if err != nil {
		return nil, err
	}

	if cached := p.urlCachedRecord(ctx, pkgPURL, versionPURL, req); cached != nil {
		return cached, nil
	}
	metrics.RecordCacheMiss(urlEcosystem)
	return p.coalescedFetchFromURL(ctx, urlEcosystem, req.host, req.version, req.filename,
		pkgPURL, versionPURL, downloadURL, nil, req.digest)
}

// urlCachedRecord is checkCache for callers that will not read the object:
// it confirms the object exists instead of opening it.
func (p *Proxy) urlCachedRecord(ctx context.Context, pkgPURL, versionPURL string, req urlRequest) *CacheResult {
	record, err := p.DB.GetCachedArtifact(pkgPURL, versionPURL, req.filename)
	if err != nil {
		p.Logger.Warn("failed to read url cache record", "purl", versionPURL, "error", err)
		return nil
	}
	if record == nil {
		return nil
	}
	if _, err := newIntegrityChecks(record.Artifact.Digest.Encoded(), record.Integrity.String); err != nil {
		p.rejectUnusableCacheRecord(record, versionPURL, req.filename, err)
		return nil
	}
	if !artifactHashMatches(record.Artifact.Digest.Encoded(), req.digest) {
		// Left in place; the fetch replacing it discards it under the key.
		return nil
	}

	exists, err := p.Storage.Exists(ctx, record.StoragePath)
	if err != nil {
		metrics.RecordStorageError("exists")
		p.Logger.Warn("failed to check stored url artifact, will refetch",
			"path", record.StoragePath, "error", err)
		return nil
	}
	if !exists {
		metrics.RecordMissingObject(urlEcosystem)
		p.Logger.Warn("cached url artifact missing from storage, will refetch",
			"purl", versionPURL, "path", record.StoragePath)
		return nil
	}

	p.recordCacheHit(urlEcosystem, versionPURL, req.filename)
	return &CacheResult{
		Artifact:    record.Artifact,
		Cached:      true,
		storagePath: record.StoragePath,
	}
}
