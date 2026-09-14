package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

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
