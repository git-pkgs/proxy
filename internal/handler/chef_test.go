package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	upstreamhttp "github.com/git-pkgs/proxy/internal/httpclient"
	"github.com/git-pkgs/registries/fetch"
)

const (
	chefTestProxyURL = "http://proxy.example"
	chefTestAPI      = chefTestProxyURL + "/chef/api/v1"
	chefTestTarball  = "cookbook tarball bytes"
)

// fakeSupermarket serves the parts of the Supermarket API Berkshelf uses, in
// the shapes supermarket.chef.io returns them. Downloads redirect to a second
// server standing in for the storage host, as Supermarket redirects to S3.
type fakeSupermarket struct {
	api   *httptest.Server
	files *httptest.Server
	// filesURL is files.URL on another hostname, so a redirect to it leaves
	// the API host the way a redirect to S3 does.
	filesURL string

	available atomic.Bool
	// token, when set, is the bearer token the API requires.
	token string
	// tarball is the content of every cookbook archive it serves.
	tarball string

	mu       sync.Mutex
	requests map[string]int
	fileAuth []string
}

func newFakeSupermarket(t *testing.T, token string) *fakeSupermarket {
	t.Helper()
	s := &fakeSupermarket{token: token, tarball: chefTestTarball, requests: map[string]int{}}
	s.available.Store(true)

	s.files = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests["files "+r.URL.Path]++
		s.fileAuth = append(s.fileAuth, r.Header.Get("Authorization"))
		s.mu.Unlock()
		if !s.available.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path != "/cookbook_versions/tarballs/34899/original/apt.tgz" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-gzip")
		_, _ = w.Write([]byte(s.tarball))
	}))
	t.Cleanup(s.files.Close)
	s.filesURL = strings.Replace(s.files.URL, "127.0.0.1", "localhost", 1)

	s.api = httptest.NewServer(http.HandlerFunc(s.serveAPI))
	t.Cleanup(s.api.Close)
	return s
}

func (s *fakeSupermarket) serveAPI(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests[r.URL.Path]++
	s.mu.Unlock()
	if !s.available.Load() {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	if s.token != "" && r.Header.Get("Authorization") != "Bearer "+s.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	api := s.api.URL + "/api/v1"
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/universe":
		// Like Supermarket's, these locations carry an explicit port.
		located := api
		_, _ = fmt.Fprintf(w, `{"apt":{"7.5.22":{"location_type":"opscode","location_path":%q,"download_url":%q,"dependencies":{"compat_resource":">= 0.0.0"}},`+
			`"7.6.0":{"location_type":"opscode","location_path":%q,"download_url":%q,"dependencies":{}}},`+
			`"compat_resource":{"12.19.1":{"location_type":"opscode","location_path":%q,"download_url":%q,"dependencies":{"apt":"~> 7.5"}}}}`,
			located, api+"/cookbooks/apt/versions/7.5.22/download",
			located, api+"/cookbooks/apt/versions/7.6.0/download",
			located, api+"/cookbooks/compat_resource/versions/12.19.1/download")
	case "/api/v1/cookbooks/apt":
		_, _ = fmt.Fprintf(w, `{"name":"apt","maintainer":"sous-chefs","latest_version":%q,"external_url":"https://github.com/sous-chefs/apt","deprecated":false,"versions":[%q,%q]}`,
			api+"/cookbooks/apt/versions/7.6.0", api+"/cookbooks/apt/versions/7.6.0", api+"/cookbooks/apt/versions/7.5.22")
	case "/api/v1/cookbooks/apt/versions/7.5.22":
		_, _ = fmt.Fprintf(w, `{"license":"Apache-2.0","tarball_file_size":%d,"version":"7.5.22","cookbook":%q,"file":%q,"dependencies":{"compat_resource":">= 0.0.0"}}`,
			len(s.tarball), api+"/cookbooks/apt", api+"/cookbooks/apt/versions/7.5.22/download")
	case "/api/v1/cookbooks/apt/versions/7.5.22/download":
		http.Redirect(w, r, s.filesURL+"/cookbook_versions/tarballs/34899/original/apt.tgz?1714639378", http.StatusFound)
	default:
		http.NotFound(w, r)
	}
}

func (s *fakeSupermarket) count(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[key]
}

// newChefTestHandler returns a handler for s using client for every upstream
// request, with metadata caching on.
func newChefTestHandler(t *testing.T, s *fakeSupermarket, client *http.Client) (*ChefHandler, *Proxy) {
	t.Helper()
	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.HTTPClient = client
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(client), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	return NewChefHandler(proxy, chefTestProxyURL, s.api.URL), proxy
}

func serveChefRequest(h *ChefHandler, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	return w
}

// proxyPath turns an absolute proxy URL from a rewritten document into the
// path the handler is mounted at.
func proxyPath(t *testing.T, rawURL string) string {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	if parsed.Scheme+"://"+parsed.Host != chefTestProxyURL || !strings.HasPrefix(parsed.Path, "/chef/") {
		t.Fatalf("URL %q does not point at the proxy", rawURL)
	}
	return strings.TrimPrefix(parsed.Path, "/chef")
}

type chefUniverseEntry struct {
	LocationType string          `json:"location_type"`
	LocationPath string          `json:"location_path"`
	DownloadURL  string          `json:"download_url"`
	Dependencies json.RawMessage `json:"dependencies"`
}

func decodeChefUniverse(t *testing.T, body []byte) map[string]map[string]chefUniverseEntry {
	t.Helper()
	var universe map[string]map[string]chefUniverseEntry
	if err := json.Unmarshal(body, &universe); err != nil {
		t.Fatalf("decode universe: %v\n%s", err, body)
	}
	return universe
}

func TestChefUniverseRewritesLocations(t *testing.T) {
	s := newFakeSupermarket(t, "")
	h, _ := newChefTestHandler(t, s, s.api.Client())

	w := serveChefRequest(h, "/universe")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != contentTypeJSON {
		t.Errorf("Content-Type = %q, want %q", got, contentTypeJSON)
	}
	body := w.Body.String()
	if strings.Contains(body, s.api.URL) {
		t.Errorf("rewritten universe still points at the upstream: %s", body)
	}

	universe := decodeChefUniverse(t, w.Body.Bytes())
	wantDeps := map[string]map[string]string{
		"apt":             {"7.5.22": `{"compat_resource":">= 0.0.0"}`, "7.6.0": `{}`},
		"compat_resource": {"12.19.1": `{"apt":"~> 7.5"}`},
	}
	for name, versions := range wantDeps {
		for version, deps := range versions {
			entry, ok := universe[name][version]
			if !ok {
				t.Errorf("universe lost %s %s", name, version)
				continue
			}
			if entry.LocationType != "opscode" {
				t.Errorf("%s %s location_type = %q, want opscode", name, version, entry.LocationType)
			}
			if entry.LocationPath != chefTestAPI {
				t.Errorf("%s %s location_path = %q, want %q", name, version, entry.LocationPath, chefTestAPI)
			}
			wantDownload := chefTestAPI + "/cookbooks/" + name + "/versions/" + version + "/download"
			if entry.DownloadURL != wantDownload {
				t.Errorf("%s %s download_url = %q, want %q", name, version, entry.DownloadURL, wantDownload)
			}
			if string(entry.Dependencies) != deps {
				t.Errorf("%s %s dependencies = %s, want %s unchanged", name, version, entry.Dependencies, deps)
			}
		}
	}
	// Constraints keep their spelling rather than being HTML-escaped.
	if !strings.Contains(body, `">= 0.0.0"`) || !strings.Contains(body, `"~> 7.5"`) {
		t.Errorf("dependency constraints were re-escaped: %s", body)
	}
}

func TestChefCookbookMetadataRewritesVersionURLs(t *testing.T) {
	s := newFakeSupermarket(t, "")
	h, _ := newChefTestHandler(t, s, s.api.Client())

	w := serveChefRequest(h, "/api/v1/cookbooks/apt")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var cookbook struct {
		Name          string   `json:"name"`
		LatestVersion string   `json:"latest_version"`
		Versions      []string `json:"versions"`
		ExternalURL   string   `json:"external_url"`
		Deprecated    bool     `json:"deprecated"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &cookbook); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cookbook.LatestVersion != chefTestAPI+"/cookbooks/apt/versions/7.6.0" {
		t.Errorf("latest_version = %q", cookbook.LatestVersion)
	}
	want := []string{chefTestAPI + "/cookbooks/apt/versions/7.6.0", chefTestAPI + "/cookbooks/apt/versions/7.5.22"}
	if strings.Join(cookbook.Versions, ",") != strings.Join(want, ",") {
		t.Errorf("versions = %v, want %v", cookbook.Versions, want)
	}
	if cookbook.Name != "apt" || cookbook.ExternalURL != "https://github.com/sous-chefs/apt" || cookbook.Deprecated {
		t.Errorf("unrelated fields changed: %+v", cookbook)
	}
}

func TestChefVersionMetadataAcceptsBerkshelfVersions(t *testing.T) {
	s := newFakeSupermarket(t, "")
	h, _ := newChefTestHandler(t, s, s.api.Client())

	// Berkshelf writes versions with underscores in cookbook API paths.
	for _, version := range []string{"7_5_22", "7.5.22"} {
		w := serveChefRequest(h, "/api/v1/cookbooks/apt/versions/"+version)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200: %s", version, w.Code, w.Body.String())
		}
		var document struct {
			Version      string          `json:"version"`
			Cookbook     string          `json:"cookbook"`
			File         string          `json:"file"`
			Dependencies json.RawMessage `json:"dependencies"`
			License      string          `json:"license"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &document); err != nil {
			t.Fatalf("%s: decode: %v", version, err)
		}
		if document.Cookbook != chefTestAPI+"/cookbooks/apt" {
			t.Errorf("%s: cookbook = %q", version, document.Cookbook)
		}
		if document.File != chefTestAPI+"/cookbooks/apt/versions/7.5.22/download" {
			t.Errorf("%s: file = %q", version, document.File)
		}
		if string(document.Dependencies) != `{"compat_resource":">= 0.0.0"}` || document.License != "Apache-2.0" || document.Version != "7.5.22" {
			t.Errorf("%s: unrelated fields changed: %+v", version, document)
		}
	}
	// Both spellings are one document, fetched once.
	if got := s.count("/api/v1/cookbooks/apt/versions/7.5.22"); got != 1 {
		t.Errorf("upstream version requests = %d, want 1", got)
	}
}

// TestChefBerkshelfFlow follows the requests Berkshelf makes: the universe,
// then the cookbook version at the universe's location_path, then its file.
// The download redirects to the storage host and is cached. It is then served
// with the upstream down, as is the universe.
func TestChefBerkshelfFlow(t *testing.T) {
	s := newFakeSupermarket(t, "")
	h, proxy := newChefTestHandler(t, s, s.api.Client())

	universe := decodeChefUniverse(t, serveChefRequest(h, "/universe").Body.Bytes())
	locationPath := universe["apt"]["7.5.22"].LocationPath

	versionResponse := serveChefRequest(h, proxyPath(t, locationPath+"/cookbooks/apt/versions/7_5_22"))
	if versionResponse.Code != http.StatusOK {
		t.Fatalf("version status = %d: %s", versionResponse.Code, versionResponse.Body.String())
	}
	var version struct {
		File string `json:"file"`
	}
	if err := json.Unmarshal(versionResponse.Body.Bytes(), &version); err != nil {
		t.Fatalf("decode version: %v", err)
	}

	download := serveChefRequest(h, proxyPath(t, version.File))
	if download.Code != http.StatusOK {
		t.Fatalf("download status = %d: %s", download.Code, download.Body.String())
	}
	if download.Body.String() != chefTestTarball {
		t.Errorf("download body = %q, want the tarball", download.Body.String())
	}
	if got := s.count("files /cookbook_versions/tarballs/34899/original/apt.tgz"); got != 1 {
		t.Errorf("storage host requests = %d, want 1 after following the redirect", got)
	}

	// A second download is a cache hit.
	if again := serveChefRequest(h, proxyPath(t, version.File)); again.Code != http.StatusOK || again.Body.String() != chefTestTarball {
		t.Fatalf("cached download = %d %q", again.Code, again.Body.String())
	}
	if got := s.count("/api/v1/cookbooks/apt/versions/7.5.22/download"); got != 1 {
		t.Errorf("upstream download requests = %d, want 1 (second served from cache)", got)
	}

	// With the upstream down and the metadata TTL lapsed, the cached
	// cookbook and the stored universe are still served.
	s.available.Store(false)
	proxy.MetadataTTL = 0
	if offline := serveChefRequest(h, proxyPath(t, version.File)); offline.Code != http.StatusOK || offline.Body.String() != chefTestTarball {
		t.Errorf("offline download = %d %q, want the cached tarball", offline.Code, offline.Body.String())
	}
	offlineUniverse := serveChefRequest(h, "/universe")
	if offlineUniverse.Code != http.StatusOK {
		t.Fatalf("offline universe status = %d: %s", offlineUniverse.Code, offlineUniverse.Body.String())
	}
	if got := decodeChefUniverse(t, offlineUniverse.Body.Bytes())["apt"]["7.5.22"].DownloadURL; got != version.File {
		t.Errorf("offline universe download_url = %q, want %q", got, version.File)
	}
}

func TestChefUpstreamAuthentication(t *testing.T) {
	const token = "supermarket-token"
	s := newFakeSupermarket(t, token)
	// Credentials are scoped to the Supermarket, as upstream.auth scopes
	// them by URL, so the storage host it redirects to never sees them.
	client := &http.Client{Transport: upstreamhttp.NewTransport(http.DefaultTransport,
		upstreamhttp.AuthFunc(func(requestURL string) (string, string) {
			if strings.HasPrefix(requestURL, s.api.URL+"/") {
				return "Authorization", "Bearer " + token
			}
			return "", ""
		}))}
	h, _ := newChefTestHandler(t, s, client)

	for _, target := range []string{"/universe", "/api/v1/cookbooks/apt", "/api/v1/cookbooks/apt/versions/7.5.22"} {
		if w := serveChefRequest(h, target); w.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200: %s", target, w.Code, w.Body.String())
		}
	}
	download := serveChefRequest(h, "/api/v1/cookbooks/apt/versions/7.5.22/download")
	if download.Code != http.StatusOK || download.Body.String() != chefTestTarball {
		t.Fatalf("download = %d %q", download.Code, download.Body.String())
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, auth := range s.fileAuth {
		if auth != "" {
			t.Errorf("storage host received Authorization %q", auth)
		}
	}
}

func TestChefRejectsInvalidRequests(t *testing.T) {
	s := newFakeSupermarket(t, "")
	h, _ := newChefTestHandler(t, s, s.api.Client())

	tests := []struct {
		target string
		want   int
	}{
		{"/api/v1/cookbooks/.hidden", http.StatusBadRequest},
		{"/api/v1/cookbooks/apt/versions/latest", http.StatusBadRequest},
		{"/api/v1/cookbooks/apt/versions/1.2.3.4", http.StatusBadRequest},
		{"/api/v1/cookbooks/apt/versions/1.x/download", http.StatusBadRequest},
		{"/api/v1/cookbooks/missing", http.StatusNotFound},
		{"/api/v1/cookbooks/apt/versions/9.9.9", http.StatusNotFound},
	}
	for _, tt := range tests {
		if w := serveChefRequest(h, tt.target); w.Code != tt.want {
			t.Errorf("%s: status = %d, want %d: %s", tt.target, w.Code, tt.want, w.Body.String())
		}
	}
}

// An upstream document that cannot be rewritten is refused rather than
// passed through, which would send clients around the proxy.
func TestChefUnreadableUniverseIsBadGateway(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`["not","a","universe"]`))
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	h := NewChefHandler(proxy, chefTestProxyURL, upstream.URL)

	w := serveChefRequest(h, "/universe")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "not") {
		t.Errorf("unreadable document was passed through: %s", w.Body.String())
	}
}

// Switching upstream.chef to another Supermarket keeps the cache, but an
// archive cached from the previous one is not served for a cookbook the new
// one publishes under the same name and version.
func TestChefArchiveCacheDependsOnUpstream(t *testing.T) {
	const target = "/api/v1/cookbooks/apt/versions/7.5.22/download"
	public := newFakeSupermarket(t, "")
	private := newFakeSupermarket(t, "")
	private.tarball = "private cookbook tarball bytes"

	publicHandler, proxy := newChefTestHandler(t, public, public.api.Client())
	if w := serveChefRequest(publicHandler, target); w.Code != http.StatusOK || w.Body.String() != public.tarball {
		t.Fatalf("public download = %d %q", w.Code, w.Body.String())
	}

	privateHandler := NewChefHandler(proxy, chefTestProxyURL, private.api.URL)
	w := serveChefRequest(privateHandler, target)
	if w.Code != http.StatusOK {
		t.Fatalf("private download status = %d: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != private.tarball {
		t.Errorf("private download = %q, want the private Supermarket's tarball", w.Body.String())
	}
	if got := private.count(target); got != 1 {
		t.Errorf("private download requests = %d, want 1", got)
	}

	// Each upstream's archive stays cached under its own identity.
	for _, tt := range []struct {
		handler  *ChefHandler
		upstream *fakeSupermarket
	}{{publicHandler, public}, {privateHandler, private}} {
		if again := serveChefRequest(tt.handler, target); again.Code != http.StatusOK || again.Body.String() != tt.upstream.tarball {
			t.Errorf("cached download from %s = %d %q, want %q", tt.upstream.api.URL, again.Code, again.Body.String(), tt.upstream.tarball)
		}
		if got := tt.upstream.count(target); got != 1 {
			t.Errorf("download requests to %s = %d, want 1 (second served from cache)", tt.upstream.api.URL, got)
		}
	}
}

func TestChefCacheKeyDependsOnUpstream(t *testing.T) {
	a := NewChefHandler(nil, chefTestProxyURL, "https://supermarket.chef.io")
	b := NewChefHandler(nil, chefTestProxyURL, "https://supermarket.example.com/")
	if a.cacheKey("universe") == b.cacheKey("universe") {
		t.Error("two Supermarkets share a universe cache key")
	}
	if containsPathTraversal(a.cacheKey("cookbooks/apt/versions/7.5.22")) {
		t.Error("cache key is not a valid metadata key")
	}
	if a.archiveFilename("apt", "7.5.22") == b.archiveFilename("apt", "7.5.22") {
		t.Error("two Supermarkets share an archive filename")
	}
	if got := a.archiveFilename("apt", "7.5.22"); !strings.HasPrefix(got, "apt-7.5.22-") || !strings.HasSuffix(got, ".tgz") {
		t.Errorf("archive filename = %q, want apt-7.5.22-<upstream>.tgz", got)
	}
	if NewChefHandler(nil, chefTestProxyURL, "https://supermarket.example.com").upstreamKey != b.upstreamKey {
		t.Error("a trailing slash changes the upstream key")
	}
}
