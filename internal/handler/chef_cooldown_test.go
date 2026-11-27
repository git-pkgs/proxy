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
	"github.com/git-pkgs/proxy/internal/config"
	registryclient "github.com/git-pkgs/registries/client"
	"github.com/git-pkgs/registries/fetch"
)

// chefCooldownSupermarket serves nginx, updated an hour ago, with an old
// 1.0.0 and a recent 1.1.0; apt, updated ten days ago; old, updated longer
// ago, which a universe filter stops before; and legacy, a deprecated
// cookbook updated an hour ago with an old 3.0.0 and a recent 3.1.0, which
// Supermarket lists after every cookbook that isn't deprecated.
type chefCooldownSupermarket struct {
	server    *httptest.Server
	now       time.Time
	mu        sync.Mutex
	hits      map[string]int
	fail      map[string]bool
	cookbooks map[string]chefFakeCookbook
	// limited are paths answered with 429.
	limited map[string]bool
	// recent is the recently updated listing; nil lists cookbooks as
	// Supermarket orders them.
	recent []string
	total  int
}

type chefFakeCookbook struct {
	updated    time.Duration
	deprecated bool
	// versions are the cookbook's versions and their ages.
	versions map[string]time.Duration
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

func newChefCooldownSupermarket(t *testing.T) *chefCooldownSupermarket {
	t.Helper()
	day := 24 * time.Hour
	s := &chefCooldownSupermarket{
		now: time.Now().UTC().Truncate(time.Second), hits: map[string]int{}, fail: map[string]bool{}, limited: map[string]bool{},
		cookbooks: map[string]chefFakeCookbook{
			"nginx":  {updated: time.Hour, versions: map[string]time.Duration{"1.0.0": 30 * day, "1.1.0": time.Hour}},
			"apt":    {updated: 10 * day, versions: map[string]time.Duration{"2.0.0": 10 * day}},
			"old":    {updated: 400 * day, versions: map[string]time.Duration{"0.1.0": 400 * day}},
			"legacy": {updated: time.Hour, deprecated: true, versions: map[string]time.Duration{"3.0.0": 30 * day, "3.1.0": time.Hour}},
		},
	}
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.server.Close)
	return s
}

// listing returns the recently updated listing: cookbooks that aren't
// deprecated, newest first, then deprecated ones, newest first.
func (s *chefCooldownSupermarket) listing() ([]string, int) {
	if s.recent != nil {
		return s.recent, s.total
	}
	names := make([]string, 0, len(s.cookbooks))
	for name := range s.cookbooks {
		names = append(names, name)
	}
	slices.SortFunc(names, func(a, b string) int {
		ca, cb := s.cookbooks[a], s.cookbooks[b]
		if ca.deprecated != cb.deprecated {
			if ca.deprecated {
				return 1
			}
			return -1
		}
		if ca.updated != cb.updated {
			return int(ca.updated - cb.updated)
		}
		return strings.Compare(a, b)
	})
	return names, len(names)
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
		for name, c := range s.cookbooks {
			universe[name] = map[string]any{}
			for v := range c.versions {
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
		var start, items int
		_, _ = fmt.Sscan(r.URL.Query().Get("start"), &start)
		_, _ = fmt.Sscan(r.URL.Query().Get("items"), &items)
		recent, total := s.listing()
		page := []map[string]string{}
		for i := start; i < len(recent) && i < start+min(items, chefRecentPageSize); i++ {
			page = append(page, map[string]string{"cookbook_name": recent[i]})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"start": start, "total": total, "items": page})
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/v1/cookbooks/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	name, rest, _ := strings.Cut(rest, "/versions/")
	c, ok := s.cookbooks[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if rest == "" {
		var listed []string
		for v := range c.versions {
			listed = append(listed, v)
		}
		slices.Sort(listed)
		slices.Reverse(listed)
		urls := make([]string, len(listed))
		for i, v := range listed {
			urls[i] = api + "/cookbooks/" + name + "/versions/" + v
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": name, "latest_version": urls[0], "versions": urls, "deprecated": c.deprecated,
			"updated_at": s.now.Add(-c.updated).Format(time.RFC3339),
		})
		return
	}
	age, ok := c.versions[rest]
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
	proxy.LongestCooldown = func(string) time.Duration { return 72 * time.Hour }
	fetcher.artifact = &fetch.Artifact{
		Body: io.NopCloser(strings.NewReader(chefTestTarball)), Size: int64(len(chefTestTarball)), ContentType: "application/gzip",
	}
	h := NewChefHandler(proxy, chefTestProxyURL, s.server.URL)
	// Stand in for the public Supermarket, whose times are stored without
	// a repository_url.
	h.timeQualifier = ""
	return h.Routes(), proxy, fetcher
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
	if got := listed["legacy"]; !slices.Equal(got, []string{"3.0.0"}) {
		t.Errorf("legacy = %v, want [3.0.0]: deprecated cookbooks are listed after the others, and checked too", got)
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
	if got := s.count("/api/v1/cookbooks/old/versions/0.1.0"); got != 0 {
		t.Errorf("old 0.1.0 read %d times, want none", got)
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
	proxy.Cooldown, proxy.LongestCooldown = nil, nil
	if got := chefUniverseVersions(t, chefGet(t, h, "/universe"))["nginx"]; len(got) != 2 {
		t.Errorf("nginx = %v, want both versions", got)
	}
	if got := s.count("/api/v1/cookbooks"); got != 0 {
		t.Errorf("listing read %d times without cooldown", got)
	}
}

// TestChefCooldownForOtherEcosystemsReadsNoListing checks that a cooldown
// that can't apply to Chef leaves the universe alone: nothing is read from
// the upstream to filter it, so an upstream without the listing still works.
func TestChefCooldownForOtherEcosystemsReadsNoListing(t *testing.T) {
	s := newChefCooldownSupermarket(t)
	h, proxy, _ := chefCooldownHandler(t, s)
	cfg := &config.CooldownConfig{Ecosystems: map[string]string{"npm": "3d"}, Packages: map[string]string{"pkg:npm/foo": "90d"}}
	policy, err := cfg.Policy()
	if err != nil {
		t.Fatal(err)
	}
	proxy.Cooldown, proxy.LongestCooldown = policy, cfg.Longest
	if got := chefUniverseVersions(t, chefGet(t, h, "/universe"))["nginx"]; len(got) != 2 {
		t.Errorf("nginx = %v, want both versions", got)
	}
	if got := s.count("/api/v1/cookbooks"); got != 0 {
		t.Errorf("listing read %d times for an npm cooldown", got)
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

func TestChefCooldownFindsDeprecatedCookbooksPastOldOnes(t *testing.T) {
	s := newChefCooldownSupermarket(t)
	h, _, _ := chefCooldownHandler(t, s)
	for i := range 3 * chefRecentPageSize {
		s.cookbooks[fmt.Sprintf("old%03d", i)] = chefFakeCookbook{
			updated: time.Duration(500+i) * 24 * time.Hour, versions: map[string]time.Duration{"1.0.0": 900 * 24 * time.Hour},
		}
	}

	listed := chefUniverseVersions(t, chefGet(t, h, "/universe"))
	if got := listed["legacy"]; !slices.Equal(got, []string{"3.0.0"}) {
		t.Errorf("legacy = %v, want [3.0.0]", got)
	}
	if got := listed["nginx"]; !slices.Equal(got, []string{"1.0.0"}) {
		t.Errorf("nginx = %v, want [1.0.0]", got)
	}
	read := 0
	for name := range s.cookbooks {
		if strings.HasPrefix(name, "old") {
			read += s.count("/api/v1/cookbooks/" + name)
		}
	}
	if read > 12 {
		t.Errorf("read %d old cookbooks to find the deprecated ones, want a binary search", read)
	}
}

// TestChefCooldownFailsWhenAProbedCookbookIsGone checks that the search for
// the deprecated section doesn't guess the section of a cookbook deleted
// after the listing named it: skipping past it could miss a deprecated
// cookbook with a version in cooldown.
func TestChefCooldownFailsWhenAProbedCookbookIsGone(t *testing.T) {
	s := newChefCooldownSupermarket(t)
	h, _, _ := chefCooldownHandler(t, s)
	s.cookbooks["olddeprecated"] = chefFakeCookbook{
		updated: 400 * 24 * time.Hour, deprecated: true, versions: map[string]time.Duration{"1.0.0": 400 * 24 * time.Hour},
	}
	// legacy opens the deprecated section; the search probes ghost first.
	s.recent, s.total = []string{"old", "legacy", "ghost", "olddeprecated"}, 4

	if w := chefGet(t, h, "/universe"); w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502: legacy 3.1.0 is in cooldown and would be listed", w.Code)
	}
}

func TestChefCooldownStopsInTheDeprecatedSection(t *testing.T) {
	s := newChefCooldownSupermarket(t)
	h, _, _ := chefCooldownHandler(t, s)
	s.cookbooks["retired"] = chefFakeCookbook{updated: 300 * 24 * time.Hour, deprecated: true, versions: map[string]time.Duration{"1.0.0": 300 * 24 * time.Hour}}
	s.cookbooks["gone"] = chefFakeCookbook{updated: 600 * 24 * time.Hour, deprecated: true, versions: map[string]time.Duration{"1.0.0": 600 * 24 * time.Hour}}

	chefUniverseVersions(t, chefGet(t, h, "/universe"))
	if got := s.count("/api/v1/cookbooks/gone"); got != 0 {
		t.Errorf("gone read %d times: the walk stops at retired, the first old deprecated cookbook", got)
	}
}

func TestChefCooldownKeepsTimesPerUpstream(t *testing.T) {
	s := newChefCooldownSupermarket(t)
	_, proxy, _ := chefCooldownHandler(t, s)
	h := NewChefHandler(proxy, chefTestProxyURL, s.server.URL).Routes()
	// A time stored for the default Supermarket's nginx 1.1.0 says nothing
	// about the one on this upstream.
	if err := proxy.DB.SetVersionPublishedAt("pkg:chef/nginx@1.1.0", "pkg:chef/nginx", s.now.Add(-300*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if w := chefGet(t, h, "/api/v1/cookbooks/nginx/versions/1.1.0"); w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
	if got := s.count("/api/v1/cookbooks/nginx/versions/1.1.0"); got != 1 {
		t.Errorf("1.1.0 read %d times, want once", got)
	}
	if w := chefGet(t, h, "/api/v1/cookbooks/nginx/versions/1.1.0"); w.Code != http.StatusForbidden {
		t.Errorf("second status = %d, want 403", w.Code)
	}
	if got := s.count("/api/v1/cookbooks/nginx/versions/1.1.0"); got != 1 {
		t.Errorf("1.1.0 read %d times, want once: its time is stored for this upstream", got)
	}
	// Nor does the package page get this upstream's times.
	ver, err := proxy.DB.GetVersionByPURL("pkg:chef/nginx@1.1.0")
	if err != nil || ver == nil || !ver.PublishedAt.Time.Equal(s.now.Add(-300*24*time.Hour)) {
		t.Errorf("default upstream's row = %+v, %v; want it unchanged", ver, err)
	}
	if pkg, err := proxy.DB.GetPackageByEcosystemName("chef", "nginx"); err != nil || pkg != nil {
		t.Errorf("package row = %+v, %v; want none recorded for another upstream", pkg, err)
	}
}

func TestChefTimeKey(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	for upstream, want := range map[string]string{
		"https://supermarket.chef.io":         "pkg:chef/nginx@1.0.0",
		"https://supermarket.chef.io/":        "pkg:chef/nginx@1.0.0",
		"https://supermarket.example.com/sub": "pkg:chef/nginx@1.0.0?repository_url=https%3A%2F%2Fsupermarket.example.com%2Fsub",
	} {
		versionPURL, packagePURL := NewChefHandler(proxy, chefTestProxyURL, upstream).timeKey("nginx", "1.0.0")
		if versionPURL != want || !strings.HasPrefix(versionPURL, "pkg:chef/nginx@") || !strings.HasPrefix(packagePURL, "pkg:chef/nginx") {
			t.Errorf("%s: timeKey = %q, %q; want %q", upstream, versionPURL, packagePURL, want)
		}
	}
}

func TestChefCooldownStopsInADeprecatedSectionReachedByWalking(t *testing.T) {
	s := newChefCooldownSupermarket(t)
	h, _, _ := chefCooldownHandler(t, s)
	delete(s.cookbooks, "apt")
	delete(s.cookbooks, "old")
	s.cookbooks["retired"] = chefFakeCookbook{updated: 300 * 24 * time.Hour, deprecated: true, versions: map[string]time.Duration{"1.0.0": 300 * 24 * time.Hour}}
	s.cookbooks["gone"] = chefFakeCookbook{updated: 600 * 24 * time.Hour, deprecated: true, versions: map[string]time.Duration{"1.0.0": 600 * 24 * time.Hour}}

	if got := chefUniverseVersions(t, chefGet(t, h, "/universe"))["legacy"]; !slices.Equal(got, []string{"3.0.0"}) {
		t.Errorf("legacy = %v, want [3.0.0]", got)
	}
	if got := s.count("/api/v1/cookbooks/gone"); got != 0 {
		t.Errorf("gone read %d times: the walk stops at retired", got)
	}
}

func TestChefCooldownListingShorterThanItsTotal(t *testing.T) {
	s := newChefCooldownSupermarket(t)
	h, _, _ := chefCooldownHandler(t, s)
	s.recent, s.total = []string{"nginx", "apt", "old"}, 60

	if got := chefUniverseVersions(t, chefGet(t, h, "/universe"))["nginx"]; !slices.Equal(got, []string{"1.0.0"}) {
		t.Errorf("nginx = %v, want [1.0.0]", got)
	}
}
