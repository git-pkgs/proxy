package handler

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
