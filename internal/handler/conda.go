package handler

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/git-pkgs/proxy/internal/jsonstream"
	"github.com/git-pkgs/proxy/internal/retention"
	"github.com/git-pkgs/purl"
)

const (
	condaUpstream = "https://conda.anaconda.org"
	minCondaParts = 3 // name-version-build requires at least 3 hyphen-separated parts
)

// Packages are stored under the name {channel}/{name}, which purl writes as
// pkg:conda/{channel}%2F{name}. Package rules name the channel as the
// namespace, pkg:conda/conda-forge/numpy, and are mapped to that form.
func init() {
	retention.Default.Register(retention.Spec{Key: "conda", CanonicalPackage: condaRetentionKey})
}

// condaRetentionKey maps a package rule to the stored package PURL. It
// takes the channel from the namespace, or from the name in the stored
// form. Downloads are routed as /{channel}/{arch}/{filename}, so the channel
// is always one path segment; a key without a channel, with more segments
// or with an empty one is refused, since no stored package looks like that.
// The package name is lowercased the way conda treats it; the channel stays
// as the URL had it.
func condaRetentionKey(p *purl.PURL) (string, bool) {
	if p == nil {
		return "", false
	}
	full := p.Name
	if p.Namespace != "" {
		full = p.Namespace + "/" + p.Name
	}
	segments := strings.Split(full, "/")
	if len(segments) != 2 || slices.Contains(segments, "") {
		return "", false
	}
	channel, name := segments[0], strings.ToLower(segments[1])
	return retention.DefaultCanonical("conda")(purl.New(p.Type, "", channel+"/"+name, "", nil))
}

// CondaHandler handles Conda/Anaconda registry protocol requests.
type CondaHandler struct {
	proxy       *Proxy
	upstreamURL string
	proxyURL    string
}

// NewCondaHandler creates a new Conda protocol handler.
func NewCondaHandler(proxy *Proxy, proxyURL string) *CondaHandler {
	return &CondaHandler{
		proxy:       proxy,
		upstreamURL: condaUpstream,
		proxyURL:    strings.TrimSuffix(proxyURL, "/"),
	}
}

// NewCondaHandlerWithUpstream creates a Conda handler with a custom upstream.
func NewCondaHandlerWithUpstream(proxy *Proxy, proxyURL, upstreamURL string) *CondaHandler {
	h := NewCondaHandler(proxy, proxyURL)
	h.upstreamURL = configuredUpstreamURL(upstreamURL, condaUpstream)
	return h
}

// Routes returns the HTTP handler for Conda requests.
func (h *CondaHandler) Routes() http.Handler {
	mux := http.NewServeMux()

	// Channel index (repodata)
	mux.HandleFunc("GET /{channel}/{arch}/repodata.json", h.handleRepodata)
	mux.HandleFunc("GET /{channel}/{arch}/repodata.json.bz2", h.handleRepodataVariant)
	mux.HandleFunc("GET /{channel}/{arch}/current_repodata.json", h.handleRepodata)
	mux.HandleFunc("GET /{channel}/{arch}/repodata_from_packages.json", h.handleRepodata)

	// Sharded repodata (CEP 16)
	mux.HandleFunc("GET /{channel}/{arch}/"+condaShardIndexFile, h.handleShardIndex)
	mux.HandleFunc("GET /{channel}/{arch}/shards/{filename}", h.handleShard)
	mux.HandleFunc("GET /{channel}/{arch}/"+condaCooldownShardDir+"/{bucket}/{rest...}", h.handleCooldownShard)

	// Package downloads (cache these)
	mux.HandleFunc("GET /{channel}/{arch}/{filename}", h.handleDownload)

	return mux
}

// handleDownload serves a package file, fetching and caching from upstream if needed.
func (h *CondaHandler) handleDownload(w http.ResponseWriter, r *http.Request) {
	channel := r.PathValue("channel")
	arch := r.PathValue("arch")
	filename := r.PathValue("filename")

	if strings.HasSuffix(filename, condaShardSuffix) {
		h.handleShard(w, r)
		return
	}

	// Only cache actual package files
	if !h.isPackageFile(filename) {
		if isCondaRepodataVariant(filename) && h.cooldownEnabled() {
			refuseUnfilteredRepodata(w)
			return
		}
		h.proxyUpstream(w, r)
		return
	}

	name, version := h.parseFilename(filename)
	if name == "" {
		h.proxyUpstream(w, r)
		return
	}

	// Include channel in package name
	packageName := channel + "/" + name

	h.proxy.Logger.Debug("conda download request",
		"channel", channel, "arch", arch, "name", name, "version", version)

	// Builds and subdirs of one version each have their own timestamp, so
	// the check is per file and keeps no time in the shared versions row.
	if h.proxy.withholdInCooldown(w, downloadVersion{
		ecosystem: "conda", policyName: name, version: version,
		lookup: func() (time.Time, bool, error) { return h.publishTime(r, channel, arch, filename) },
	}) {
		return
	}

	upstreamURL := h.upstreamURL + r.URL.Path
	// The same filename can hold different bytes in each subdir, so the
	// subdir is part of the cache key.
	cacheFilename := arch + "/" + filename

	result, err := h.proxy.GetOrFetchArtifactFromURL(r.Context(), "conda", packageName, version, cacheFilename, upstreamURL)
	if err != nil {
		h.proxy.serveArtifactError(w, err, "failed to fetch package")
		return
	}

	ServeArtifactRequest(w, r, result)
}

// isPackageFile returns true if the filename is a Conda package.
func (h *CondaHandler) isPackageFile(filename string) bool {
	return strings.HasSuffix(filename, ".tar.bz2") || strings.HasSuffix(filename, ".conda")
}

// parseFilename extracts name and version from a Conda package filename.
func (h *CondaHandler) parseFilename(filename string) (name, version string) {
	return CondaFilenameIdentity(filename)
}

// CondaFilenameIdentity returns the name and version a download of filename
// is cached under, or empty strings when it is not a package file name.
// Conda filenames are: {name}-{version}-{build}.{ext}
// e.g., "numpy-1.24.0-py311h64a7726_0.conda"
func CondaFilenameIdentity(filename string) (name, version string) {
	// Remove extension
	base := filename
	for _, ext := range []string{".conda", ".tar.bz2"} {
		if strings.HasSuffix(base, ext) {
			base = strings.TrimSuffix(base, ext)
			break
		}
	}

	// Split by hyphens, the format is name-version-build
	// The name can contain hyphens, so we need to find version-build at the end
	parts := strings.Split(base, "-")
	if len(parts) < minCondaParts {
		return "", ""
	}

	// Build is the last part, version is second to last
	// Everything before is the name
	build := parts[len(parts)-1]
	version = parts[len(parts)-2]
	name = strings.Join(parts[:len(parts)-2], "-")

	// Version should start with a digit
	if len(version) == 0 || version[0] < '0' || version[0] > '9' {
		return "", ""
	}

	// Include build in version for uniqueness
	_ = build

	return name, version
}

// isCondaRepodataVariant reports whether filename is a form of a subdir's
// repodata other than the plain JSON documents the proxy filters, such as
// repodata.json.zst, repodata.json.bz2 or repodata.jlap patches. Package
// files are told apart by the caller.
func isCondaRepodataVariant(filename string) bool {
	return strings.HasPrefix(filename, "repodata") || strings.HasPrefix(filename, "current_repodata")
}

// handleRepodataVariant serves repodata.json.bz2 from the metadata cache,
// unless cooldown is on: the proxy filters only the JSON, so a compressed
// copy would list versions still in cooldown.
func (h *CondaHandler) handleRepodataVariant(w http.ResponseWriter, r *http.Request) {
	if h.cooldownEnabled() {
		refuseUnfilteredRepodata(w)
		return
	}
	h.proxyCached(w, r)
}

// refuseUnfilteredRepodata answers 404 for a repodata variant the proxy
// can't filter. conda and mamba then fall back to repodata.json.
func refuseUnfilteredRepodata(w http.ResponseWriter) {
	http.Error(w, "only repodata.json is served while cooldown is enabled", http.StatusNotFound)
}

// handleRepodata proxies repodata.json, applying cooldown filtering when enabled.
func (h *CondaHandler) handleRepodata(w http.ResponseWriter, r *http.Request) {
	if !h.cooldownEnabled() {
		h.proxyCached(w, r)
		return
	}

	upstreamURL := h.upstreamURL + r.URL.Path

	h.proxy.Logger.Debug("fetching repodata for cooldown filtering", "url", upstreamURL)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
	}
	req.Header.Set(headerAcceptEncoding, "gzip")

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

	if resp.StatusCode != http.StatusOK {
		h.proxy.relayResponse(w, r, resp, nil)
		return
	}

	// Explicitly requesting gzip disables the transport's automatic decoding.
	// Cooldown needs JSON, so decode here and bound the decompressed size too.
	var reader io.Reader = resp.Body
	if strings.EqualFold(strings.TrimSpace(resp.Header.Get(headerContentEncoding)), "gzip") {
		decoded, err := gzip.NewReader(resp.Body)
		if err != nil {
			http.Error(w, "failed to decode response", http.StatusBadGateway)
			return
		}
		defer func() { _ = decoded.Close() }()
		reader = decoded
	}
	// conda-forge's linux-64 repodata.json is several hundred MB, so it is
	// filtered as it streams rather than read through ReadMetadata. Once
	// output has started, an error aborts the response so the client never
	// takes a truncated or unfiltered index as complete.
	out := &countingWriter{w: w}
	buffered := bufio.NewWriterSize(out, repodataWriteBuffer)
	w.Header().Set(headerContentType, "application/json")
	err = h.filterRepodata(io.LimitReader(reader, maxRepodataSize), buffered)
	if err == nil {
		err = buffered.Flush()
	}
	if err != nil {
		h.proxy.Logger.Error("failed to filter conda repodata", "url", upstreamURL, "error", err)
		if out.n == 0 {
			http.Error(w, "failed to read response", http.StatusBadGateway)
			return
		}
		panic(http.ErrAbortHandler)
	}
}

// countingWriter records whether anything has reached the response yet.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// condaTimestampDivisor converts Conda's millisecond timestamps to seconds.
const condaTimestampDivisor = 1000

const (
	repodataWriteBuffer = 64 << 10
	// maxRepodataSize stops a runaway upstream body. It is well above the
	// largest conda-forge index, which is under 500 MB.
	maxRepodataSize = 4 << 30
)

// filterRepodata copies repodata.json from r to w, leaving out entries in
// packages and packages.conda that are still inside their cooldown. Kept
// entries and every other member are written with their upstream bytes.
func (h *CondaHandler) filterRepodata(r io.Reader, w io.Writer) error {
	return jsonstream.CopyObject(json.NewDecoder(r), w, func(m *jsonstream.Member) error {
		if m.Key != "packages" && m.Key != "packages.conda" {
			return m.Copy()
		}
		return m.Object(func(entry *jsonstream.Member) error {
			raw, err := entry.Raw()
			if err != nil {
				return err
			}
			if h.inCooldown(entry.Key, raw) {
				return nil
			}
			return entry.Write(raw)
		})
	})
}

type repodataEntry struct {
	Name      string  `json:"name"`
	Version   string  `json:"version"`
	Timestamp float64 `json:"timestamp"`
}

// inCooldown reports whether a repodata entry was published too recently.
// Entries without a name or timestamp are kept.
func (h *CondaHandler) inCooldown(filename string, raw []byte) bool {
	var entry repodataEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return false
	}
	return h.recordInCooldown(filename, entry.Name, entry.Version, entry.Timestamp)
}

// recordInCooldown reports whether a package record with a millisecond
// timestamp was published too recently. Records without a name or
// timestamp are kept.
func (h *CondaHandler) recordInCooldown(filename, name, version string, timestamp float64) bool {
	if name == "" || timestamp == 0 {
		return false
	}
	publishedAt := time.Unix(int64(timestamp)/condaTimestampDivisor, 0)
	if h.proxy.Cooldown.IsAllowed("conda", canonicalPackagePURL("conda", name), publishedAt) {
		return false
	}
	h.proxy.Logger.Info("cooldown: filtering conda package",
		"name", name, "version", version, "filename", filename)
	return true
}

// metadataCacheKey is the metadata cache key of a request path.
func (h *CondaHandler) metadataCacheKey(urlPath string) string {
	return hashedCacheKey(h.upstreamURL, urlPath)
}

// publishTime reads a package file's timestamp from the subdir's
// repodata.json, fetched with the cache key and encoding proxyCached uses.
// The repodata is indexed once per document (see publishIndex), since a
// subdir's index runs to hundreds of megabytes and an install downloads many
// files from it. A compressed copy over metadata_max_size can't be cached,
// so it is streamed from upstream for each check, as handleRepodata does.
func (h *CondaHandler) publishTime(r *http.Request, channel, arch, filename string) (time.Time, bool, error) {
	repodataPath := "/" + channel + "/" + arch + "/repodata.json"
	cacheKey := h.metadataCacheKey(repodataPath)
	res := h.proxy.lookupMetadata(r.Context(), "conda", cacheKey, h.upstreamURL+repodataPath, "*/*", "gzip", nil)
	var index map[string]time.Time
	var err error
	switch {
	case errors.Is(res.err, ErrMetadataTooLarge):
		index, err = h.streamRepodataIndex(r, h.upstreamURL+repodataPath)
	case res.err != nil:
		err = res.err
	default:
		index, err = publishIndex(h.proxy, "conda\x00"+cacheKey, res, h.indexRepodata)
	}
	if err != nil {
		return time.Time{}, false, err
	}
	published, ok := index[filename]
	return published, ok, nil
}

// indexRepodata indexes a cached repodata.json body.
func (h *CondaHandler) indexRepodata(body []byte, encoding string) (map[string]time.Time, error) {
	var reader io.Reader = bytes.NewReader(body)
	if strings.EqualFold(strings.TrimSpace(encoding), "gzip") {
		decoded, err := gzip.NewReader(reader)
		if err != nil {
			return nil, err
		}
		reader = decoded
	}
	return readRepodataIndex(reader)
}

// streamRepodataIndex indexes repodata.json as it streams from upstream.
func (h *CondaHandler) streamRepodataIndex(r *http.Request, upstreamURL string) (map[string]time.Time, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(headerAcceptEncoding, "gzip")
	resp, err := h.proxy.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("repodata returned %d", resp.StatusCode)
	}
	var reader io.Reader = resp.Body
	if strings.EqualFold(strings.TrimSpace(resp.Header.Get(headerContentEncoding)), "gzip") {
		decoded, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		defer func() { _ = decoded.Close() }()
		reader = decoded
	}
	return readRepodataIndex(reader)
}

// readRepodataIndex maps each package file in a repodata.json to its
// timestamp, leaving out files without one. It reads the document as it
// streams, under the same bound as filterRepodata, so the decompressed size
// is not limited by metadata_max_size.
func readRepodataIndex(r io.Reader) (map[string]time.Time, error) {
	index := make(map[string]time.Time)
	err := jsonstream.CopyObject(json.NewDecoder(io.LimitReader(r, maxRepodataSize)), io.Discard, func(m *jsonstream.Member) error {
		if m.Key != "packages" && m.Key != "packages.conda" {
			return m.Copy()
		}
		return m.Object(func(entry *jsonstream.Member) error {
			raw, err := entry.Raw()
			if err != nil {
				return err
			}
			var e repodataEntry
			if json.Unmarshal(raw, &e) == nil && e.Timestamp != 0 {
				index[entry.Key] = time.Unix(int64(e.Timestamp)/condaTimestampDivisor, 0)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return index, nil
}

// proxyCached forwards a metadata request with caching.
func (h *CondaHandler) proxyCached(w http.ResponseWriter, r *http.Request) {
	cacheKey := h.metadataCacheKey(r.URL.Path)
	// Large JSON indexes can exceed the metadata limit uncompressed. Conda
	// clients decode Content-Encoding, so cache and relay gzip verbatim. The
	// already-compressed repodata.json.bz2 route must stay on identity.
	acceptEncoding := "identity"
	if strings.HasSuffix(r.URL.Path, ".json") {
		acceptEncoding = "gzip"
	}
	h.proxy.proxyCachedWithEncoding(w, r, h.upstreamURL+r.URL.Path, "conda", cacheKey, acceptEncoding, "*/*")
}

// proxyUpstream forwards a request to Anaconda without caching.
func (h *CondaHandler) proxyUpstream(w http.ResponseWriter, r *http.Request) {
	h.proxy.ProxyUpstream(w, r, h.upstreamURL+r.URL.Path, []string{headerAcceptEncoding})
}
