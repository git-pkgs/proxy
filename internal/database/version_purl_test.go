package database

import (
	"database/sql"
	"net/url"
	"testing"
	"time"
)

func TestVersionFromPURL(t *testing.T) {
	tests := []struct {
		name string
		purl string
		want string
	}{
		{"simple", "pkg:npm/lodash@4.17.21", "4.17.21"},
		{"namespaced", "pkg:composer/symfony/console@6.0.0", "6.0.0"},
		// Debian/Ubuntu versions routinely contain "+", which PURL encodes.
		{"encoded plus", "pkg:deb/nmap@7.91%2Bdfsg1%2Breally7.80%2Bdfsg1-2ubuntu0.1", "7.91+dfsg1+really7.80+dfsg1-2ubuntu0.1"},
		{"encoded epoch", "pkg:deb/curl@1%3A7.81.0-1", "1:7.81.0-1"},
		{"encoded plus with qualifier", "pkg:deb/nmap@7.91%2Bdfsg1?repository_url=http%3A%2F%2Fexample.com", "7.91+dfsg1"},
		{"tilde is not encoded", "pkg:deb/foo@1.0~rc1", "1.0~rc1"},
		{"no version", "pkg:npm/lodash", ""},
		{"invalid escape passed through", "pkg:npm/lodash@1.0%zz", "1.0%zz"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := VersionFromPURL(tt.purl); got != tt.want {
				t.Errorf("VersionFromPURL(%q) = %q, want %q", tt.purl, got, tt.want)
			}
			v := &Version{PURL: tt.purl}
			if got := v.Version(); got != tt.want {
				t.Errorf("Version.Version() for %q = %q, want %q", tt.purl, got, tt.want)
			}
		})
	}
}

// TestVersionEscapedVersion checks the value the templates put in a URL. It
// must survive the round trip back through the router: escaping here and
// decoding per path segment on the way in has to yield the original version.
func TestVersionEscapedVersion(t *testing.T) {
	tests := []struct {
		name string
		purl string
		want string
	}{
		{"simple", "pkg:npm/lodash@4.17.21", "4.17.21"},
		// "+" is legal in a path segment, so it stays literal and the UI keeps
		// showing the version the way Debian writes it.
		{"plus stays literal", "pkg:deb/nmap@7.91%2Bdfsg1-2ubuntu0.1", "7.91+dfsg1-2ubuntu0.1"},
		// A slash would otherwise split the version into two path segments.
		{"slash", "pkg:golang/example@release%2F1", "release%2F1"},
		// A question mark would otherwise start the query string.
		{"question mark", "pkg:npm/example@v1%3Fbuild", "v1%3Fbuild"},
		// A version containing a literal "%2B" is stored double-encoded; the
		// link must re-encode it or it decodes back to "+" instead.
		{"literal percent escape", "pkg:npm/example@1.0%252B", "1.0%252B"},
		{"space", "pkg:npm/example@1.0%20beta", "1.0%20beta"},
		{"no version", "pkg:npm/lodash", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := &Version{PURL: tt.purl}
			got := v.EscapedVersion()
			if got != tt.want {
				t.Errorf("EscapedVersion() for %q = %q, want %q", tt.purl, got, tt.want)
			}
			// The router decodes each path segment, which must give back the
			// version the page displays.
			decoded, err := url.PathUnescape(got)
			if err != nil {
				t.Fatalf("PathUnescape(%q) failed: %v", got, err)
			}
			if decoded != v.Version() {
				t.Errorf("round trip for %q = %q, want %q", tt.purl, decoded, v.Version())
			}
		})
	}
}

func TestVersionDisplayPURL(t *testing.T) {
	tests := []struct {
		name string
		purl string
		want string
	}{
		{"simple", "pkg:npm/lodash@4.17.21", "pkg:npm/lodash@4.17.21"},
		{
			"encoded plus",
			"pkg:deb/nmap@7.91%2Bdfsg1%2Breally7.80%2Bdfsg1-2ubuntu0.1",
			"pkg:deb/nmap@7.91+dfsg1+really7.80+dfsg1-2ubuntu0.1",
		},
		{
			"qualifier preserved",
			"pkg:deb/nmap@7.91%2Bdfsg1?repository_url=http%3A%2F%2Fexample.com",
			"pkg:deb/nmap@7.91+dfsg1?repository_url=http%3A%2F%2Fexample.com",
		},
		// The namespace is encoded too: MakePURLString("npm", "@babel/core", …)
		// produces "pkg:npm/%40babel/core@…".
		{"encoded npm scope", "pkg:npm/%40babel/core@7.0.0", "pkg:npm/@babel/core@7.0.0"},
		{"encoded scope without version", "pkg:npm/%40babel/core", "pkg:npm/@babel/core"},
		{"no version", "pkg:npm/lodash", "pkg:npm/lodash"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := &Version{PURL: tt.purl}
			if got := v.DisplayPURL(); got != tt.want {
				t.Errorf("DisplayPURL() for %q = %q, want %q", tt.purl, got, tt.want)
			}
		})
	}
}

// TestGetRecentlyCachedPackagesDecodesVersion guards the dashboard's "recently
// cached" list, which derives the version from the version PURL.
func TestGetRecentlyCachedPackagesDecodesVersion(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		const versionPURL = "pkg:deb/nmap@7.91%2Bdfsg1%2Breally7.80%2Bdfsg1-2ubuntu0.1"

		if err := db.UpsertPackage(&Package{
			PURL: "pkg:deb/nmap", Ecosystem: "deb", Name: "nmap",
		}); err != nil {
			t.Fatalf("UpsertPackage failed: %v", err)
		}
		if err := db.UpsertVersion(&Version{
			PURL: versionPURL, PackagePURL: "pkg:deb/nmap",
		}); err != nil {
			t.Fatalf("UpsertVersion failed: %v", err)
		}
		if err := db.UpsertArtifact(&Artifact{
			VersionPURL: versionPURL,
			Filename:    "nmap_7.91+dfsg1+really7.80+dfsg1-2ubuntu0.1_amd64.deb",
			UpstreamURL: "http://archive.ubuntu.com/ubuntu/pool/universe/n/nmap/nmap.deb",
			StoragePath: sql.NullString{String: "/cache/nmap.deb", Valid: true},
			FetchedAt:   sql.NullTime{Time: time.Now(), Valid: true},
		}); err != nil {
			t.Fatalf("UpsertArtifact failed: %v", err)
		}

		recent, err := db.GetRecentlyCachedPackages(10)
		if err != nil {
			t.Fatalf("GetRecentlyCachedPackages failed: %v", err)
		}
		if len(recent) != 1 {
			t.Fatalf("expected 1 recent package, got %d", len(recent))
		}
		const want = "7.91+dfsg1+really7.80+dfsg1-2ubuntu0.1"
		if recent[0].Version != want {
			t.Errorf("Version = %q, want %q", recent[0].Version, want)
		}
		if recent[0].VersionPURL != versionPURL {
			t.Errorf("VersionPURL = %q, want %q", recent[0].VersionPURL, versionPURL)
		}
	})
}
