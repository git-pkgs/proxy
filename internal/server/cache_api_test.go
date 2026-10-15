package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/go-chi/chi/v5"
)

const (
	cacheTestPackage = "pkg:maven/com.example/lib"
	cacheTestVersion = cacheTestPackage + "@1.0"
)

// seedCacheDeleteVersion caches lib-1.0.jar and its .sha1 for
// com.example:lib 1.0, plus a pushed lib-1.0-local.jar when pushed is set.
func seedCacheDeleteVersion(t *testing.T, db *database.DB, pushed bool) {
	t.Helper()
	if err := db.UpsertPackage(&database.Package{PURL: cacheTestPackage, Ecosystem: "maven", Name: "com.example:lib"}); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}
	if err := db.UpsertVersion(&database.Version{PURL: cacheTestVersion, PackagePURL: cacheTestPackage}); err != nil {
		t.Fatalf("UpsertVersion: %v", err)
	}
	files := map[string]string{"lib-1.0.jar": database.SourceUpstream, "lib-1.0.jar.sha1": database.SourceUpstream}
	if pushed {
		files["lib-1.0-local.jar"] = database.SourceLocal
	}
	for filename, source := range files {
		if err := db.UpsertArtifact(&database.Artifact{
			VersionPURL: cacheTestVersion,
			Filename:    filename,
			UpstreamURL: "https://repo1.maven.org/maven2/com/example/lib/1.0/" + filename,
			StoragePath: sql.NullString{String: "objects/" + filename, Valid: true},
			FetchedAt:   sql.NullTime{Time: time.Now(), Valid: true},
			Source:      source,
		}); err != nil {
			t.Fatalf("UpsertArtifact %s: %v", filename, err)
		}
	}
}

func cacheDelete(ts *testServer, target string) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	r.Delete("/api/cache/{ecosystem}/*", ts.server.handleCacheDelete)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, target, nil))
	return w
}

func cachedFiles(t *testing.T, db *database.DB) []string {
	t.Helper()
	artifacts, err := db.GetArtifactsByVersionPURL(cacheTestVersion)
	if err != nil {
		t.Fatalf("GetArtifactsByVersionPURL: %v", err)
	}
	var cached []string
	for _, art := range artifacts {
		if art.StoragePath.Valid {
			cached = append(cached, art.Filename)
		}
	}
	slices.Sort(cached)
	return cached
}

func queuedDeletes(t *testing.T, db *database.DB) []string {
	t.Helper()
	paths, err := db.GetDuePendingDeletes(time.Now().Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("GetDuePendingDeletes: %v", err)
	}
	slices.Sort(paths)
	return paths
}

func TestCacheDelete_OneFile(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()
	seedCacheDeleteVersion(t, ts.db, false)

	w := cacheDelete(ts, "/api/cache/maven/com.example:lib/1.0?filename=lib-1.0.jar")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp CacheDeleteResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.PURL != cacheTestVersion || !slices.Equal(resp.Deleted, []string{"lib-1.0.jar"}) {
		t.Errorf("response = %+v, want lib-1.0.jar deleted from %s", resp, cacheTestVersion)
	}
	if got := cachedFiles(t, ts.db); !slices.Equal(got, []string{"lib-1.0.jar.sha1"}) {
		t.Errorf("still cached = %v, want only the .sha1", got)
	}
	if got := queuedDeletes(t, ts.db); !slices.Equal(got, []string{"objects/lib-1.0.jar"}) {
		t.Errorf("queued for deletion = %v, want the jar's object", got)
	}
}

func TestCacheDelete_WholeVersionKeepsPushedFiles(t *testing.T) {
	ts := newTestServer(t)
	defer ts.close()
	seedCacheDeleteVersion(t, ts.db, true)

	w := cacheDelete(ts, "/api/cache/maven/com.example:lib/1.0")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if got := cachedFiles(t, ts.db); !slices.Equal(got, []string{"lib-1.0-local.jar"}) {
		t.Errorf("still cached = %v, want only the pushed jar", got)
	}
	if got := queuedDeletes(t, ts.db); !slices.Equal(got, []string{"objects/lib-1.0.jar", "objects/lib-1.0.jar.sha1"}) {
		t.Errorf("queued for deletion = %v, want both upstream objects", got)
	}
}

func TestCacheDelete_Refusals(t *testing.T) {
	tests := []struct {
		name   string
		target string
		want   int
	}{
		{"pushed file", "/api/cache/maven/com.example:lib/1.0?filename=lib-1.0-local.jar", http.StatusConflict},
		{"unknown file", "/api/cache/maven/com.example:lib/1.0?filename=other.jar", http.StatusNotFound},
		{"unknown version", "/api/cache/maven/com.example:lib/2.0", http.StatusNotFound},
		{"unknown package", "/api/cache/maven/com.example:other/1.0", http.StatusNotFound},
		{"no version", "/api/cache/maven/lib", http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := newTestServer(t)
			defer ts.close()
			seedCacheDeleteVersion(t, ts.db, true)

			w := cacheDelete(ts, tt.target)
			if w.Code != tt.want {
				t.Fatalf("status = %d, want %d; body: %s", w.Code, tt.want, w.Body.String())
			}
			if got := cachedFiles(t, ts.db); len(got) != 3 {
				t.Errorf("still cached = %v, want all three files untouched", got)
			}
		})
	}
}

func TestVersionPage_CacheDeleteButtons(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		ts := newTestServer(t)
		seedCacheDeleteVersion(t, ts.db, true)
		ts.server.cfg.CacheDelete = enabled

		w := httptest.NewRecorder()
		ts.handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ui/package/maven/com.example:lib/1.0", nil))
		ts.close()
		if w.Code != http.StatusOK {
			t.Fatalf("enabled=%v: status = %d, want 200", enabled, w.Code)
		}
		body := w.Body.String()
		// One button per upstream file plus the version-wide one; the pushed
		// jar gets none.
		wantButtons := 0
		if enabled {
			wantButtons = 3
		}
		if got := strings.Count(body, `class="cache-delete `); got != wantButtons {
			t.Errorf("enabled=%v: %d delete buttons, want %d", enabled, got, wantButtons)
		}
		if enabled && !strings.Contains(body, `data-cache-url="/ui/api/cache/maven/com.example:lib/1.0"`) {
			t.Errorf("page has no data-cache-url for the version")
		}
	}
}
