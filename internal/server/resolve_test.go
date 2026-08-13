package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/go-chi/chi/v5"
)

func newTestDB(t *testing.T) (*database.DB, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "resolve-test-*")
	if err != nil {
		t.Fatal(err)
	}
	db, err := database.Create(filepath.Join(dir, "test.db"))
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatal(err)
	}
	return db, func() { _ = db.Close(); _ = os.RemoveAll(dir) }
}

func seedPackage(t *testing.T, db *database.DB, ecosystem, name, purl string) {
	t.Helper()
	if err := db.UpsertPackage(&database.Package{
		PURL: purl, Ecosystem: ecosystem, Name: name,
	}); err != nil {
		t.Fatalf("failed to upsert package %s: %v", name, err)
	}
}

func TestResolvePackageName(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	seedPackage(t, db, "npm", "lodash", "pkg:npm/lodash")
	seedPackage(t, db, "composer", "monolog/monolog", "pkg:composer/monolog/monolog")
	seedPackage(t, db, "composer", "symfony/console", "pkg:composer/symfony/console")

	tests := []struct {
		name      string
		ecosystem string
		segments  []string
		wantName  string
		wantRest  []string
	}{
		{
			name: "simple package", ecosystem: "npm",
			segments: []string{"lodash"}, wantName: "lodash", wantRest: nil,
		},
		{
			name: "simple package with version", ecosystem: "npm",
			segments: []string{"lodash", "4.17.21"}, wantName: "lodash", wantRest: []string{"4.17.21"},
		},
		{
			name: "namespaced package", ecosystem: "composer",
			segments: []string{"monolog", "monolog"}, wantName: "monolog/monolog", wantRest: nil,
		},
		{
			name: "namespaced package with version", ecosystem: "composer",
			segments: []string{"symfony", "console", "6.0.0"}, wantName: "symfony/console", wantRest: []string{"6.0.0"},
		},
		{
			name: "namespaced with version and action", ecosystem: "composer",
			segments: []string{"symfony", "console", "6.0.0", "browse"},
			wantName: "symfony/console", wantRest: []string{"6.0.0", "browse"},
		},
		{
			name: "not found", ecosystem: "npm",
			segments: []string{"nonexistent"}, wantName: "", wantRest: []string{"nonexistent"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, rest := resolvePackageName(db, tt.ecosystem, tt.segments)
			if name != tt.wantName {
				t.Errorf("name = %q, want %q", name, tt.wantName)
			}
			if len(rest) != len(tt.wantRest) {
				t.Errorf("rest = %v, want %v", rest, tt.wantRest)
			} else {
				for i := range rest {
					if rest[i] != tt.wantRest[i] {
						t.Errorf("rest[%d] = %q, want %q", i, rest[i], tt.wantRest[i])
					}
				}
			}
		})
	}
}

func TestSplitWildcardPath(t *testing.T) {
	tests := []struct {
		input   string
		encoded bool
		want    []string
	}{
		{"lodash", false, []string{"lodash"}},
		{"lodash/4.17.21", false, []string{"lodash", "4.17.21"}},
		{"monolog/monolog", false, []string{"monolog", "monolog"}},
		{"symfony/console/6.0.0/browse", false, []string{"symfony", "console", "6.0.0", "browse"}},
		{"", false, nil},
		{"/", false, nil},
		// chi routes on the raw path when it differs from the canonical
		// encoding of the decoded path, so segments arrive percent-encoded and
		// must be decoded.
		{
			"nmap/7.91%2Bdfsg1%2Breally7.80%2Bdfsg1-2ubuntu0.1", true,
			[]string{"nmap", "7.91+dfsg1+really7.80+dfsg1-2ubuntu0.1"},
		},
		{"%40babel/core/7.0.0", true, []string{"@babel", "core", "7.0.0"}},
		// An encoded separator stays inside its segment rather than splitting.
		{"vendor%2Fname/1.0.0", true, []string{"vendor/name", "1.0.0"}},
		// Invalid escapes are passed through untouched.
		{"lodash/1.0%zz", true, []string{"lodash", "1.0%zz"}},
		// When chi routed on the already-decoded path, an escape that survived
		// is part of the value: a version whose text is "1.0%2B" reaches here
		// as "1.0%2B" and decoding it again would yield "1.0+".
		{"nmap/1.0%2B", false, []string{"nmap", "1.0%2B"}},
	}

	for _, tt := range tests {
		got := splitWildcardPath(tt.input, tt.encoded)
		if len(got) != len(tt.want) {
			t.Errorf("splitWildcardPath(%q, %v) = %v, want %v", tt.input, tt.encoded, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("splitWildcardPath(%q, %v)[%d] = %q, want %q",
					tt.input, tt.encoded, i, got[i], tt.want[i])
			}
		}
	}
}

func TestValidatePackagePath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"simple", "lodash", false},
		{"with version", "lodash/4.17.21", false},
		{"npm scoped", "@babel/core/7.0.0", false},
		{"composer namespaced", "symfony/console/6.0.0", false},
		{"maven coordinates", "org.apache.commons/commons-lang3/3.12.0", false},
		{"unicode", "café/1.0.0", false},
		{"encoded plus in version", "nmap/7.91%2Bdfsg1-2ubuntu0.1", false},
		{"empty", "", true},
		{"null byte", "lodash\x00/4.17.21", true},
		{"encoded null byte", "lodash/%00", true},
		{"encoded newline", "lodash/1.0%0A", true},
		{"parent segment", "lodash/../4.17.21", true},
		{"encoded parent segment", "lodash/%2E%2E/4.17.21", true},
		// A decoded segment can contain slashes, so traversal can hide inside
		// one segment. Registries interpolate the resolved name straight into
		// an upstream URL, and Go sends dot-segments verbatim.
		{"traversal inside one segment", "pkg%2F..%2F..%2Fadmin", true},
		{"traversal via encoded dots and slash", "pkg%2f%2e%2e%2fadmin", true},
		{"encoded slash alone is allowed", "vendor%2Fname/1.0.0", false},
		{"null byte suffix", "lodash\x00", true},
		{"newline", "lodash\n4.17.21", true},
		{"carriage return", "lodash\r", true},
		{"escape", "lodash\x1b[31m", true},
		{"delete", "lodash\x7f", true},
		{"too long", strings.Repeat("a", maxPackagePathLen+1), true},
		{"at limit", strings.Repeat("a", maxPackagePathLen), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The verdict must not depend on whether chi routed on the raw or
			// on the already-decoded path: an escape that reaches a handler
			// undecoded is decoded by the upstream registry instead, so it is
			// rejected either way.
			for _, encoded := range []bool{false, true} {
				err := validatePackagePath(tt.path, encoded)
				if (err != nil) != tt.wantErr {
					t.Errorf("validatePackagePath(%q, %v) error = %v, wantErr %v",
						tt.path, encoded, err, tt.wantErr)
				}
			}
		})
	}
}

// TestPackagePathSegments drives the real router, which is what decides whether
// the wildcard still carries percent-encoding. Go decodes the request path
// itself unless the escaping is non-canonical, so the same version can arrive
// either way and only one of the two forms may be decoded again.
func TestPackagePathSegments(t *testing.T) {
	tests := []struct {
		name   string
		target string
		want   []string
	}{
		{"plain", "/pkg/npm/lodash/4.17.21", []string{"lodash", "4.17.21"}},
		{"encoded plus", "/pkg/deb/nmap/7.91%2Bdfsg1-2ubuntu0.1", []string{"nmap", "7.91+dfsg1-2ubuntu0.1"}},
		{"decoded plus", "/pkg/deb/nmap/7.91+dfsg1-2ubuntu0.1", []string{"nmap", "7.91+dfsg1-2ubuntu0.1"}},
		// An encoded slash is one segment, not a separator.
		{"encoded slash", "/pkg/composer/vendor%2Fname/1.0.0", []string{"vendor/name", "1.0.0"}},
		{"question mark", "/pkg/npm/example/v1%3Fbuild", []string{"example", "v1?build"}},
		// "1.0%252B" is the escaped form of the version "1.0%2B"; net/url
		// already decoded it once, so it must not be decoded again.
		{"literal percent escape", "/pkg/npm/example/1.0%252B", []string{"example", "1.0%2B"}},
		{"browse suffix", "/pkg/deb/nmap/7.91%2Bdfsg1/browse", []string{"nmap", "7.91+dfsg1", "browse"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			var gotErr error

			router := chi.NewRouter()
			router.Get("/pkg/{ecosystem}/*", func(_ http.ResponseWriter, r *http.Request) {
				got, gotErr = packagePathSegments(r)
			})
			router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", tt.target, nil))

			if gotErr != nil {
				t.Fatalf("packagePathSegments(%q) failed: %v", tt.target, gotErr)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("segments for %q = %v, want %v", tt.target, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("segments for %q [%d] = %q, want %q", tt.target, i, got[i], tt.want[i])
				}
			}
		})
	}
}
