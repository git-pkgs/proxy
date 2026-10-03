package handler

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/registries/fetch"
)

func TestNuGetCooldownRoutes(t *testing.T) {
	for _, disableCompression := range []bool{false, true} {
		t.Run(map[bool]string{false: "transport gzip", true: "explicit gzip"}[disableCompression], func(t *testing.T) {
			proxy, db, store, fetcher := setupTestProxy(t)
			proxy.Cooldown = &cooldown.Config{Default: "14d"}
			proxy.CacheMetadata = true
			proxy.MetadataTTL = time.Hour
			seedPackage(t, db, store, "nuget", "testpkg", "2.0.0", "testpkg.2.0.0.nupkg", "cached package")
			seedPackage(t, db, store, "nuget", "testpkg", "1.0.0", "testpkg.1.0.0.nupkg", "old package")
			metadataRequests := 0
			upstream := newNuGetCooldownUpstream(t, &metadataRequests)
			defer upstream.Close()
			transport := http.DefaultTransport.(*http.Transport).Clone()
			transport.DisableCompression = disableCompression
			defer transport.CloseIdleConnections()
			proxy.HTTPClient = &http.Client{Transport: transport}
			h := NewNuGetHandlerWithUpstreams(proxy, "http://proxy.test", upstream.URL, upstream.URL)
			routes := http.StripPrefix("/nuget", h.Routes())
			get := func(path string, status int) *httptest.ResponseRecorder {
				t.Helper()
				return nugetGet(t, routes, path, status)
			}
			list := get("/nuget/v3-flatcontainer/testpkg/index.json", http.StatusOK)
			if got := strings.TrimSpace(list.Body.String()); got != `{"versions":["1.0.0"]}` {
				t.Fatalf("filtered list = %s", got)
			}
			index := get("/nuget"+nugetRegistrationPath+"testpkg/index.json", http.StatusOK)
			if strings.Contains(index.Body.String(), `"version":"2.0.0"`) || strings.Contains(index.Body.String(), upstream.URL) {
				t.Fatalf("registration leaks blocked leaf or upstream link: %s", index.Body.String())
			}
			if index.Header().Get("Content-Encoding") != "" || !json.Valid(index.Body.Bytes()) {
				t.Fatal("registration must be decoded JSON")
			}
			var doc struct {
				Items []struct {
					ID           string `json:"@id"`
					Count        int
					Lower, Upper string
					Items        []struct {
						ID             string `json:"@id"`
						PackageContent string
					}
				}
			}
			if err := json.Unmarshal(index.Body.Bytes(), &doc); err != nil {
				t.Fatal(err)
			}
			if len(doc.Items) != 1 || doc.Items[0].Count != 1 || doc.Items[0].Upper != "1.0.0" {
				t.Fatalf("incorrect page: %+v", doc)
			}
			get(doc.Items[0].ID, http.StatusOK)
			get(doc.Items[0].Items[0].ID, http.StatusOK)
			get(doc.Items[0].Items[0].PackageContent, http.StatusOK)
			get("/nuget"+nugetRegistrationPath+"testpkg/2.0.0.json", http.StatusNotFound)
			get("/nuget/v3-flatcontainer/TestPkg/2.0.0/testpkg.2.0.0.nupkg", http.StatusNotFound)
			get("/nuget/v3-flatcontainer/testpkg/2.0.0/testpkg.nuspec", http.StatusNotFound)
			if fetcher.fetchCalled {
				t.Fatal("blocked or cached downloads must not fetch artifacts")
			}

			// Reevaluate fresh, unfiltered metadata under a changed package policy.
			requestsBefore := metadataRequests
			proxy.Cooldown = &cooldown.Config{Default: "14d", Packages: map[string]string{"pkg:nuget/testpkg": "1d"}}
			list = get("/nuget/v3-flatcontainer/testpkg/index.json", http.StatusOK)
			if !strings.Contains(list.Body.String(), "2.0.0") {
				t.Fatal("fresh metadata retained the previous policy")
			}
			get("/nuget/v3-flatcontainer/testpkg/2.0.0/testpkg.2.0.0.nupkg", http.StatusOK)
			if metadataRequests != requestsBefore {
				t.Fatal("fresh metadata should be reused")
			}
		})
	}
}

func nugetGet(t *testing.T, routes http.Handler, path string, status int) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	routes.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != status {
		t.Fatalf("GET %s: status %d, want %d: %s", path, w.Code, status, w.Body.String())
	}
	return w
}

func TestNuGetMetadataWithoutEffectiveCooldown(t *testing.T) {
	for _, tt := range []struct {
		name   string
		policy *cooldown.Config
	}{
		{"package exemption", &cooldown.Config{Default: "14d", Packages: map[string]string{"pkg:nuget/testpkg": "0"}}},
		{"ecosystem exemption", &cooldown.Config{Default: "14d", Ecosystems: map[string]string{"nuget": "0"}}},
		{"other ecosystem only", &cooldown.Config{Ecosystems: map[string]string{"npm": "14d"}}},
		{"other package only", &cooldown.Config{Packages: map[string]string{"pkg:nuget/other": "14d"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const body = `{"versions":["1.0.0","2.0.0"]}`
			const pagePath = nugetRegistrationPath + "testpkg/page/1.0.0/2.0.0.json"
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v3-flatcontainer/testpkg/index.json":
					_, _ = io.WriteString(w, body)
				case nugetRegistrationPath + "testpkg/index.json":
					_, _ = io.WriteString(w, `{"count":1,"items":[{"@id":"`+pagePath+`","count":2,"lower":"1.0.0","upper":"2.0.0"}]}`)
				default:
					t.Errorf("unnecessary registration request: %s", r.URL.Path)
					http.Error(w, "registration unavailable", http.StatusServiceUnavailable)
				}
			}))
			defer upstream.Close()
			p := nugetTestProxy()
			p.Cooldown = tt.policy
			h := NewNuGetHandlerWithUpstreams(p, "http://proxy.test", upstream.URL, upstream.URL)
			w := nugetGet(t, h.Routes(), "/v3-flatcontainer/TestPkg/index.json", http.StatusOK)
			if got := strings.TrimSpace(w.Body.String()); got != body {
				t.Fatalf("version list = %s, want %s", got, body)
			}
			w = nugetGet(t, h.Routes(), nugetRegistrationPath+"testpkg/index.json", http.StatusOK)
			if !strings.Contains(w.Body.String(), `"@id":"http://proxy.test/nuget`+pagePath+`"`) {
				t.Fatalf("registration page link was not rewritten: %s", w.Body.String())
			}
		})
	}
}

func TestNuGetCooldownColdDownload(t *testing.T) {
	for _, tt := range []struct {
		name, published string
		policy          *cooldown.Config
		want            int
	}{
		{"recent", time.Now().Add(-time.Hour).Format(time.RFC3339), &cooldown.Config{Default: "14d"}, http.StatusNotFound},
		{"missing timestamp", "", &cooldown.Config{Default: "14d"}, http.StatusOK},
		{"package exemption", time.Now().Add(-time.Hour).Format(time.RFC3339), &cooldown.Config{Default: "14d", Packages: map[string]string{"pkg:nuget/testpkg": "0"}}, http.StatusOK},
		{"ecosystem override", time.Now().Add(-time.Hour).Format(time.RFC3339), &cooldown.Config{Ecosystems: map[string]string{"nuget": "14d"}}, http.StatusNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p, _, _, fetcher := setupTestProxy(t)
			p.Cooldown = tt.policy
			fetcher.artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader("package")), ContentType: "application/octet-stream"}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]string{"published": tt.published})
			}))
			defer upstream.Close()
			h := NewNuGetHandlerWithUpstreams(p, "http://proxy.test", upstream.URL, upstream.URL)
			nugetGet(t, h.Routes(), "/v3-flatcontainer/testpkg/2.0.0/testpkg.2.0.0.nupkg", tt.want)
			if fetcher.fetchCalled != (tt.want == http.StatusOK) {
				t.Errorf("artifact fetch called = %v", fetcher.fetchCalled)
			}
		})
	}
}

// TestNuGetCooldownConcurrentDownloads asserts that a download which joins
// another request's metadata fetch still applies the cooldown. Each caller
// decodes the leaf in its own validate callback, so a waiter on the shared
// fetch must run its own rather than finding no document and allowing it.
func TestNuGetCooldownConcurrentDownloads(t *testing.T) {
	p, db, store, fetcher := setupTestProxy(t)
	p.Cooldown = &cooldown.Config{Default: "14d"}
	seedPackage(t, db, store, "nuget", "testpkg", "2.0.0", "testpkg.2.0.0.nupkg", "cached package")
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		enteredOnce.Do(func() { close(entered) })
		<-release
		_ = json.NewEncoder(w).Encode(map[string]string{"published": time.Now().Add(-time.Hour).Format(time.RFC3339)})
	}))
	// Cleanups run last first: release the held request before closing the
	// server, so a failed wait can't hang the test.
	t.Cleanup(upstream.Close)
	t.Cleanup(releaseUpstream)
	routes := NewNuGetHandlerWithUpstreams(p, "http://proxy.test", upstream.URL, upstream.URL).Routes()

	statuses := make(chan int, 2)
	download := func() {
		w := httptest.NewRecorder()
		routes.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v3-flatcontainer/testpkg/2.0.0/testpkg.2.0.0.nupkg", nil))
		statuses <- w.Code
	}
	go download()
	<-entered
	go download()
	target := upstream.URL + nugetRegistrationPath + "testpkg/2.0.0.json"
	key := metadataCoalesceKey("nuget", fmt.Sprintf("_cooldown/%x", sha256.Sum256([]byte(target))), target, contentTypeJSON, "", true)
	waitForMetadataWaiters(t, p, key, 1)
	releaseUpstream()

	for range 2 {
		if status := <-statuses; status != http.StatusNotFound {
			t.Errorf("download status = %d, want %d", status, http.StatusNotFound)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("upstream metadata requests = %d, want 1", got)
	}
	if fetcher.fetchCalled {
		t.Error("blocked downloads must not fetch artifacts")
	}
}

func TestNuGetRegistrationServiceAliases(t *testing.T) {
	h := NewNuGetHandler(nugetTestProxy(), "http://proxy.test")
	for _, tt := range []struct{ service, path string }{
		{"RegistrationsBaseUrl", "/v3/registration5-semver1/"},
		{"RegistrationsBaseUrl/3.0.0-beta", "/v3/registration5-semver1/"},
		{"RegistrationsBaseUrl/3.0.0-rc", "/v3/registration5-semver1/"},
		{"RegistrationsBaseUrl/3.4.0", "/v3/registration5-gz-semver1/"},
		{"RegistrationsBaseUrl/3.6.0", nugetRegistrationPath},
		{"RegistrationsBaseUrl/Versioned", nugetRegistrationPath},
	} {
		t.Run(tt.service, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tt.path+"testpkg/index.json" {
					t.Errorf("wrong hive: %s", r.URL.Path)
				}
				_, _ = io.WriteString(w, `{"count":0,"items":[]}`)
			}))
			defer upstream.Close()
			h.upstreamURL = upstream.URL
			h.proxy.Cooldown = &cooldown.Config{Default: "14d"}
			body := []byte(`{"resources":[{"@id":"` + upstream.URL + tt.path + `","@type":"` + tt.service + `"}]}`)
			out, err := h.rewriteServiceIndex(body)
			if err != nil {
				t.Fatal(err)
			}
			var doc struct {
				Resources []struct {
					ID string `json:"@id"`
				}
			}
			if err := json.Unmarshal(out, &doc); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			http.StripPrefix("/nuget", h.Routes()).ServeHTTP(w, httptest.NewRequest(http.MethodGet, doc.Resources[0].ID+"testpkg/index.json", nil))
			if w.Code != http.StatusOK {
				t.Fatalf("alias route status = %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestNuGetCooldownMetadataErrors(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		status     int
	}{
		{"upstream failure", "unavailable", http.StatusServiceUnavailable},
		{"invalid JSON", "broken JSON", http.StatusOK},
		{"null", "null", http.StatusOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer upstream.Close()
			p := nugetTestProxy()
			p.Cooldown = &cooldown.Config{Default: "14d"}
			h := NewNuGetHandlerWithUpstreams(p, "http://proxy.test", upstream.URL, upstream.URL)
			for _, path := range []string{"/v3-flatcontainer/testpkg/index.json", "/v3-flatcontainer/testpkg/2.0.0/testpkg.2.0.0.nupkg", nugetRegistrationPath + "testpkg/index.json"} {
				w := httptest.NewRecorder()
				h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
				if w.Code != http.StatusBadGateway {
					t.Errorf("GET %s: %d, want 502", path, w.Code)
				}
			}
		})
	}
}

func TestNuGetCooldownRejectsUnsafePageLinks(t *testing.T) {
	for _, link := range []string{"https://other.example/page.json", "/v3/registration5-gz-semver2/other/page/1/2.json", "page/../index.json", "index.json"} {
		t.Run(link, func(t *testing.T) {
			requests := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{"@id": link}}})
			}))
			defer upstream.Close()
			p := nugetTestProxy()
			p.Cooldown = &cooldown.Config{Default: "14d"}
			h := NewNuGetHandlerWithUpstreams(p, "http://proxy.test", upstream.URL, upstream.URL)
			nugetGet(t, h.Routes(), nugetRegistrationPath+"testpkg/index.json", http.StatusBadGateway)
			if requests != 1 {
				t.Fatalf("unsafe page link was followed (%d requests)", requests)
			}
		})
	}
}

func TestNuGetCooldownDecompressedMetadataLimit(t *testing.T) {
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	_, _ = io.WriteString(gz, `{"padding":"`+strings.Repeat("x", 2048)+`"}`)
	_ = gz.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(compressed.Bytes())
	}))
	defer upstream.Close()
	p := nugetTestProxy()
	p.Cooldown = &cooldown.Config{Default: "14d"}
	p.MetadataMaxSize = 1024
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	defer transport.CloseIdleConnections()
	p.HTTPClient = &http.Client{Transport: transport}
	h := NewNuGetHandlerWithUpstreams(p, "http://proxy.test", upstream.URL, upstream.URL)
	nugetGet(t, h.Routes(), nugetRegistrationPath+"testpkg/index.json", http.StatusBadGateway)
}

func newNuGetCooldownUpstream(t *testing.T, metadataRequests *int) *httptest.Server {
	t.Helper()
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		(*metadataRequests)++
		base := upstream.URL + nugetRegistrationPath + "testpkg/"
		leaf := func(version string, age time.Duration) map[string]any {
			return map[string]any{
				"@id":            base + version + ".json",
				"packageContent": upstream.URL + "/v3-flatcontainer/testpkg/" + version + "/testpkg." + version + ".nupkg",
				"catalogEntry":   map[string]any{"id": "TestPkg", "version": version, "published": time.Now().Add(-age).Format(time.RFC3339)},
			}
		}
		page := map[string]any{"@id": base + "page/1.0.0/2.0.0.json", "lower": "1.0.0", "upper": "2.0.0", "count": 2,
			"parent": base + "index.json", "items": []any{leaf("1.0.0", 30*24*time.Hour), leaf("2.0.0", 2*24*time.Hour)}}
		var body any
		switch r.URL.Path {
		case "/v3-flatcontainer/testpkg/index.json":
			body = map[string]any{"versions": []string{"1.0.0", "2.0.0"}}
		case nugetRegistrationPath + "testpkg/index.json":
			// This index deliberately does not inline its leaves.
			body = map[string]any{"count": 1, "items": []any{map[string]any{
				"@id": page["@id"], "count": 2, "lower": "1.0.0", "upper": "2.0.0",
			}}}
		case nugetRegistrationPath + "testpkg/page/1.0.0/2.0.0.json":
			body = page
		case nugetRegistrationPath + "testpkg/1.0.0.json":
			body = map[string]any{"published": time.Now().Add(-30 * 24 * time.Hour).Format(time.RFC3339)}
		case nugetRegistrationPath + "testpkg/2.0.0.json":
			body = map[string]any{"published": time.Now().Add(-2 * 24 * time.Hour).Format(time.RFC3339)}
		default:
			t.Errorf("unexpected metadata request: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_ = json.NewEncoder(gz).Encode(body)
		_ = gz.Close()
	}))

	return upstream
}

func TestNuGetCooldownLegacyRegistration(t *testing.T) {
	for _, service := range []string{"RegistrationsBaseUrl", "RegistrationsBaseUrl/3.0.0-beta", "RegistrationsBaseUrl/3.0.0-rc", "RegistrationsBaseUrl/3.4.0"} {
		t.Run(service, func(t *testing.T) {
			prefix := "/v3/registration5-semver1/"
			if service == "RegistrationsBaseUrl/3.4.0" {
				prefix = "/v3/registration5-gz-semver1/"
			}
			upstream := newNuGetLegacyUpstream(t, service, prefix)
			defer upstream.Close()
			p, db, store, fetcher := setupTestProxy(t)
			p.Cooldown = &cooldown.Config{Default: "14d"}
			seedPackage(t, db, store, "nuget", "testpkg", "1.0.0", "testpkg.1.0.0.nupkg", "cached old package")
			seedPackage(t, db, store, "nuget", "testpkg", "2.0.0", "testpkg.2.0.0.nupkg", "cached recent package")
			h := NewNuGetHandlerWithUpstreams(p, "http://proxy.test", upstream.URL+"/feed", upstream.URL)
			list := nugetGet(t, h.Routes(), "/v3-flatcontainer/testpkg/index.json", http.StatusOK)
			if strings.TrimSpace(list.Body.String()) != `{"versions":["1.0.0"]}` {
				t.Fatalf("incorrect version list: %s", list.Body.String())
			}
			nugetGet(t, h.Routes(), "/v3-flatcontainer/testpkg/1.0.0/testpkg.1.0.0.nupkg", http.StatusOK)
			nugetGet(t, h.Routes(), "/v3-flatcontainer/testpkg/2.0.0/testpkg.2.0.0.nupkg", http.StatusNotFound)
			if fetcher.fetchCalled {
				t.Fatal("cached or blocked package must not be fetched")
			}
		})
	}
}

func newNuGetLegacyUpstream(t *testing.T, service, prefix string) *httptest.Server {
	t.Helper()
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/feed")
		base := upstream.URL + "/feed" + prefix + "testpkg/"
		published := func(age time.Duration) string { return time.Now().Add(-age).Format(time.RFC3339) }
		var body any
		switch path {
		case "/v3/index.json":
			body = map[string]any{"resources": []any{map[string]string{"@id": upstream.URL + "/feed" + prefix, "@type": service}}}
		case "/v3-flatcontainer/testpkg/index.json":
			body = map[string]any{"versions": []string{"1.0.0", "2.0.0"}}
		case prefix + "testpkg/index.json":
			body = map[string]any{"items": []any{map[string]any{"@id": base + "page/1.0.0/2.0.0.json"}}}
		case prefix + "testpkg/page/1.0.0/2.0.0.json":
			body = map[string]any{"items": []any{
				map[string]any{"catalogEntry": map[string]string{"id": "testpkg", "version": "1.0.0", "published": published(30 * 24 * time.Hour)}},
				map[string]any{"catalogEntry": map[string]string{"id": "testpkg", "version": "2.0.0", "published": published(time.Hour)}},
			}}
		case prefix + "testpkg/1.0.0.json":
			body = map[string]string{"published": published(30 * 24 * time.Hour)}
		case prefix + "testpkg/2.0.0.json":
			body = map[string]string{"published": published(time.Hour)}
		default:
			if !strings.HasPrefix(path, nugetRegistrationPath) {
				t.Errorf("unexpected request: %s", r.URL.Path)
			}
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	return upstream
}

func TestNuGetRegistrationDoesNotFallbackOnFailure(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusUnauthorized, http.StatusOK} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != nugetRegistrationPath+"testpkg/index.json" {
					t.Errorf("must not switch registration hive on failure: %s", r.URL.Path)
				}
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "invalid metadata")
			}))
			defer upstream.Close()
			h := NewNuGetHandlerWithUpstreams(nugetTestProxy(), "http://proxy.test", upstream.URL, upstream.URL)
			if _, _, err := h.nugetRegistrationMetadata(t.Context(), "testpkg/index.json"); err == nil {
				t.Fatal("expected metadata error")
			}
		})
	}
}

func TestNuGetMetadataPreservesValidCache(t *testing.T) {
	for _, invalid := range []string{"broken JSON", "null", "[]", string([]byte{0x1f, 0x8b, 0x00})} {
		t.Run(invalid, func(t *testing.T) {
			const good = `{"published":"2020-01-01T00:00:00Z"}`
			var response atomic.Value
			response.Store(good)
			var requests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) > 1 && r.Header.Get("If-None-Match") != `"good"` {
					t.Errorf("cached ETag was replaced: %s", r.Header.Get("If-None-Match"))
				}
				body := response.Load().(string)
				etag := `"good"`
				if body == invalid {
					etag = `"bad"`
				}
				w.Header().Set("ETag", etag)
				_, _ = io.WriteString(w, body)
			}))
			defer upstream.Close()
			p, _, _, _ := setupTestProxy(t)
			p.CacheMetadata = true
			h := NewNuGetHandlerWithUpstreams(p, "http://proxy.test", upstream.URL, upstream.URL)
			check := func(want string) {
				t.Helper()
				doc, err := h.nugetMetadata(t.Context(), nugetRegistrationPath+"testpkg/1.0.0.json")
				if err != nil || doc["published"] != want {
					t.Fatalf("metadata = %v, err = %v, want publication %s", doc, err, want)
				}
			}
			check("2020-01-01T00:00:00Z")
			response.Store(invalid)
			check("2020-01-01T00:00:00Z") // Bad 200 must fall back without overwriting.
			p.MetadataTTL = time.Hour
			check("2020-01-01T00:00:00Z") // The on-disk cache must still be usable.
			if requests.Load() != 2 {
				t.Fatalf("requests = %d, want 2", requests.Load())
			}
			p.MetadataTTL = 0
			response.Store(`{"published":"2021-01-01T00:00:00Z"}`)
			check("2021-01-01T00:00:00Z") // A later valid response replaces the cache.
		})
	}
}

func TestNuGetMetadataInvalidResponseNotCached(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			_, _ = io.WriteString(w, "invalid JSON")
			return
		}
		_, _ = io.WriteString(w, `{"published":"2020-01-01T00:00:00Z"}`)
	}))
	defer upstream.Close()
	p, _, _, _ := setupTestProxy(t)
	p.CacheMetadata = true
	p.MetadataTTL = time.Hour
	h := NewNuGetHandlerWithUpstreams(p, "http://proxy.test", upstream.URL, upstream.URL)
	path := nugetRegistrationPath + "testpkg/1.0.0.json"
	if _, err := h.nugetMetadata(t.Context(), path); err == nil {
		t.Fatal("invalid response without a usable cache must fail")
	}
	if _, err := h.nugetMetadata(t.Context(), path); err != nil {
		t.Fatalf("invalid response was cached: %v", err)
	}
	if requests.Load() != 2 {
		t.Fatalf("requests = %d, want 2", requests.Load())
	}
}

func TestNuGetRegistrationDoesNotGuessUnadvertisedAliases(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case nugetRegistrationPath + "testpkg/index.json":
			http.NotFound(w, r)
		case "/v3/index.json":
			_, _ = io.WriteString(w, `{"resources":[{"@type":"UnrelatedService","@id":"https://other.example/"}]}`)
		default:
			t.Errorf("unadvertised endpoint requested: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	h := NewNuGetHandlerWithUpstreams(nugetTestProxy(), "http://proxy.test", upstream.URL, upstream.URL)
	_, _, err := h.nugetRegistrationMetadata(t.Context(), "testpkg/index.json")
	if !errors.Is(err, ErrUpstreamNotFound) {
		t.Fatalf("error = %v, want metadata not found", err)
	}
}
