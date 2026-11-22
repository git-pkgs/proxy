package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/registries/fetch"
)

// nuget.org reports 1900-01-01 as the publish time of an unlisted version,
// so unlisting a release must not make it look old enough to leave its
// cooldown. The time it was created is in its catalog entry.
func TestNuGetCooldownUnlistedDownload(t *testing.T) {
	const unlisted = "1900-01-01T00:00:00+00:00"
	const catalogPath = "/v3/catalog0/data/2026.11.20.10.00.00/testpkg.2.0.0.json"
	recent := time.Now().Add(-time.Hour).Format(time.RFC3339)
	old := time.Now().AddDate(-2, 0, 0).Format(time.RFC3339)

	for _, tt := range []struct {
		name string
		// leaf is the registration leaf; CATALOG stands for the upstream's
		// link to the catalog entry.
		leaf string
		// catalog is the catalog entry, served with catalogStatus.
		catalog       string
		catalogStatus int
		want          int
		wantCatalog   int
	}{
		{"created recently", `{"published":"` + unlisted + `","listed":false,"catalogEntry":"CATALOG"}`,
			`{"created":"` + recent + `","published":"1900-01-01T00:00:00Z","listed":false}`, http.StatusOK, http.StatusNotFound, 1},
		{"created long ago", `{"published":"` + unlisted + `","listed":false,"catalogEntry":"CATALOG"}`,
			`{"created":"` + old + `","published":"1900-01-01T00:00:00Z","listed":false}`, http.StatusOK, http.StatusOK, 1},
		{"catalog entry unavailable", `{"published":"` + unlisted + `","listed":false,"catalogEntry":"CATALOG"}`,
			`unavailable`, http.StatusServiceUnavailable, http.StatusBadGateway, 1},
		{"catalog entry without a creation time", `{"published":"` + unlisted + `","catalogEntry":"CATALOG"}`,
			`{"published":"1900-01-01T00:00:00Z"}`, http.StatusOK, http.StatusOK, 1},
		{"no catalog entry", `{"published":"` + unlisted + `","listed":false}`,
			``, http.StatusNotFound, http.StatusOK, 0},
		{"catalog entry on another host", `{"published":"` + unlisted + `","catalogEntry":"https://catalog.example.com` + catalogPath + `"}`,
			``, http.StatusNotFound, http.StatusOK, 0},
		{"listed and recent", `{"published":"` + recent + `","listed":true,"catalogEntry":"CATALOG"}`,
			`{"created":"` + old + `"}`, http.StatusOK, http.StatusNotFound, 0},
		{"listed and old", `{"published":"` + old + `","listed":true,"catalogEntry":"CATALOG"}`,
			`{"created":"` + recent + `"}`, http.StatusOK, http.StatusOK, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p, _, _, fetcher := setupTestProxy(t)
			p.Cooldown = &cooldown.Config{Default: "14d"}
			fetcher.artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader("package")), ContentType: "application/octet-stream"}
			catalogRequests := 0
			var upstream *httptest.Server
			upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == catalogPath {
					catalogRequests++
					w.WriteHeader(tt.catalogStatus)
					_, _ = io.WriteString(w, tt.catalog)
					return
				}
				_, _ = io.WriteString(w, strings.ReplaceAll(tt.leaf, "CATALOG", upstream.URL+catalogPath))
			}))
			defer upstream.Close()
			h := NewNuGetHandlerWithUpstreams(p, "http://proxy.test", upstream.URL, upstream.URL)

			nugetGet(t, h.Routes(), "/v3-flatcontainer/testpkg/2.0.0/testpkg.2.0.0.nupkg", tt.want)
			if fetcher.fetchCalled != (tt.want == http.StatusOK) {
				t.Errorf("artifact fetch called = %v", fetcher.fetchCalled)
			}
			if catalogRequests != tt.wantCatalog {
				t.Errorf("catalog requests = %d, want %d", catalogRequests, tt.wantCatalog)
			}
		})
	}
}

// The catalog entry of a leaf is decoded as a link when it is one.
func TestDecodeNuGetTimesCatalogLink(t *testing.T) {
	for body, want := range map[string]string{
		`{"catalogEntry":"https://api.nuget.org/v3/catalog0/data/x/a.1.0.0.json","published":"1900-01-01T00:00:00+00:00"}`: "https://api.nuget.org/v3/catalog0/data/x/a.1.0.0.json",
		`{"catalogEntry":{"@id":"https://api.nuget.org/v3/catalog0/data/x/a.1.0.0.json","version":"1.0.0"}}`:               "",
		`{"published":"2026-01-01T00:00:00Z"}`: "",
	} {
		document, err := decodeNuGetTimes([]byte(body))
		if err != nil {
			t.Fatalf("decodeNuGetTimes(%s): %v", body, err)
		}
		if document.CatalogLink != want {
			t.Errorf("decodeNuGetTimes(%s).CatalogLink = %q, want %q", body, document.CatalogLink, want)
		}
	}
}

// Only a link into the upstream is fetched as a catalog entry.
func TestNuGetCatalogPath(t *testing.T) {
	p, _, _, _ := setupTestProxy(t)
	h := NewNuGetHandlerWithUpstreams(p, "http://proxy.test", "https://nuget.example.com/feed", "")
	for link, want := range map[string]string{
		"https://nuget.example.com/feed/v3/catalog0/data/x/a.1.0.0.json": "/v3/catalog0/data/x/a.1.0.0.json",
		"": "",
		"http://nuget.example.com/feed/v3/catalog0/a.json":           "",
		"https://other.example.com/feed/v3/catalog0/a.json":          "",
		"https://nuget.example.com/feedback/v3/catalog0/a.json":      "",
		"https://nuget.example.com/v3/catalog0/a.json":               "",
		"https://nuget.example.com/feed/v3/../../private/a.json":     "",
		"https://nuget.example.com/feed/v3/catalog0/a.json?token=x":  "",
		"https://nuget.example.com/feed/v3/catalog0/a.json#fragment": "",
		"https://nuget.example.com/feed/%2e%2e/private/a.json":       "",
		"://bad": "",
	} {
		got, ok := h.nugetCatalogPath(link)
		if got != want || ok != (want != "") {
			t.Errorf("nugetCatalogPath(%q) = %q, %v; want %q", link, got, ok, want)
		}
	}
}
