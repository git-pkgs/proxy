package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	registryclient "github.com/git-pkgs/registries/client"
	"github.com/git-pkgs/registries/fetch"
)

// chefCooldownSupermarket serves nginx, updated an hour ago, with an old
// 1.0.0 and a recent 1.1.0; apt, updated ten days ago; and old, updated
// longer ago, which a universe filter stops before.
type chefCooldownSupermarket struct {
	server *httptest.Server
	now    time.Time
	mu     sync.Mutex
	hits   map[string]int
	fail   map[string]bool
	// limited are paths answered with 429.
	limited map[string]bool
	// recent is the recently updated listing, newest first.
	recent []string
	total  int
}

func (s *chefCooldownSupermarket) count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[path]
}

func (s *chefCooldownSupermarket) setFail(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail[path] = true
}

var chefCooldownVersions = map[string]map[string]time.Duration{
	"nginx": {"1.0.0": 30 * 24 * time.Hour, "1.1.0": time.Hour},
	"apt":   {"2.0.0": 10 * 24 * time.Hour},
	"old":   {"0.1.0": 400 * 24 * time.Hour},
}

var chefCooldownUpdated = map[string]time.Duration{
	"nginx": time.Hour, "apt": 10 * 24 * time.Hour, "old": 400 * 24 * time.Hour,
}

func newChefCooldownSupermarket(t *testing.T) *chefCooldownSupermarket {
	t.Helper()
	s := &chefCooldownSupermarket{
		now: time.Now().UTC().Truncate(time.Second), hits: map[string]int{}, fail: map[string]bool{}, limited: map[string]bool{},
		recent: []string{"nginx", "apt", "old"},
	}
	s.total = len(s.recent)
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.server.Close)
	return s
}

func (s *chefCooldownSupermarket) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.hits[r.URL.Path]++
	fail, limited := s.fail[r.URL.Path], s.limited[r.URL.Path]
	s.mu.Unlock()
	if limited {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "slow down", http.StatusTooManyRequests)
		return
	}
	if fail {
		http.Error(w, "down", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentTypeJSON)
	api := s.server.URL + "/api/v1"
	if r.URL.Path == "/universe" {
		universe := map[string]map[string]any{}
		for name, versions := range chefCooldownVersions {
			universe[name] = map[string]any{}
			for v := range versions {
				universe[name][v] = map[string]any{
					"location_type": "opscode", "location_path": api,
					"download_url": api + "/cookbooks/" + name + "/versions/" + v + "/download", "dependencies": map[string]any{},
				}
			}
		}
		_ = json.NewEncoder(w).Encode(universe)
		return
	}
	if r.URL.Path == "/api/v1/cookbooks" {
		if r.URL.Query().Get("order") != "recently_updated" {
			http.NotFound(w, r)
			return
		}
		var start int
		_, _ = fmt.Sscan(r.URL.Query().Get("start"), &start)
		items := []map[string]string{}
		for i := start; i < len(s.recent) && i < start+chefRecentPageSize; i++ {
			items = append(items, map[string]string{"cookbook_name": s.recent[i]})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"start": start, "total": s.total, "items": items})
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/v1/cookbooks/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	name, rest, _ := strings.Cut(rest, "/versions/")
	versions, ok := chefCooldownVersions[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if rest == "" {
		var listed []string
		for v := range versions {
			listed = append(listed, v)
		}
		slices.Sort(listed)
		slices.Reverse(listed)
		urls := make([]string, len(listed))
		for i, v := range listed {
			urls[i] = api + "/cookbooks/" + name + "/versions/" + v
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": name, "latest_version": urls[0], "versions": urls,
			"updated_at": s.now.Add(-chefCooldownUpdated[name]).Format(time.RFC3339),
		})
		return
	}
	age, ok := versions[rest]
	if !ok {
		http.NotFound(w, r)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"version": rest, "published_at": s.now.Add(-age).Format(time.RFC3339),
		"cookbook": api + "/cookbooks/" + name, "file": api + "/cookbooks/" + name + "/versions/" + rest + "/download",
	})
}

func chefCooldownHandler(t *testing.T, s *chefCooldownSupermarket) (http.Handler, *Proxy, *mockFetcher) {
	t.Helper()
	proxy, _, _, fetcher := setupTestProxy(t)
	proxy.Cooldown = &cooldown.Config{Default: "3d"}
	proxy.LongestCooldown = 72 * time.Hour
	fetcher.artifact = &fetch.Artifact{
		Body: io.NopCloser(strings.NewReader(chefTestTarball)), Size: int64(len(chefTestTarball)), ContentType: "application/gzip",
	}
	return NewChefHandler(proxy, chefTestProxyURL, s.server.URL).Routes(), proxy, fetcher
}

func chefGet(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

func chefUniverseVersions(t *testing.T, w *httptest.ResponseRecorder) map[string][]string {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("universe status = %d: %s", w.Code, w.Body.String())
	}
	var universe map[string]map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &universe); err != nil {
		t.Fatal(err)
	}
	listed := map[string][]string{}
	for name, versions := range universe {
		for v := range versions {
			listed[name] = append(listed[name], v)
		}
		slices.Sort(listed[name])
	}
	return listed
}

func TestChefCooldownFiltersUniverse(t *testing.T) {
	s := newChefCooldownSupermarket(t)
	h, proxy, _ := chefCooldownHandler(t, s)

	listed := chefUniverseVersions(t, chefGet(t, h, "/universe"))
	if got := listed["nginx"]; !slices.Equal(got, []string{"1.0.0"}) {
		t.Errorf("nginx = %v, want [1.0.0]", got)
	}
	if got := listed["apt"]; !slices.Equal(got, []string{"2.0.0"}) {
		t.Errorf("apt = %v, want [2.0.0]", got)
	}
	if got := listed["old"]; !slices.Equal(got, []string{"0.1.0"}) {
		t.Errorf("old = %v, want [0.1.0]", got)
	}
	if got := s.count("/api/v1/cookbooks/old"); got != 0 {
		t.Errorf("old's cookbook read %d times: the walk stops at apt, updated before the cooldown", got)
	}
	if got := s.count("/api/v1/cookbooks/apt/versions/2.0.0"); got != 0 {
		t.Errorf("apt 2.0.0 read %d times: apt was updated before the cooldown", got)
	}
	assertWithheldRecorded(t, proxy.DB, "chef", "nginx", "1.1.0", s.now.Add(-time.Hour))
}

func TestChefCooldownReadsTheListingAgainForEachFilter(t *testing.T) {
	s := newChefCooldownSupermarket(t)
	h, proxy, _ := chefCooldownHandler(t, s)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour

	// A cookbook document cached before nginx 1.1.0 was published would say
	// nginx hasn't changed; the filter reads it from the upstream.
	chefGet(t, h, "/api/v1/cookbooks/nginx")
	before := s.count("/api/v1/cookbooks/nginx")
	chefUniverseVersions(t, chefGet(t, h, "/universe"))
	if got := s.count("/api/v1/cookbooks/nginx"); got != before+1 {
		t.Errorf("nginx's cookbook read %d times for the universe, want 1", got-before)
	}
}

func TestChefCooldownFailsUniverseClosed(t *testing.T) {
	for _, path := range []string{"/api/v1/cookbooks", "/api/v1/cookbooks/nginx", "/api/v1/cookbooks/nginx/versions/1.1.0"} {
		s := newChefCooldownSupermarket(t)
		h, _, _ := chefCooldownHandler(t, s)
		s.setFail(path)
		if w := chefGet(t, h, "/universe"); w.Code != http.StatusBadGateway {
			t.Errorf("%s failing: universe status = %d, want 502", path, w.Code)
		}
	}
}

func TestChefCooldownStopsAtTooManyRecentCookbooks(t *testing.T) {
	s := newChefCooldownSupermarket(t)
	h, _, _ := chefCooldownHandler(t, s)
	s.recent = nil
	for range maxChefRecentPages * chefRecentPageSize {
		s.recent = append(s.recent, "nginx")
	}
	s.total = len(s.recent) + 1
	if w := chefGet(t, h, "/universe"); w.Code != http.StatusBadGateway {
		t.Errorf("universe status = %d, want 502", w.Code)
	}
}

func TestChefCooldownFollowsListingPages(t *testing.T) {
	s := newChefCooldownSupermarket(t)
	h, _, _ := chefCooldownHandler(t, s)
	// The first page is all recent cookbooks, so the walk reads the second,
	// which starts with apt, updated before the cooldown.
	s.recent = []string{"nginx"}
	for i := 1; i < chefRecentPageSize; i++ {
		s.recent = append(s.recent, fmt.Sprintf("recent%d", i))
	}
	s.recent = append(s.recent, "apt", "old")
	s.total = len(s.recent)

	listed := chefUniverseVersions(t, chefGet(t, h, "/universe"))
	if got := listed["nginx"]; !slices.Equal(got, []string{"1.0.0"}) {
		t.Errorf("nginx = %v, want [1.0.0]", got)
	}
	if got := s.count("/api/v1/cookbooks/apt"); got != 1 {
		t.Errorf("apt read %d times, want once, on the second page", got)
	}
	if got := s.count("/api/v1/cookbooks/old"); got != 0 {
		t.Errorf("old read %d times, want none", got)
	}
}

func TestChefCooldownFiltersCookbook(t *testing.T) {
	s := newChefCooldownSupermarket(t)
	h, _, _ := chefCooldownHandler(t, s)

	w := chefGet(t, h, "/api/v1/cookbooks/nginx")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var doc struct {
		Latest   string   `json:"latest_version"`
		Versions []string `json:"versions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	want := chefTestAPI + "/cookbooks/nginx/versions/1.0.0"
	if doc.Latest != want || !slices.Equal(doc.Versions, []string{want}) {
		t.Errorf("latest = %q, versions = %v; want only %s", doc.Latest, doc.Versions, want)
	}

	// apt was updated before the cooldown, so no version of it is read.
	if w := chefGet(t, h, "/api/v1/cookbooks/apt"); w.Code != http.StatusOK {
		t.Fatalf("apt status = %d", w.Code)
	}
	if got := s.count("/api/v1/cookbooks/apt/versions/2.0.0"); got != 0 {
		t.Errorf("apt 2.0.0 read %d times", got)
	}
}

func TestChefCooldownRefusesVersionAndDownload(t *testing.T) {
	s := newChefCooldownSupermarket(t)
	h, _, fetcher := chefCooldownHandler(t, s)

	for _, path := range []string{
		"/api/v1/cookbooks/nginx/versions/1.1.0",
		"/api/v1/cookbooks/nginx/versions/1_1_0",
		"/api/v1/cookbooks/nginx/versions/1.1.0/download",
	} {
		w := chefGet(t, h, path)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "cooldown: pkg:chef/nginx@1.1.0") {
			t.Errorf("%s: %d %s, want 403 naming the version", path, w.Code, w.Body.String())
		}
	}
	if fetcher.fetchCalled {
		t.Error("archive of a version in cooldown was fetched")
	}
	for _, path := range []string{
		"/api/v1/cookbooks/nginx/versions/1.0.0",
		"/api/v1/cookbooks/nginx/versions/1.0.0/download",
	} {
		if w := chefGet(t, h, path); w.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200; %s", path, w.Code, w.Body.String())
		}
	}
}

func TestChefCooldownServesCachedDownloadWhenTimeCantBeRead(t *testing.T) {
	s := newChefCooldownSupermarket(t)
	h, proxy, fetcher := chefCooldownHandler(t, s)
	proxy.Cooldown = &cooldown.Config{Default: "0"}
	if w := chefGet(t, h, "/api/v1/cookbooks/nginx/versions/1.1.0/download"); w.Code != http.StatusOK {
		t.Fatalf("download without cooldown = %d", w.Code)
	}
	proxy.Cooldown = &cooldown.Config{Default: "3d"}
	s.setFail("/api/v1/cookbooks/nginx/versions/1.1.0")
	fetcher.fetchCalled = false

	w := chefGet(t, h, "/api/v1/cookbooks/nginx/versions/1.1.0/download")
	if w.Code != http.StatusOK || w.Body.String() != chefTestTarball {
		t.Errorf("cached download = %d %q", w.Code, w.Body.String())
	}
	if w := chefGet(t, h, "/api/v1/cookbooks/nginx/versions/1.1.0"); w.Code != http.StatusBadGateway {
		t.Errorf("version document = %d, want 502", w.Code)
	}
}

func TestChefCooldownExemptCookbook(t *testing.T) {
	s := newChefCooldownSupermarket(t)
	h, proxy, _ := chefCooldownHandler(t, s)
	proxy.Cooldown = &cooldown.Config{Default: "3d", Packages: map[string]string{"pkg:chef/nginx": "0"}}

	if got := chefUniverseVersions(t, chefGet(t, h, "/universe"))["nginx"]; !slices.Equal(got, []string{"1.0.0", "1.1.0"}) {
		t.Errorf("nginx = %v, want both versions", got)
	}
	if got := s.count("/api/v1/cookbooks/nginx/versions/1.1.0"); got != 0 {
		t.Errorf("1.1.0 read %d times for an exempt cookbook", got)
	}
	if w := chefGet(t, h, "/api/v1/cookbooks/nginx/versions/1.1.0"); w.Code != http.StatusOK {
		t.Errorf("version status = %d, want 200", w.Code)
	}
}

func TestChefWithoutCooldownReadsNoListing(t *testing.T) {
	s := newChefCooldownSupermarket(t)
	h, proxy, _ := chefCooldownHandler(t, s)
	proxy.Cooldown = nil
	if got := chefUniverseVersions(t, chefGet(t, h, "/universe"))["nginx"]; len(got) != 2 {
		t.Errorf("nginx = %v, want both versions", got)
	}
	if got := s.count("/api/v1/cookbooks"); got != 0 {
		t.Errorf("listing read %d times without cooldown", got)
	}
}

func TestChefCooldownStoresTimesAndRecordsRefusals(t *testing.T) {
	s := newChefCooldownSupermarket(t)
	h, proxy, _ := chefCooldownHandler(t, s)

	for range 2 {
		if w := chefGet(t, h, "/api/v1/cookbooks/nginx/versions/1.1.0"); w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", w.Code)
		}
	}
	if got := s.count("/api/v1/cookbooks/nginx/versions/1.1.0"); got != 1 {
		t.Errorf("1.1.0 read %d times, want once: its time is stored", got)
	}
	assertWithheldRecorded(t, proxy.DB, "chef", "nginx", "1.1.0", s.now.Add(-time.Hour))

	// A version out of cooldown has its time stored too.
	if w := chefGet(t, h, "/api/v1/cookbooks/nginx/versions/1.0.0"); w.Code != http.StatusOK {
		t.Fatalf("1.0.0 status = %d", w.Code)
	}
	ver, err := proxy.DB.GetVersionByPURL("pkg:chef/nginx@1.0.0")
	if err != nil || ver == nil || !ver.PublishedAt.Valid || !ver.PublishedAt.Time.Equal(s.now.Add(-30*24*time.Hour)) {
		t.Errorf("1.0.0 row = %+v, %v; want its publish time", ver, err)
	}
}

func TestChefFreshDocumentRefusesAStaleCopy(t *testing.T) {
	s := newChefCooldownSupermarket(t)
	_, proxy, _ := chefCooldownHandler(t, s)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	h := NewChefHandler(proxy, chefTestProxyURL, s.server.URL)
	ctx := httptest.NewRequest(http.MethodGet, "/", nil).Context()

	if _, err := h.freshDocument(ctx, "cookbooks/nginx", h.upstreamCookbookURL("nginx")); err != nil {
		t.Fatal(err)
	}
	s.setFail("/api/v1/cookbooks/nginx")
	if _, err := h.freshDocument(ctx, "cookbooks/nginx", h.upstreamCookbookURL("nginx")); err == nil {
		t.Error("a stale copy was returned as the upstream's answer")
	}
}

func TestChefCooldownRateLimitedUniverseIsHeld(t *testing.T) {
	s := newChefCooldownSupermarket(t)
	h, proxy, _ := chefCooldownHandler(t, s)
	s.limited["/api/v1/cookbooks/nginx/versions/1.1.0"] = true
	gate := registryclient.NewRateLimitGate(s.server.Client().Transport)
	proxy.HTTPClient = &http.Client{Transport: gate}
	proxy.RateLimits = gate
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour

	// The 429 itself fails the filter; while the gate holds the upstream,
	// the universe, served from the metadata cache, is answered with 503.
	if w := chefGet(t, h, "/universe"); w.Code != http.StatusBadGateway {
		t.Fatalf("first status = %d, want 502", w.Code)
	}
	w := chefGet(t, h, "/universe")
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Errorf("status = %d, Retry-After %q; want 503 with Retry-After", w.Code, w.Header().Get("Retry-After"))
	}
}
