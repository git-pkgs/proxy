package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/git-pkgs/proxy/internal/database"
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
		input string
		want  []string
	}{
		{"lodash", []string{"lodash"}},
		{"lodash/4.17.21", []string{"lodash", "4.17.21"}},
		{"monolog/monolog", []string{"monolog", "monolog"}},
		{"symfony/console/6.0.0/browse", []string{"symfony", "console", "6.0.0", "browse"}},
		{"", nil},
		{"/", nil},
	}

	for _, tt := range tests {
		got := splitWildcardPath(tt.input)
		if len(got) != len(tt.want) {
			t.Errorf("splitWildcardPath(%q) = %v, want %v", tt.input, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("splitWildcardPath(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
			}
		}
	}
}
