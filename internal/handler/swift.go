package handler

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/packageurl"
)

const (
	swiftAcceptJSON     = "application/vnd.swift.registry.v1+json"
	swiftAcceptManifest = "application/vnd.swift.registry.v1+swift"
	swiftAcceptArchive  = "application/vnd.swift.registry.v1+zip"
	swiftContentVersion = "1"
	swiftMaxScopeLength = 39
	swiftMaxNameLength  = 100
)

// SwiftHandler handles the read-only Swift Package Registry v1 protocol.
type SwiftHandler struct {
	proxy       *Proxy
	upstreamURL string
	proxyURL    string
}

// NewSwiftHandler creates a Swift Package Registry protocol handler.
func NewSwiftHandler(proxy *Proxy, proxyURL, upstreamURL string) *SwiftHandler {
	if strings.TrimSpace(upstreamURL) == "" {
		upstreamURL = config.DefaultSwiftUpstream
	}

	return &SwiftHandler{
		proxy:       proxy,
		upstreamURL: strings.TrimSuffix(upstreamURL, "/"),
		proxyURL:    strings.TrimSuffix(proxyURL, "/"),
	}
}

// Routes returns the HTTP handler for Swift registry requests.
func (h *SwiftHandler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /identifiers", h.handleIdentifiers)
	mux.HandleFunc("GET /{scope}/{name}/{version}/Package.swift", h.handleManifest)
	mux.HandleFunc("GET /{scope}/{name}/{version}", h.handleRelease)
	mux.HandleFunc("PUT /{scope}/{name}/{version}", h.handlePublishingUnsupported)
	mux.HandleFunc("GET /{scope}/{name}", h.handlePackageReleases)
	return mux
}

func (h *SwiftHandler) handlePackageReleases(w http.ResponseWriter, r *http.Request) {
	scope := r.PathValue("scope")
	name := strings.TrimSuffix(r.PathValue("name"), ".json")
	if !validSwiftScope(scope) || !validSwiftPackageName(name) {
		writeSwiftProblem(w, http.StatusBadRequest, "invalid package identifier")
		return
	}
	scope, name = canonicalSwiftPackage(scope, name)

	upstreamURL := h.buildUpstreamURL(scope, name, "", "", r.URL.RawQuery)
	body, contentType, responseHeaders, err := h.fetchMetadataWithHeaders(
		r.Context(), upstreamURL, requestAccept(r, swiftAcceptJSON),
	)
	if err != nil {
		h.writeMetadataError(w, err)
		return
	}

	rewritten, err := h.rewriteReleaseURLs(scope, name, body)
	if err != nil {
		h.proxy.Logger.Warn("failed to rewrite Swift release URLs", "error", err)
		rewritten = body
	}
	for _, link := range responseHeaders.Values("Link") {
		w.Header().Add("Link", h.rewriteLinkHeader(link, upstreamURL))
	}
	writeSwiftMetadata(w, r, rewritten, contentType)
}

func (h *SwiftHandler) handleRelease(w http.ResponseWriter, r *http.Request) {
	scope := r.PathValue("scope")
	name := r.PathValue("name")
	version := r.PathValue("version")
	if strings.HasSuffix(version, ".zip") {
		h.handleSourceArchive(w, r, scope, name, strings.TrimSuffix(version, ".zip"))
		return
	}

	version = strings.TrimSuffix(version, ".json")
	if !validSwiftPackageReference(scope, name, version) {
		writeSwiftProblem(w, http.StatusBadRequest, "invalid package release")
		return
	}
	scope, name = canonicalSwiftPackage(scope, name)

	upstreamURL := h.buildUpstreamURL(scope, name, version, "", r.URL.RawQuery)
	body, contentType, err := h.proxy.FetchOrCacheMetadata(
		r.Context(), "swift", swiftReleaseCacheKey(scope, name, version), upstreamURL, requestAccept(r, swiftAcceptJSON),
	)
	if err != nil {
		h.writeMetadataError(w, err)
		return
	}
	writeSwiftMetadata(w, r, body, contentType)
}

func (h *SwiftHandler) handleManifest(w http.ResponseWriter, r *http.Request) {
	scope := r.PathValue("scope")
	name := r.PathValue("name")
	version := r.PathValue("version")
	if !validSwiftPackageReference(scope, name, version) {
		writeSwiftProblem(w, http.StatusBadRequest, "invalid package release")
		return
	}
	scope, name = canonicalSwiftPackage(scope, name)

	upstreamURL := h.buildUpstreamURL(scope, name, version, "Package.swift", r.URL.RawQuery)
	h.proxySwiftResource(w, r, upstreamURL, swiftAcceptManifest)
}

func (h *SwiftHandler) handleIdentifiers(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("url") == "" {
		writeSwiftProblem(w, http.StatusBadRequest, "url query parameter is required")
		return
	}

	upstreamURL := h.upstreamURL + "/identifiers?" + r.URL.RawQuery
	cacheKey := swiftMetadataCacheKey("identifiers", r.URL.RawQuery)
	body, contentType, err := h.proxy.FetchOrCacheMetadata(
		r.Context(), "swift", cacheKey, upstreamURL, requestAccept(r, swiftAcceptJSON),
	)
	if err != nil {
		h.writeMetadataError(w, err)
		return
	}
	writeSwiftMetadata(w, r, body, contentType)
}

func (h *SwiftHandler) handlePublishingUnsupported(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Allow", "GET, HEAD")
	writeSwiftProblem(w, http.StatusMethodNotAllowed, "publishing isn't supported")
}

func (h *SwiftHandler) handleSourceArchive(w http.ResponseWriter, r *http.Request, scope, name, version string) {
	if !validSwiftPackageReference(scope, name, version) {
		writeSwiftProblem(w, http.StatusBadRequest, "invalid package release")
		return
	}
	scope, name = canonicalSwiftPackage(scope, name)

	packageName := scope + "/" + name
	filename := fmt.Sprintf("%s-%s.zip", name, version)
	upstreamURL := h.buildUpstreamURL(scope, name, version+".zip", "", r.URL.RawQuery)
	packagePURL, versionPURL := packageurl.MakeCacheStrings("swift", packageName, version)
	if packagePURL == "" || versionPURL == "" {
		h.writeArtifactError(w, fmt.Errorf("%w: swift %q", errUnsupportedPackageIdentity, packageName))
		return
	}
	archiveInfo, infoErr := h.fetchArchiveInfo(r.Context(), scope, name, version)
	if infoErr != nil {
		h.writeArtifactError(w, fmt.Errorf("fetching release metadata: %w", infoErr))
		return
	}

	if r.Method == http.MethodHead {
		h.handleSourceArchiveHead(w, r, name, version, filename, packagePURL, versionPURL, upstreamURL, archiveInfo)
		return
	}

	headers := make(http.Header)
	headers.Set("Accept", requestAccept(r, swiftAcceptArchive))
	result, err := h.proxy.getOrFetchArtifactFromURLWithCachePURLs(
		r.Context(), "swift", packageName, version, filename, packagePURL, versionPURL,
		upstreamURL, headers, archiveInfo.checksum,
	)
	if err != nil {
		h.writeArtifactError(w, err)
		return
	}

	result.ContentType = "application/zip"
	setSwiftArchiveHeaders(w.Header(), name, version, result.Hash, archiveInfo)
	serveArtifact(w, r.Method, result)
}

func (h *SwiftHandler) handleSourceArchiveHead(
	w http.ResponseWriter,
	r *http.Request,
	name, version, filename, packagePURL, versionPURL, upstreamURL string,
	archiveInfo swiftArchiveInfo,
) {
	result, err := h.proxy.getCachedArtifactWithUpstreamHash(
		r.Context(), packagePURL, versionPURL, filename, archiveInfo.checksum,
	)
	if err != nil {
		h.writeArtifactError(w, err)
		return
	}
	if result != nil {
		result.ContentType = "application/zip"
		setSwiftArchiveHeaders(w.Header(), name, version, result.Hash, archiveInfo)
		serveArtifact(w, r.Method, result)
		return
	}

	size, err := h.probeSourceArchive(r.Context(), upstreamURL, requestAccept(r, swiftAcceptArchive))
	if err != nil {
		h.writeArtifactError(w, err)
		return
	}
	setSwiftArchiveHeaders(w.Header(), name, version, "", archiveInfo)
	w.Header().Set("Content-Type", "application/zip")
	if size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	w.WriteHeader(http.StatusOK)
}

func (h *SwiftHandler) probeSourceArchive(ctx context.Context, upstreamURL, accept string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamURL, nil)
	if err != nil {
		return 0, fmt.Errorf("creating upstream archive request: %w", err)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("Range", "bytes=0-0")
	h.proxy.applyUpstreamAuth(req)

	resp, err := h.proxy.HTTPClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("requesting upstream archive: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return 0, ErrUpstreamNotFound
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("upstream archive returned %d", resp.StatusCode)
	}

	if resp.StatusCode == http.StatusPartialContent {
		_, total, found := strings.Cut(resp.Header.Get("Content-Range"), "/")
		if !found || total == "*" {
			return -1, nil
		}
		if parsed, parseErr := strconv.ParseInt(total, 10, 64); parseErr == nil {
			return parsed, nil
		}
		return -1, nil
	}

	size := int64(-1)
	if contentLength := resp.Header.Get("Content-Length"); contentLength != "" {
		if parsed, parseErr := strconv.ParseInt(contentLength, 10, 64); parseErr == nil {
			size = parsed
		}
	}
	return size, nil
}

func (h *SwiftHandler) fetchMetadataWithHeaders(
	ctx context.Context,
	upstreamURL, accept string,
) ([]byte, string, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamURL, nil)
	if err != nil {
		return nil, "", nil, fmt.Errorf("creating upstream metadata request: %w", err)
	}
	req.Header.Set("Accept", accept)
	h.proxy.applyUpstreamAuth(req)

	resp, err := h.proxy.HTTPClient.Do(req)
	if err != nil {
		return nil, "", nil, fmt.Errorf("requesting upstream metadata: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, "", nil, ErrUpstreamNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", nil, fmt.Errorf("upstream metadata returned %d", resp.StatusCode)
	}

	body, err := h.proxy.ReadMetadata(resp.Body)
	if err != nil {
		return nil, "", nil, fmt.Errorf("reading upstream metadata: %w", err)
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = contentTypeJSON
	}
	return body, contentType, resp.Header.Clone(), nil
}

type swiftReleaseMetadata struct {
	Resources []struct {
		Name     string `json:"name"`
		Type     string `json:"type"`
		Checksum string `json:"checksum"`
		Signing  *struct {
			Signature string `json:"signatureBase64Encoded"`
			Format    string `json:"signatureFormat"`
		} `json:"signing"`
	} `json:"resources"`
}

type swiftArchiveInfo struct {
	checksum        string
	signature       string
	signatureFormat string
}

func (h *SwiftHandler) fetchArchiveInfo(ctx context.Context, scope, name, version string) (swiftArchiveInfo, error) {
	upstreamURL := h.buildUpstreamURL(scope, name, version, "", "")
	body, _, err := h.proxy.FetchOrCacheMetadata(
		ctx, "swift", swiftReleaseCacheKey(scope, name, version), upstreamURL, swiftAcceptJSON,
	)
	if err != nil {
		return swiftArchiveInfo{}, err
	}

	var metadata swiftReleaseMetadata
	if err := json.Unmarshal(body, &metadata); err != nil {
		return swiftArchiveInfo{}, fmt.Errorf("parsing release metadata: %w", err)
	}
	for _, resource := range metadata.Resources {
		if resource.Name != "source-archive" || resource.Type != "application/zip" {
			continue
		}
		checksum, err := normalizeSwiftChecksum(resource.Checksum)
		if err != nil {
			return swiftArchiveInfo{}, err
		}
		info := swiftArchiveInfo{checksum: checksum}
		if resource.Signing != nil {
			if resource.Signing.Signature == "" || resource.Signing.Format == "" {
				return swiftArchiveInfo{}, errors.New("source archive signing metadata is incomplete")
			}
			info.signature = resource.Signing.Signature
			info.signatureFormat = resource.Signing.Format
		}
		return info, nil
	}

	return swiftArchiveInfo{}, errors.New("source archive is missing from release metadata")
}

func normalizeSwiftChecksum(checksum string) (string, error) {
	digest, err := hex.DecodeString(checksum)
	if err != nil || len(digest) != sha256.Size {
		return "", errors.New("source archive checksum is not a SHA-256 digest")
	}
	return hex.EncodeToString(digest), nil
}

func setSwiftArchiveHeaders(header http.Header, name, version, contentHash string, info swiftArchiveInfo) {
	header.Set("Cache-Control", "public, immutable")
	header.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.zip"`, name, version))
	header.Set("Content-Version", swiftContentVersion)

	checksum := info.checksum
	if checksum == "" {
		checksum = contentHash
	}
	if digest := swiftDigestHeader(checksum); digest != "" {
		header.Set("Digest", digest)
	}
	if info.signature != "" && info.signatureFormat != "" {
		header.Set("X-Swift-Package-Signature", info.signature)
		header.Set("X-Swift-Package-Signature-Format", info.signatureFormat)
	}
}

func swiftDigestHeader(checksum string) string {
	digest, err := hex.DecodeString(checksum)
	if err != nil || len(digest) != sha256.Size {
		return ""
	}
	return "sha-256=" + base64.StdEncoding.EncodeToString(digest)
}

func (h *SwiftHandler) proxySwiftResource(w http.ResponseWriter, r *http.Request, upstreamURL, defaultAccept string) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, nil)
	if err != nil {
		writeSwiftProblem(w, http.StatusInternalServerError, "failed to create upstream request")
		return
	}
	req.Header.Set("Accept", requestAccept(r, defaultAccept))
	for _, name := range []string{"If-Modified-Since", "If-None-Match"} {
		if value := r.Header.Get(name); value != "" {
			req.Header.Set(name, value)
		}
	}
	h.proxy.applyUpstreamAuth(req)

	resp, err := h.proxy.HTTPClient.Do(req)
	if err != nil {
		writeSwiftProblem(w, http.StatusBadGateway, "upstream request failed")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	copySwiftResponseHeaders(w.Header(), resp.Header)
	if location := resp.Header.Get("Location"); location != "" {
		w.Header().Set("Location", h.rewriteRegistryURL(location, upstreamURL))
	}
	for _, link := range resp.Header.Values("Link") {
		w.Header().Add("Link", h.rewriteLinkHeader(link, upstreamURL))
	}
	if w.Header().Get("Content-Version") == "" {
		w.Header().Set("Content-Version", swiftContentVersion)
	}

	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, resp.Body)
	}
}

func copySwiftResponseHeaders(dst, src http.Header) {
	for _, name := range []string{
		"Cache-Control", "Content-Disposition", "Content-Language", "Content-Length",
		"Content-Type", "Content-Version", "Digest", "ETag", "Last-Modified",
		"Retry-After", "Vary", "Warning", "X-Swift-Package-Signature",
		"X-Swift-Package-Signature-Format",
	} {
		for _, value := range src.Values(name) {
			dst.Add(name, value)
		}
	}
}

func (h *SwiftHandler) rewriteLinkHeader(value, upstreamRequestURL string) string {
	var result strings.Builder
	for len(value) > 0 {
		start := strings.IndexByte(value, '<')
		if start < 0 {
			result.WriteString(value)
			break
		}
		endOffset := strings.IndexByte(value[start+1:], '>')
		if endOffset < 0 {
			result.WriteString(value)
			break
		}
		end := start + 1 + endOffset
		result.WriteString(value[:start+1])
		result.WriteString(h.rewriteRegistryURL(value[start+1:end], upstreamRequestURL))
		result.WriteByte('>')
		value = value[end+1:]
	}
	return result.String()
}

func (h *SwiftHandler) rewriteRegistryURL(rawURL, upstreamRequestURL string) string {
	base, err := url.Parse(h.upstreamURL)
	if err != nil {
		return rawURL
	}
	requestURL, err := url.Parse(upstreamRequestURL)
	if err != nil {
		return rawURL
	}
	reference, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	absolute := requestURL.ResolveReference(reference)
	if !strings.EqualFold(absolute.Scheme, base.Scheme) || !strings.EqualFold(absolute.Host, base.Host) {
		return rawURL
	}

	basePath := strings.TrimSuffix(base.EscapedPath(), "/")
	absolutePath := absolute.EscapedPath()
	if absolutePath != basePath && !strings.HasPrefix(absolutePath, basePath+"/") {
		return rawURL
	}
	suffix := strings.TrimPrefix(absolutePath, basePath)
	rewritten := h.proxyURL + "/swift" + suffix
	if absolute.RawQuery != "" {
		rewritten += "?" + absolute.RawQuery
	}
	if absolute.Fragment != "" {
		rewritten += "#" + absolute.Fragment
	}
	return rewritten
}

func (h *SwiftHandler) rewriteReleaseURLs(scope, name string, body []byte) ([]byte, error) {
	var metadata map[string]any
	if err := json.Unmarshal(body, &metadata); err != nil {
		return nil, err
	}
	releases, ok := metadata["releases"].(map[string]any)
	if !ok {
		return body, nil
	}

	for version, value := range releases {
		release, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if _, hasURL := release["url"]; !hasURL {
			continue
		}
		release["url"] = fmt.Sprintf(
			"%s/swift/%s/%s/%s",
			h.proxyURL,
			url.PathEscape(scope),
			url.PathEscape(name),
			url.PathEscape(version),
		)
	}
	return json.Marshal(metadata)
}

func (h *SwiftHandler) buildUpstreamURL(scope, name, version, resource, rawQuery string) string {
	parts := []string{h.upstreamURL, url.PathEscape(scope), url.PathEscape(name)}
	if version != "" {
		parts = append(parts, url.PathEscape(version))
	}
	if resource != "" {
		parts = append(parts, resource)
	}
	result := strings.Join(parts, "/")
	if rawQuery != "" {
		result += "?" + rawQuery
	}
	return result
}

func swiftMetadataCacheKey(parts ...string) string {
	joined := strings.Join(parts, "\x00")
	digest := sha256.Sum256([]byte(joined))
	return hex.EncodeToString(digest[:])
}

func swiftReleaseCacheKey(scope, name, version string) string {
	return swiftMetadataCacheKey("release", scope, name, version)
}

func requestAccept(r *http.Request, fallback string) string {
	if accept := r.Header.Get("Accept"); accept != "" {
		return accept
	}
	return fallback
}

func writeSwiftMetadata(w http.ResponseWriter, r *http.Request, body []byte, contentType string) {
	if contentType == "" {
		contentType = "application/json"
	}
	digest := sha256.Sum256(body)
	etag := fmt.Sprintf(`"%x"`, digest)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Version", swiftContentVersion)
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

func (h *SwiftHandler) writeMetadataError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrUpstreamNotFound) {
		writeSwiftProblem(w, http.StatusNotFound, "not found")
		return
	}
	h.proxy.Logger.Error("Swift metadata request failed", "error", err)
	writeSwiftProblem(w, http.StatusBadGateway, "upstream request failed")
}

func (h *SwiftHandler) writeArtifactError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrUpstreamNotFound) {
		writeSwiftProblem(w, http.StatusNotFound, "release not found")
		return
	}
	h.proxy.Logger.Error("Swift archive request failed", "error", err)
	writeSwiftProblem(w, http.StatusBadGateway, "failed to fetch package")
}

func writeSwiftProblem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Content-Version", swiftContentVersion)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"detail": detail})
}

func validSwiftPackageReference(scope, name, version string) bool {
	return validSwiftScope(scope) && validSwiftPackageName(name) && version != "" && version != "." && version != ".." && !strings.ContainsAny(version, "/\\")
}

func canonicalSwiftPackage(scope, name string) (string, string) {
	return strings.ToLower(scope), strings.ToLower(name)
}

func validSwiftScope(scope string) bool {
	return validSwiftIdentifier(scope, swiftMaxScopeLength, "-")
}

func validSwiftPackageName(name string) bool {
	return validSwiftIdentifier(name, swiftMaxNameLength, "-_")
}

func validSwiftIdentifier(value string, maxLength int, separators string) bool {
	if value == "" || len(value) > maxLength {
		return false
	}
	previousSeparator := false
	for i := 0; i < len(value); i++ {
		character := value[i]
		separator := strings.ContainsRune(separators, rune(character))
		if separator {
			if i == 0 || i == len(value)-1 || previousSeparator {
				return false
			}
			previousSeparator = true
			continue
		}
		if (character < 'a' || character > 'z') &&
			(character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') {
			return false
		}
		previousSeparator = false
	}
	return true
}
