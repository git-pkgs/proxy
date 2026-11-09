package handler

import (
	"crypto/sha256"
	"encoding/hex"
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

	"github.com/git-pkgs/registries/fetch"
)

const (
	terraformTestHost    = "registry.example"
	terraformTestArchive = "provider zip bytes"
)

// fakeTerraformRegistry serves the parts of the Provider Registry Protocol a
// network mirror needs, in the shapes registry.terraform.io returns them.
// Downloads point at a second server, as the registry points at
// releases.hashicorp.com or GitHub releases.
type fakeTerraformRegistry struct {
	api   *httptest.Server
	files *httptest.Server

	available atomic.Bool
	// providersPath is the providers.v1 entry of service discovery.
	providersPath string
	// archive is the content of every provider archive it serves, and
	// shasum the checksum the download documents give for it.
	archive string
	shasum  string

	mu       sync.Mutex
	requests map[string]int
}

func newFakeTerraformRegistry(t *testing.T) *fakeTerraformRegistry {
	t.Helper()
	s := &fakeTerraformRegistry{providersPath: "/v1/providers/", archive: terraformTestArchive, requests: map[string]int{}}
	sum := sha256.Sum256([]byte(s.archive))
	s.shasum = hex.EncodeToString(sum[:])
	s.available.Store(true)

	s.files = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.count("files " + r.URL.Path)
		if !s.available.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(s.archive))
	}))
	t.Cleanup(s.files.Close)
	s.api = httptest.NewServer(http.HandlerFunc(s.serveAPI))
	t.Cleanup(s.api.Close)
	return s
}

func (s *fakeTerraformRegistry) count(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests[key]++
}

func (s *fakeTerraformRegistry) requestCount(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[key]
}

func (s *fakeTerraformRegistry) totalRequests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, n := range s.requests {
		total += n
	}
	return total
}

func (s *fakeTerraformRegistry) serveAPI(w http.ResponseWriter, r *http.Request) {
	s.count(r.URL.Path)
	if !s.available.Load() {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	providers := strings.TrimSuffix(s.providersPath, "/")
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/.well-known/terraform.json":
		_, _ = fmt.Fprintf(w, `{"modules.v1":"/v1/modules/","providers.v1":%q}`, s.providersPath)
	case r.URL.Path == providers+"/hashicorp/random/versions":
		_, _ = w.Write([]byte(`{"id":"hashicorp/random","versions":[
			{"version":"3.6.0","protocols":["5.0"],"platforms":[{"os":"linux","arch":"amd64"},{"os":"darwin","arch":"arm64"}]},
			{"version":"3.5.1","protocols":["5.0"],"platforms":[{"os":"linux","arch":"amd64"}]}
		],"warnings":null}`))
	case strings.HasPrefix(r.URL.Path, providers+"/hashicorp/random/"):
		rest := strings.TrimPrefix(r.URL.Path, providers+"/hashicorp/random/")
		parts := strings.Split(rest, "/")
		if len(parts) != 4 || parts[1] != "download" || (parts[0] != "3.6.0" && parts[0] != "3.5.1") {
			http.NotFound(w, r)
			return
		}
		version, goos, arch := parts[0], parts[2], parts[3]
		if version == "3.5.1" && goos != "linux" {
			http.NotFound(w, r)
			return
		}
		filename := fmt.Sprintf("terraform-provider-random_%s_%s_%s.zip", version, goos, arch)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"protocols":    []string{"5.0"},
			"os":           goos,
			"arch":         arch,
			"filename":     filename,
			"download_url": s.files.URL + "/terraform-provider-random/" + version + "/" + filename,
			"shasum":       s.shasum,
		})
	default:
		http.NotFound(w, r)
	}
}

// newTerraformTestHandler returns a handler that mirrors s as
// terraformTestHost, with metadata caching on.
func newTerraformTestHandler(t *testing.T, s *fakeTerraformRegistry) (*TerraformHandler, *Proxy) {
	t.Helper()
	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	client := s.api.Client()
	proxy.HTTPClient = client
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(client), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	return NewTerraformHandler(proxy, map[string]string{terraformTestHost: s.api.URL}), proxy
}

func serveTerraformRequest(h *TerraformHandler, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	return w
}

const terraformTestProvider = "/providers/" + terraformTestHost + "/hashicorp/random/"

type terraformArchives struct {
	Archives map[string]struct {
		URL    string   `json:"url"`
		Hashes []string `json:"hashes"`
	} `json:"archives"`
}

func TestTerraformProviderIndexListsVersions(t *testing.T) {
	s := newFakeTerraformRegistry(t)
	h, _ := newTerraformTestHandler(t, s)

	w := serveTerraformRequest(h, terraformTestProvider+"index.json")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != contentTypeJSON {
		t.Errorf("Content-Type = %q, want %q", ct, contentTypeJSON)
	}
	var index struct {
		Versions map[string]json.RawMessage `json:"versions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &index); err != nil {
		t.Fatalf("decode index: %v", err)
	}
	if len(index.Versions) != 2 || string(index.Versions["3.6.0"]) != "{}" || string(index.Versions["3.5.1"]) != "{}" {
		t.Errorf("versions = %s, want 3.6.0 and 3.5.1 as empty objects", w.Body.String())
	}
}

func TestTerraformProviderVersionListsArchives(t *testing.T) {
	s := newFakeTerraformRegistry(t)
	h, _ := newTerraformTestHandler(t, s)

	w := serveTerraformRequest(h, terraformTestProvider+"3.6.0.json")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var doc terraformArchives
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode version: %v", err)
	}
	if len(doc.Archives) != 2 {
		t.Fatalf("archives = %s, want linux_amd64 and darwin_arm64", w.Body.String())
	}
	for platform, archive := range doc.Archives {
		// The URL is relative to the version document, so it resolves
		// under the provider's directory on the proxy.
		if want := "3.6.0/" + platform + ".zip"; archive.URL != want {
			t.Errorf("%s url = %q, want %q", platform, archive.URL, want)
		}
		if len(archive.Hashes) != 1 || archive.Hashes[0] != "zh:"+s.shasum {
			t.Errorf("%s hashes = %v, want [zh:%s]", platform, archive.Hashes, s.shasum)
		}
	}
}

func TestTerraformProviderArchiveIsCached(t *testing.T) {
	s := newFakeTerraformRegistry(t)
	h, proxy := newTerraformTestHandler(t, s)

	var doc terraformArchives
	if err := json.Unmarshal(serveTerraformRequest(h, terraformTestProvider+"3.6.0.json").Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode version: %v", err)
	}
	base, _ := url.Parse("http://proxy.example" + terraformTestProvider + "3.6.0.json")
	ref, _ := url.Parse(doc.Archives["linux_amd64"].URL)
	archivePath := base.ResolveReference(ref).Path

	download := serveTerraformRequest(h, archivePath)
	if download.Code != http.StatusOK || download.Body.String() != terraformTestArchive {
		t.Fatalf("download = %d %q, want the archive", download.Code, download.Body.String())
	}
	if again := serveTerraformRequest(h, archivePath); again.Code != http.StatusOK || again.Body.String() != terraformTestArchive {
		t.Fatalf("cached download = %d %q", again.Code, again.Body.String())
	}
	if got := s.requestCount("files /terraform-provider-random/3.6.0/terraform-provider-random_3.6.0_linux_amd64.zip"); got != 1 {
		t.Errorf("archive requests = %d, want 1 (second served from cache)", got)
	}

	// With the upstream down and the metadata TTL lapsed, the cached
	// archive and the stored documents are still served.
	s.available.Store(false)
	proxy.MetadataTTL = 0
	if offline := serveTerraformRequest(h, archivePath); offline.Code != http.StatusOK || offline.Body.String() != terraformTestArchive {
		t.Errorf("offline download = %d %q, want the cached archive", offline.Code, offline.Body.String())
	}
	for _, file := range []string{"index.json", "3.6.0.json"} {
		if offline := serveTerraformRequest(h, terraformTestProvider+file); offline.Code != http.StatusOK {
			t.Errorf("offline %s status = %d: %s", file, offline.Code, offline.Body.String())
		}
	}
}

func TestTerraformProviderArchiveChecksumMismatch(t *testing.T) {
	s := newFakeTerraformRegistry(t)
	s.shasum = strings.Repeat("0", 64)
	h, _ := newTerraformTestHandler(t, s)

	if w := serveTerraformRequest(h, terraformTestProvider+"3.6.0/linux_amd64.zip"); w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 for an archive that doesn't match its shasum", w.Code)
	}
	// Nothing was cached, so the next request fetches again.
	serveTerraformRequest(h, terraformTestProvider+"3.6.0/linux_amd64.zip")
	if got := s.requestCount("files /terraform-provider-random/3.6.0/terraform-provider-random_3.6.0_linux_amd64.zip"); got != 2 {
		t.Errorf("archive requests = %d, want 2", got)
	}
}

func TestTerraformProviderDiscovery(t *testing.T) {
	s := newFakeTerraformRegistry(t)
	s.providersPath = "/api/providers/v1/"
	h, _ := newTerraformTestHandler(t, s)

	if w := serveTerraformRequest(h, terraformTestProvider+"index.json"); w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if got := s.requestCount("/api/providers/v1/hashicorp/random/versions"); got != 1 {
		t.Errorf("versions requests at the discovered path = %d, want 1", got)
	}
}

func TestTerraformProviderNotFound(t *testing.T) {
	s := newFakeTerraformRegistry(t)
	h, _ := newTerraformTestHandler(t, s)

	for _, target := range []string{
		"/providers/" + terraformTestHost + "/hashicorp/missing/index.json",
		terraformTestProvider + "9.9.9.json",
		terraformTestProvider + "3.5.1/darwin_arm64.zip",
	} {
		if w := serveTerraformRequest(h, target); w.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", target, w.Code)
		}
	}
}

func TestTerraformProviderRejectsUnknownAndInvalidRequests(t *testing.T) {
	s := newFakeTerraformRegistry(t)
	h, _ := newTerraformTestHandler(t, s)

	for target, want := range map[string]int{
		// A hostname without a configured registry is never fetched.
		"/providers/evil.example/hashicorp/random/index.json":               http.StatusNotFound,
		"/providers/" + terraformTestHost + "/hashi.corp/random/index.json": http.StatusBadRequest,
		terraformTestProvider + "versions.txt":                              http.StatusNotFound,
		terraformTestProvider + "3.6.0/linux_amd64.tar.gz":                  http.StatusNotFound,
		terraformTestProvider + "3.6.0/linux-amd64.zip":                     http.StatusNotFound,
		terraformTestProvider + "3.6.0%2F..%2Fx/linux_amd64.zip":            http.StatusBadRequest,
	} {
		if w := serveTerraformRequest(h, target); w.Code != want {
			t.Errorf("%s status = %d, want %d", target, w.Code, want)
		}
	}
	if got := s.totalRequests(); got != 0 {
		t.Errorf("upstream requests = %d, want 0", got)
	}
}
