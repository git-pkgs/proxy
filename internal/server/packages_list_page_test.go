package server

import (
	"database/sql"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/git-pkgs/proxy/internal/database"
)

func seedFilterPackage(t *testing.T, db *database.DB, ecosystem, name string, cached ...bool) {
	t.Helper()
	pkgPURL := "pkg:" + ecosystem + "/" + name
	if err := db.UpsertPackage(&database.Package{PURL: pkgPURL, Ecosystem: ecosystem, Name: name}); err != nil {
		t.Fatal(err)
	}
	versionPURL := pkgPURL + "@1.0.0"
	if err := db.UpsertVersion(&database.Version{PURL: versionPURL, PackagePURL: pkgPURL}); err != nil {
		t.Fatal(err)
	}
	for i, stored := range cached {
		filename := fmt.Sprintf("%s-%d.tgz", name, i)
		if err := db.UpsertArtifact(&database.Artifact{
			VersionPURL: versionPURL,
			Filename:    filename,
			UpstreamURL: "https://example.test/" + filename,
			StoragePath: sql.NullString{String: filename, Valid: stored},
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPackagesListFilters(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()
	seedFilterPackage(t, ts.db, "npm", "express", true, true, false)
	seedFilterPackage(t, ts.db, "npm", "lodash", true)
	seedFilterPackage(t, ts.db, "cargo", "serde", true)
	seedFilterPackage(t, ts.db, "gem", "metadata-only")
	seedFilterPackage(t, ts.db, "pypi", "evicted", false)
	seedFilterPackage(t, ts.db, "npm", "uncached", false)

	for _, tc := range []struct {
		ecosystem string
		count     int
		names     []string
	}{
		{"", 3, []string{"express", "lodash", "serde"}},
		{"npm", 2, []string{"express", "lodash"}},
		{"cargo", 1, []string{"serde"}},
		{"gem", 0, nil},
	} {
		t.Run("ecosystem="+tc.ecosystem, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/ui/packages?sort=name&ecosystem="+tc.ecosystem, nil)
			w := httptest.NewRecorder()
			ts.handler.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			assertFilterPills(t, body, tc.ecosystem, "name", map[string]string{"": "3", "npm": "2", "cargo": "1"})
			if !strings.Contains(body, fmt.Sprintf("%d packages", tc.count)) {
				t.Errorf("missing heading count %d", tc.count)
			}
			links := regexp.MustCompile(`<a href="/ui/package/[^/]+/([^"]+)"`).FindAllStringSubmatch(body, -1)
			var names []string
			for _, link := range links {
				names = append(names, link[1])
			}
			if strings.Join(names, ",") != strings.Join(tc.names, ",") {
				t.Errorf("listed packages = %v, want %v", names, tc.names)
			}
		})
	}
}

func assertFilterPills(t *testing.T, body, selected, sortBy string, want map[string]string) {
	t.Helper()
	pills := regexp.MustCompile(`<a href="(/ui/packages[^"]*)"([^>]*)>\s*[^<]+<span[^>]*>(\d+)</span>`).FindAllStringSubmatch(body, -1)
	if len(pills) != len(want) {
		t.Fatalf("got %d pills, want %d", len(pills), len(want))
	}
	seen := make(map[string]bool)
	for _, pill := range pills {
		link, err := url.Parse(html.UnescapeString(pill[1]))
		if err != nil {
			t.Fatal(err)
		}
		params := link.Query()
		ecosystem := params.Get("ecosystem")
		if seen[ecosystem] || pill[3] != want[ecosystem] {
			t.Errorf("%q pill count = %s, want %s (duplicate: %v)", ecosystem, pill[3], want[ecosystem], seen[ecosystem])
		}
		seen[ecosystem] = true
		if params.Get("sort") != sortBy || params.Has("page") {
			t.Errorf("pill must preserve sorting and reset pagination: %s", link)
		}
		if active := strings.Contains(pill[2], `aria-current="page"`); active != (selected == ecosystem) {
			t.Errorf("%q active = %v, selected = %q", ecosystem, active, selected)
		}
	}
}

func TestPackagesListFiltersAcrossPages(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()
	for i := range 51 {
		seedFilterPackage(t, ts.db, "npm", fmt.Sprintf("package-%02d", i), true)
	}
	seedFilterPackage(t, ts.db, "cargo", "serde", true)
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ui/packages?ecosystem=npm&sort=name&page=2", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	assertFilterPills(t, body, "npm", "name", map[string]string{"": "52", "npm": "51", "cargo": "1"})
	for _, text := range []string{"51 packages in npm", "Page 2 of 2", `href="/ui/package/npm/package-50"`, `href="?ecosystem=npm&sort=name&page=1"`} {
		if !strings.Contains(html.UnescapeString(body), text) {
			t.Errorf("missing %q", text)
		}
	}
	if strings.Contains(body, `href="/ui/package/npm/package-00"`) {
		t.Error("page 2 includes package from page 1")
	}
}

func TestPackagesListFiltersEmptyCache(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()
	seedFilterPackage(t, ts.db, "npm", "metadata-only")
	w := httptest.NewRecorder()
	ts.handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ui/packages", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	assertFilterPills(t, w.Body.String(), "", defaultSortBy, map[string]string{"": "0"})
	if !strings.Contains(w.Body.String(), "No cached packages found") {
		t.Error("missing empty cache message")
	}
}
