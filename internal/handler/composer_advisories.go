package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	composerAdvisoriesPath    = "/api/security-advisories/"
	composerFilterSummaryPath = "/lists/summary.json"
	composerPackagesParam     = "packages"

	// composerAdvisoriesMaxBody caps an advisories query. Composer sends at
	// most 500 package names per request.
	composerAdvisoriesMaxBody = 1 << 20
)

// composerUpstreamIndex holds the parts of the default repository's
// packages.json that the proxy advertises in its own: the security
// advisories API and the filter lists (Packagist's malware list). Composer
// reads advisories and filter entries for each package from its p2 metadata,
// which the proxy passes on unchanged, and asks these URLs for the rest.
type composerUpstreamIndex struct {
	advisoriesURL      string
	advisoriesMetadata bool
	filterLists        json.RawMessage
	filterSummaryURL   string
}

// composerIndexFallbackTTL is how long the upstream index is kept when the
// metadata TTL is 0.
const composerIndexFallbackTTL = time.Hour

// composerIndexCache keeps the default repository's parsed packages.json in
// memory, whether or not metadata caching is on, so that serving the proxy's
// own packages.json does not wait on the upstream. A failed fetch is
// remembered for the same interval: the last good copy, or none, is used
// until then instead of retrying on every request. The zero value is ready
// to use.
type composerIndexCache struct {
	mu        sync.Mutex
	index     composerUpstreamIndex
	ok        bool
	fetchedAt time.Time
	group     singleflight.Group
	now       func() time.Time // for tests; nil means time.Now
}

func (c *composerIndexCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// upstreamIndex returns the default repository's packages.json fields. ok is
// false when the repository has not been reached since the proxy started; a
// document without the fields yields an empty index.
func (h *ComposerHandler) upstreamIndex(ctx context.Context) (composerUpstreamIndex, bool) {
	c := &h.indexCache
	ttl := h.proxy.MetadataTTL
	if ttl <= 0 {
		ttl = composerIndexFallbackTTL
	}
	c.mu.Lock()
	if !c.fetchedAt.IsZero() && c.clock().Sub(c.fetchedAt) < ttl {
		index, ok := c.index, c.ok
		c.mu.Unlock()
		return index, ok
	}
	c.mu.Unlock()

	// One fetch serves every waiting request. It is detached from the
	// request that started it, so a client hanging up neither fails the
	// others nor gets remembered as an upstream failure.
	_, _, _ = c.group.Do("index", func() (any, error) {
		index, err := h.fetchUpstreamIndex(context.WithoutCancel(ctx))
		c.mu.Lock()
		defer c.mu.Unlock()
		if err == nil {
			c.index, c.ok = index, true
		}
		c.fetchedAt = c.clock()
		return nil, nil
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.index, c.ok
}

// fetchUpstreamIndex fetches and parses the default repository's
// packages.json. A document that doesn't parse is rejected before it is
// cached, so a broken response never replaces a good copy: with metadata
// caching the stored copy is used instead, and without it the error keeps
// the copy in memory.
func (h *ComposerHandler) fetchUpstreamIndex(ctx context.Context) (composerUpstreamIndex, error) {
	indexURL := h.repoURL + "/packages.json"
	cacheKey := hashedCacheKey(h.repoURL, "/packages.json")
	validate := func(body []byte) error {
		_, err := parseComposerUpstreamIndex(indexURL, body)
		return err
	}
	res := h.proxy.fetchMetadataDocument(ctx, "composer", cacheKey, indexURL, contentTypeJSON, validate)
	if res.err != nil {
		h.proxy.Logger.Warn("failed to fetch upstream composer packages.json", "url", indexURL, "error", res.err)
		return composerUpstreamIndex{}, res.err
	}
	return parseComposerUpstreamIndex(indexURL, res.body)
}

// errComposerIndexNotObject is returned for a packages.json that is valid
// JSON but not an object, such as null.
var errComposerIndexNotObject = errors.New("composer packages.json is not a JSON object")

// parseComposerUpstreamIndex reads the advisories and filter fields of a
// packages.json. A document without them yields an empty index; one that is
// not a JSON object, or has them in the wrong shape, is an error.
func parseComposerUpstreamIndex(indexURL string, body []byte) (composerUpstreamIndex, error) {
	var doc struct {
		SecurityAdvisories struct {
			Metadata bool   `json:"metadata"`
			APIURL   string `json:"api-url"`
		} `json:"security-advisories"`
		Filter struct {
			Metadata   bool            `json:"metadata"`
			Lists      json.RawMessage `json:"lists"`
			SummaryURL string          `json:"summary-url"`
		} `json:"filter"`
	}
	var index composerUpstreamIndex
	if trimmed := bytes.TrimSpace(body); len(trimmed) == 0 || trimmed[0] != '{' {
		return index, errComposerIndexNotObject
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return index, fmt.Errorf("parsing composer packages.json: %w", err)
	}
	// Composer refuses advisories configured without an api-url unless the
	// repository lists its packages, which the proxy does not.
	if apiURL, ok := resolveComposerURL(indexURL, doc.SecurityAdvisories.APIURL); ok {
		index.advisoriesURL = apiURL
		index.advisoriesMetadata = doc.SecurityAdvisories.Metadata
	}
	// Composer only applies filter lists a repository marks with metadata.
	// A filter api-url is not passed on: without it Composer reads the
	// entries from package metadata, narrowed by the summary.
	if doc.Filter.Metadata && len(doc.Filter.Lists) > 0 && doc.Filter.Lists[0] == '{' {
		index.filterLists = doc.Filter.Lists
		index.filterSummaryURL, _ = resolveComposerURL(indexURL, doc.Filter.SummaryURL)
	}
	return index, nil
}

// resolveComposerURL resolves a URL from packages.json against the document's
// own, as Composer does, and accepts only http and https.
func resolveComposerURL(base, ref string) (string, bool) {
	if ref == "" {
		return "", false
	}
	b, err := url.Parse(base)
	if err != nil {
		return "", false
	}
	u, err := b.Parse(ref)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	return u.String(), true
}

// advertise adds the advisories and filter entries to the proxy's
// packages.json, pointing at the proxy.
func (index composerUpstreamIndex) advertise(packages map[string]any, proxyURL string) {
	if index.advisoriesURL != "" {
		packages["security-advisories"] = map[string]any{
			"metadata": index.advisoriesMetadata,
			"api-url":  proxyURL + "/composer" + composerAdvisoriesPath,
		}
	}
	if index.filterLists != nil {
		filter := map[string]any{"metadata": true, "lists": index.filterLists}
		if index.filterSummaryURL != "" {
			filter["summary-url"] = proxyURL + "/composer" + composerFilterSummaryPath
		}
		packages["filter"] = filter
	}
}

// handleSecurityAdvisories relays a security advisories query to the URL the
// default repository advertises. Composer sends the package names as
// packages[] in a POST form; a GET query is accepted too. Names served by a
// package route or a hosted-only vendor are removed first, so private package
// names are never sent to the public API.
//
// Responses are not cached: each query names a different set of packages.
func (h *ComposerHandler) handleSecurityAdvisories(w http.ResponseWriter, r *http.Request) {
	query, err := h.advisoriesQuery(w, r)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			JSONError(w, http.StatusRequestEntityTooLarge, "advisories request too large")
			return
		}
		JSONError(w, http.StatusBadRequest, "could not read advisories request")
		return
	}

	asked := len(query[composerPackagesParam+"[]"])
	public := h.publicPackageNames(query[composerPackagesParam+"[]"])
	if asked > 0 && len(public) == 0 {
		w.Header().Set(headerContentType, contentTypeJSON)
		_, _ = io.WriteString(w, `{"advisories":{}}`)
		return
	}
	if asked > 0 {
		query[composerPackagesParam+"[]"] = public
	}

	index, ok := h.upstreamIndex(r.Context())
	if !ok {
		JSONError(w, http.StatusBadGateway, "upstream repository unavailable")
		return
	}
	if index.advisoriesURL == "" {
		JSONError(w, http.StatusNotFound, "upstream repository has no security advisories API")
		return
	}

	req, err := h.advisoriesRequest(r, index.advisoriesURL, query)
	if err != nil {
		JSONError(w, http.StatusInternalServerError, "failed to create request")
		return
	}
	h.proxy.Logger.Debug("composer advisories request", "packages", len(public), "withheld", asked-len(public))

	resp, err := h.proxy.HTTPClient.Do(req)
	if err != nil {
		if writeUpstreamHeld(w, err) {
			return
		}
		h.proxy.Logger.Error("composer advisories request failed", "error", err)
		JSONError(w, http.StatusBadGateway, "failed to reach upstream repository")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	h.proxy.relayResponse(w, r, resp, nil)
}

// advisoriesQuery reads the query of an advisories request, from the form
// body of a POST or the URL of a GET. Package names come back under
// "packages[]" whichever spelling the client used: PHP's http_build_query,
// which Composer uses, numbers them as packages[0], packages[1], ...
func (h *ComposerHandler) advisoriesQuery(w http.ResponseWriter, r *http.Request) (url.Values, error) {
	raw := r.URL.RawQuery
	if r.Method == http.MethodPost {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, composerAdvisoriesMaxBody))
		if err != nil {
			return nil, err
		}
		raw = string(body)
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	for key, vals := range values {
		if key == composerPackagesParam || strings.HasPrefix(key, composerPackagesParam+"[") {
			key = composerPackagesParam + "[]"
		}
		query[key] = append(query[key], vals...)
	}
	return query, nil
}

// publicPackageNames drops the names the default repository does not serve.
func (h *ComposerHandler) publicPackageNames(names []string) []string {
	public := make([]string, 0, len(names))
	for _, name := range names {
		if _, routed := h.routes.match(name); routed || h.proxy.hostedOnly("composer", name) {
			continue
		}
		public = append(public, name)
	}
	return public
}

// advisoriesRequest builds the upstream request, sending the query the way
// the client did.
func (h *ComposerHandler) advisoriesRequest(r *http.Request, apiURL string, query url.Values) (*http.Request, error) {
	u, err := url.Parse(apiURL)
	if err != nil {
		return nil, err
	}
	var body io.Reader
	if r.Method == http.MethodPost {
		body = strings.NewReader(query.Encode())
	} else {
		u.RawQuery = joinQuery(u.RawQuery, query.Encode())
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if r.Method == http.MethodPost {
		req.Header.Set(headerContentType, "application/x-www-form-urlencoded")
	}
	for _, header := range []string{headerAccept, headerAcceptEncoding} {
		if v := r.Header.Get(header); v != "" {
			req.Header.Set(header, v)
		}
	}
	return req, nil
}

func joinQuery(a, b string) string {
	if a == "" || b == "" {
		return a + b
	}
	return a + "&" + b
}

// handleFilterSummary relays the filter list summary the default repository
// advertises. It lists every package on a filter list, so the request names
// no packages. It is not cached, so a newly listed package is blocked as soon
// as the upstream lists it; Composer revalidates its own copy with
// If-Modified-Since, which is passed on.
func (h *ComposerHandler) handleFilterSummary(w http.ResponseWriter, r *http.Request) {
	index, ok := h.upstreamIndex(r.Context())
	if !ok {
		JSONError(w, http.StatusBadGateway, "upstream repository unavailable")
		return
	}
	if index.filterSummaryURL == "" {
		JSONError(w, http.StatusNotFound, "upstream repository has no filter list summary")
		return
	}
	h.proxy.ProxyUpstream(w, r, index.filterSummaryURL,
		[]string{headerAccept, headerAcceptEncoding, "If-Modified-Since", "If-None-Match"})
}
