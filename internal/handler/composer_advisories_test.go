package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
)

// composerAdvisoryUpstream serves a packages.json advertising security
// advisories and a malware filter list, and records the advisories and
// summary requests it gets.
type composerAdvisoryUpstream struct {
	*httptest.Server
	index string
	down  atomic.Bool // answer every request with 503
	// malformed answers packages.json with an HTML page and status 200.
	malformed atomic.Bool

	mu       sync.Mutex
	requests []*recordedRequest
}

type recordedRequest struct {
	method string
	path   string
	query  url.Values
	body   string
	header http.Header
}

const composerAdvisoriesResponse = `{"advisories":{"public/library":[{"advisoryId":"PKSA-1","affectedVersions":"<1.0.1"}]}}`

const composerFilterSummary = `{"filter":{"malware":{"evil/package":"1.0.0"}}}`

func newComposerAdvisoryUpstream(t *testing.T) *composerAdvisoryUpstream {
	t.Helper()
	u := &composerAdvisoryUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.requests = append(u.requests, &recordedRequest{r.Method, r.URL.Path, r.URL.Query(), string(body), r.Header.Clone()})
		u.mu.Unlock()
		if u.down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/packages.json":
			if u.malformed.Load() {
				w.Header().Set("Content-Type", "text/html")
				_, _ = io.WriteString(w, "<html><body>Service temporarily unavailable</body></html>")
				return
			}
			_, _ = io.WriteString(w, u.index)
		case "/api/security-advisories/":
			_, _ = io.WriteString(w, composerAdvisoriesResponse)
		case "/lists/all/summary.json":
			if r.Header.Get("If-Modified-Since") != "" {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			_, _ = io.WriteString(w, composerFilterSummary)
		default:
			http.NotFound(w, r)
		}
	}))
	// Like Packagist's: the API on one host, the summary as a relative URL.
	u.index = `{
		"packages": [],
		"metadata-url": "/p2/%package%.json",
		"security-advisories": {"metadata": true, "api-url": "` + u.URL + `/api/security-advisories/"},
		"filter": {"metadata": true, "lists": {"malware": {"enabled": true}}, "summary-url": "/lists/all/summary.json"}
	}`
	t.Cleanup(u.Close)
	return u
}

// requestsTo returns the recorded requests for path.
func (u *composerAdvisoryUpstream) requestsTo(path string) []*recordedRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []*recordedRequest
	for _, r := range u.requests {
		if r.path == path {
			out = append(out, r)
		}
	}
	return out
}

func newAdvisoryComposerHandler(t *testing.T, upstream *composerAdvisoryUpstream) *ComposerHandler {
	t.Helper()
	proxy, _, _, _ := setupTestProxy(t)
	proxy.HostedEnabled = true
	proxy.HostedComposerVendors = []string{"acme"}
	return NewComposerHandlerWithUpstreams(proxy, "http://proxy.test", upstream.URL, upstream.URL).
		WithPackageRoutes(map[string]string{"example/*": "https://packages.example.com/composer"})
}

func serveComposerIndex(t *testing.T, h *ComposerHandler) map[string]any {
	t.Helper()
	w := serveComposer(h, "/packages.json")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body)
	}
	var index map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &index); err != nil {
		t.Fatalf("parsing packages.json: %v", err)
	}
	return index
}

func TestComposerServiceIndexAdvertisesAdvisoriesAndFilter(t *testing.T) {
	upstream := newComposerAdvisoryUpstream(t)
	index := serveComposerIndex(t, newAdvisoryComposerHandler(t, upstream))

	advisories, _ := index["security-advisories"].(map[string]any)
	if advisories["api-url"] != "http://proxy.test/composer/api/security-advisories/" || advisories["metadata"] != true {
		t.Errorf("security-advisories = %v, want the proxy's api-url with metadata", index["security-advisories"])
	}
	filter, _ := index["filter"].(map[string]any)
	if filter["summary-url"] != "http://proxy.test/composer/lists/summary.json" || filter["metadata"] != true {
		t.Errorf("filter = %v, want the proxy's summary-url with metadata", index["filter"])
	}
	lists, _ := filter["lists"].(map[string]any)
	if _, ok := lists["malware"]; !ok {
		t.Errorf("filter lists = %v, want upstream's malware list", filter["lists"])
	}
	if index["metadata-url"] != "http://proxy.test/composer/p2/%package%.json" {
		t.Errorf("metadata-url = %v, want the proxy's", index["metadata-url"])
	}
}

func TestComposerServiceIndexWithoutUpstreamSupport(t *testing.T) {
	tests := []struct {
		name  string
		index string
	}{
		{"not advertised", `{"packages": [], "metadata-url": "/p2/%package%.json"}`},
		{"advisories without api-url", `{"security-advisories": {"metadata": true}}`},
		{"filter without metadata", `{"filter": {"lists": {"malware": {"enabled": true}}, "api-url": "/filter"}}`},
		{"not json", `<html>`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := newComposerAdvisoryUpstream(t)
			upstream.index = tt.index
			index := serveComposerIndex(t, newAdvisoryComposerHandler(t, upstream))
			for _, key := range []string{"security-advisories", "filter"} {
				if _, ok := index[key]; ok {
					t.Errorf("%s advertised although upstream does not offer it: %v", key, index[key])
				}
			}
		})
	}
}

func TestComposerServiceIndexUpstreamDown(t *testing.T) {
	upstream := newComposerAdvisoryUpstream(t)
	h := newAdvisoryComposerHandler(t, upstream)
	upstream.Close()

	index := serveComposerIndex(t, h)
	if _, ok := index["security-advisories"]; ok {
		t.Errorf("security-advisories advertised without an upstream index: %v", index["security-advisories"])
	}
	if index["metadata-url"] == nil {
		t.Error("packages.json lacks metadata-url while the upstream is down")
	}
}

func advertisesAdvisories(index map[string]any) bool {
	_, ok := index["security-advisories"]
	return ok
}

func TestComposerServiceIndexUpstreamDownNotRetried(t *testing.T) {
	upstream := newComposerAdvisoryUpstream(t)
	upstream.down.Store(true)
	h := newAdvisoryComposerHandler(t, upstream)

	serveComposerIndex(t, h)
	fetched := len(upstream.requestsTo("/packages.json"))
	if fetched == 0 {
		t.Fatal("first packages.json made no upstream request")
	}
	index := serveComposerIndex(t, h)
	if got := len(upstream.requestsTo("/packages.json")); got != fetched {
		t.Errorf("second packages.json made %d upstream requests, want none", got-fetched)
	}
	if advertisesAdvisories(index) {
		t.Errorf("security-advisories advertised without an upstream index: %v", index["security-advisories"])
	}
}

func TestComposerServiceIndexKeptInMemory(t *testing.T) {
	upstream := newComposerAdvisoryUpstream(t)
	h := newAdvisoryComposerHandler(t, upstream)
	h.proxy.MetadataTTL = 0
	now := time.Now()
	h.indexCache.now = func() time.Time { return now }

	if !advertisesAdvisories(serveComposerIndex(t, h)) {
		t.Fatal("security-advisories not advertised")
	}
	if got := len(upstream.requestsTo("/packages.json")); got != 1 {
		t.Fatalf("upstream packages.json requests = %d, want 1", got)
	}

	// Within the interval the copy in memory is served without asking.
	upstream.down.Store(true)
	now = now.Add(composerIndexFallbackTTL - time.Minute)
	if !advertisesAdvisories(serveComposerIndex(t, h)) {
		t.Error("security-advisories dropped within the interval")
	}
	if got := len(upstream.requestsTo("/packages.json")); got != 1 {
		t.Errorf("upstream packages.json requests = %d within the interval, want 1", got)
	}

	// After it the index is fetched again; the failure keeps the last good
	// copy and is not retried until the next interval.
	now = now.Add(2 * time.Minute)
	if !advertisesAdvisories(serveComposerIndex(t, h)) {
		t.Error("last good copy dropped after a failed fetch")
	}
	fetched := len(upstream.requestsTo("/packages.json"))
	if fetched < 2 {
		t.Fatalf("upstream packages.json requests = %d after the interval, want a new fetch", fetched)
	}
	serveComposerIndex(t, h)
	if got := len(upstream.requestsTo("/packages.json")); got != fetched {
		t.Errorf("failed fetch retried %d times within the interval", got-fetched)
	}
}

// assertComposerSecurityAdvertised checks that h advertises the upstream's
// advisories API and malware filter, and relays both.
func assertComposerSecurityAdvertised(t *testing.T, h *ComposerHandler, when string) {
	t.Helper()
	index := serveComposerIndex(t, h)
	if !advertisesAdvisories(index) {
		t.Errorf("%s: security-advisories not advertised: %v", when, index)
	}
	if _, ok := index["filter"]; !ok {
		t.Errorf("%s: filter not advertised: %v", when, index)
	}
	w := serveAdvisories(h, http.MethodPost, "/api/security-advisories/", "packages%5B%5D=public%2Flibrary")
	if w.Code != http.StatusOK || w.Body.String() != composerAdvisoriesResponse {
		t.Errorf("%s: advisories = %d %q, want upstream's advisories", when, w.Code, w.Body)
	}
	if w := serveComposer(h, "/lists/summary.json"); w.Code != http.StatusOK || w.Body.String() != composerFilterSummary {
		t.Errorf("%s: filter summary = %d %q, want upstream's summary", when, w.Code, w.Body)
	}
}

// A packages.json that doesn't parse, sent with status 200, must not replace
// the last good index, with metadata caching on or off: the advisories API
// and the malware filter stay advertised and relayed.
func TestComposerServiceIndexMalformedRefreshKeepsLastGood(t *testing.T) {
	for _, cacheMetadata := range []bool{false, true} {
		t.Run(fmt.Sprintf("cache_metadata=%v", cacheMetadata), func(t *testing.T) {
			upstream := newComposerAdvisoryUpstream(t)
			h := newAdvisoryComposerHandler(t, upstream)
			h.proxy.CacheMetadata = cacheMetadata
			h.proxy.MetadataTTL = time.Hour
			now := time.Now()
			h.indexCache.now = func() time.Time { return now }
			cacheKey := hashedCacheKey(upstream.URL, "/packages.json")

			assertComposerSecurityAdvertised(t, h, "before the refresh")
			var stored *database.MetadataCacheEntry
			if cacheMetadata {
				var err error
				if stored, err = h.proxy.DB.GetMetadataCache("composer", cacheKey); err != nil || stored == nil {
					t.Fatalf("packages.json not cached: %v, %v", stored, err)
				}
				expireMetadata(t, h.proxy.DB, "composer", cacheKey)
			}

			upstream.malformed.Store(true)
			now = now.Add(2 * time.Hour)
			fetched := len(upstream.requestsTo("/packages.json"))
			assertComposerSecurityAdvertised(t, h, "after a malformed refresh")
			if got := len(upstream.requestsTo("/packages.json")); got == fetched {
				t.Fatal("packages.json was not fetched again after the TTL")
			}

			if !cacheMetadata {
				return
			}
			after, err := h.proxy.DB.GetMetadataCache("composer", cacheKey)
			if err != nil || after == nil || after.ContentDigest != stored.ContentDigest {
				t.Errorf("cached packages.json replaced by the malformed refresh: %v, %v", after, err)
			}
			// A fresh handler, as after a restart, has nothing in memory and
			// reads the stored copy.
			restarted := NewComposerHandlerWithUpstreams(h.proxy, "http://proxy.test", upstream.URL, upstream.URL)
			assertComposerSecurityAdvertised(t, restarted, "after a restart")
		})
	}
}

func TestComposerServiceIndexFetchedOnce(t *testing.T) {
	upstream := newComposerAdvisoryUpstream(t)
	h := newAdvisoryComposerHandler(t, upstream)

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() { serveComposer(h, "/packages.json") })
	}
	wg.Wait()
	if got := len(upstream.requestsTo("/packages.json")); got != 1 {
		t.Errorf("upstream packages.json requests = %d for parallel requests, want 1", got)
	}
}

func serveAdvisories(h *ComposerHandler, method, target, body string) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)
	return w
}

func TestComposerAdvisoriesWithholdPrivateNames(t *testing.T) {
	// The form Composer sends: PHP's http_build_query numbers the entries.
	form := url.Values{
		"packages[0]": {"public/library"},
		"packages[1]": {"example/private"},
		"packages[2]": {"Acme/hosted"},
		"packages[3]": {"other/library"},
	}
	tests := []struct {
		name, method, target, body string
	}{
		{"POST form", http.MethodPost, "/api/security-advisories/", form.Encode()},
		{"GET query", http.MethodGet, "/api/security-advisories/?" + form.Encode(), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := newComposerAdvisoryUpstream(t)
			h := newAdvisoryComposerHandler(t, upstream)

			w := serveAdvisories(h, tt.method, tt.target, tt.body)

			if w.Code != http.StatusOK || w.Body.String() != composerAdvisoriesResponse {
				t.Fatalf("got %d %q, want upstream's advisories", w.Code, w.Body)
			}
			sent := upstream.requestsTo("/api/security-advisories/")
			if len(sent) != 1 {
				t.Fatalf("upstream got %d advisories requests, want 1", len(sent))
			}
			if sent[0].method != tt.method {
				t.Errorf("upstream method = %s, want %s", sent[0].method, tt.method)
			}
			query := sent[0].query
			if tt.method == http.MethodPost {
				query, _ = url.ParseQuery(sent[0].body)
			}
			got := query["packages[]"]
			slices.Sort(got)
			if want := []string{"other/library", "public/library"}; !slices.Equal(got, want) {
				t.Errorf("upstream was asked about %v, want only the public packages %v", got, want)
			}
		})
	}
}

func TestComposerAdvisoriesOnlyPrivateNames(t *testing.T) {
	upstream := newComposerAdvisoryUpstream(t)
	h := newAdvisoryComposerHandler(t, upstream)

	w := serveAdvisories(h, http.MethodPost, "/api/security-advisories/", "packages%5B0%5D=example%2Fprivate")

	if w.Code != http.StatusOK || w.Body.String() != `{"advisories":{}}` {
		t.Errorf("got %d %q, want an empty advisories answer", w.Code, w.Body)
	}
	if sent := upstream.requestsTo("/api/security-advisories/"); len(sent) != 0 {
		t.Errorf("upstream was asked although every package is private: %+v", sent[0])
	}
}

func TestComposerAdvisoriesUpdatedSince(t *testing.T) {
	upstream := newComposerAdvisoryUpstream(t)
	h := newAdvisoryComposerHandler(t, upstream)

	w := serveAdvisories(h, http.MethodGet, "/api/security-advisories/?updatedSince=1700000000", "")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	sent := upstream.requestsTo("/api/security-advisories/")
	if len(sent) != 1 || sent[0].query.Get("updatedSince") != "1700000000" {
		t.Errorf("updatedSince was not passed on: %+v", sent)
	}
}

func TestComposerAdvisoriesNotOffered(t *testing.T) {
	upstream := newComposerAdvisoryUpstream(t)
	upstream.index = `{"packages": []}`
	h := newAdvisoryComposerHandler(t, upstream)

	if w := serveAdvisories(h, http.MethodPost, "/api/security-advisories/", "packages%5B%5D=public%2Flibrary"); w.Code != http.StatusNotFound {
		t.Errorf("advisories status = %d, want 404 when upstream has no API", w.Code)
	}
	if w := serveComposer(h, "/lists/summary.json"); w.Code != http.StatusNotFound {
		t.Errorf("summary status = %d, want 404 when upstream has no filter", w.Code)
	}
}

func TestComposerFilterSummaryRelayed(t *testing.T) {
	upstream := newComposerAdvisoryUpstream(t)
	h := newAdvisoryComposerHandler(t, upstream)

	w := serveComposer(h, "/lists/summary.json")
	if w.Code != http.StatusOK || w.Body.String() != composerFilterSummary {
		t.Fatalf("got %d %q, want upstream's summary", w.Code, w.Body)
	}

	req := httptest.NewRequest(http.MethodGet, "/lists/summary.json", nil)
	req.Header.Set("If-Modified-Since", "Wed, 21 Oct 2026 15:45:01 GMT")
	w = httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusNotModified {
		t.Errorf("revalidation status = %d, want 304 passed on from upstream", w.Code)
	}
}

func TestComposerMetadataKeepsAdvisoriesAndFilter(t *testing.T) {
	h := NewComposerHandler(testProxy(), "http://proxy.test")
	body := []byte(`{
		"minified": "composer/2.0",
		"packages": {"evil/package": [{"name": "evil/package", "version": "1.0.0", "dist": {"url": "https://example.com/evil.zip", "type": "zip"}}]},
		"security-advisories": [{"advisoryId": "PKSA-1", "affectedVersions": "<2.0.0"}],
		"filter": {"malware": [{"constraint": "1.0.0", "reason": "malware", "id": "PKFE-1"}]}
	}`)

	out, _, err := h.rewriteMetadataKeeping(body)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("parsing rewritten metadata: %v", err)
	}
	if !strings.Contains(string(doc["security-advisories"]), "PKSA-1") {
		t.Errorf("security-advisories lost in rewrite: %s", out)
	}
	if !strings.Contains(string(doc["filter"]), "PKFE-1") {
		t.Errorf("filter lost in rewrite: %s", out)
	}
}
