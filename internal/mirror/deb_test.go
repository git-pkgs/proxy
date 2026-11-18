package mirror

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// Loading an archive's indexes drops those past debIndexTTL, so indexes no
// longer asked for aren't kept for the Mirror's life.
func TestDebRegistryDropsExpiredIndexes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	m := &Mirror{}
	rel := debReleaseInfo{suites: []string{"bookworm"}, components: []string{"main"}}

	if _, err := m.debRegistry(srv.URL+"/old", "bookworm", "amd64", rel); err != nil {
		t.Fatal(err)
	}
	if _, err := m.debRegistry(srv.URL+"/fresh", "bookworm", "amd64", rel); err != nil {
		t.Fatal(err)
	}
	old := debIndexKey(srv.URL+"/old", "bookworm", "amd64")
	idx := m.deb.regs[old]
	idx.fetched = time.Now().Add(-debIndexTTL)
	m.deb.regs[old] = idx

	if _, err := m.debRegistry(srv.URL+"/new", "bookworm", "amd64", rel); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.deb.regs[old]; ok {
		t.Error("expired index kept")
	}
	if _, ok := m.deb.regs[debIndexKey(srv.URL+"/fresh", "bookworm", "amd64")]; !ok {
		t.Error("index within its TTL dropped")
	}
}

func TestDebReleaseAllArch(t *testing.T) {
	tests := map[string]string{
		"Codename: jammy\n":                                 "amd64",
		"Architectures: amd64 arm64 i386\n":                 "amd64",
		"Architectures: arm64 amd64\n":                      "amd64",
		"Codename: jammy\nArchitectures: all arm64 armhf\n": "arm64",
		"Architectures: armhf\nComponents: main\n":          "armhf",
		"Architectures: all\n":                              "amd64",
		"Architectures: ../x arm64\n":                       "arm64",
	}
	for release, want := range tests {
		if got := debReleaseAllArch(release); got != want {
			t.Errorf("debReleaseAllArch(%q) = %q, want %q", release, got, want)
		}
	}
}

func TestDebReleaseComponents(t *testing.T) {
	tests := map[string][]string{
		"Codename: bookworm\n": nil,
		"Components: main contrib non-free-firmware non-free\n": {"main", "contrib", "non-free-firmware", "non-free"},
		"Components: stable edge test nightly\n":                {"stable", "edge", "test", "nightly"},
		// debian-security names its components below updates/, and keeps
		// their indexes where the last part of that names them, as apt
		// reads them.
		"Components: updates/main updates/contrib updates/main\n":    {"main", "contrib"},
		"Components: ../x . main%2f\n":                               nil,
		"Codename: bookworm\nComponents: main\nArchitectures: all\n": {"main"},
	}
	for release, want := range tests {
		if got := debReleaseComponents(release); !slices.Equal(got, want) {
			t.Errorf("debReleaseComponents(%q) = %q, want %q", release, got, want)
		}
	}
}

// An archive's Releases are read once per debIndexTTL, for the suites it
// serves, their components and the architecture for Architecture: all
// packages.
func TestDebReleaseIsReadOncePerTTL(t *testing.T) {
	var reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/dists/jammy/InRelease" {
			http.NotFound(w, r)
			return
		}
		reads.Add(1)
		_, _ = io.WriteString(w, "Architectures: arm64\nComponents: stable\n")
	}))
	t.Cleanup(srv.Close)
	m := &Mirror{}
	ctx := context.Background()
	want := debReleaseInfo{suites: []string{"jammy"}, components: []string{"stable"}, allArchs: []string{"arm64"}}

	for range 2 {
		rel, err := m.debReleaseInfo(ctx, srv.URL, "jammy")
		if err != nil || !slices.Equal(rel.suites, want.suites) || !slices.Equal(rel.components, want.components) || !slices.Equal(rel.allArchs, want.allArchs) {
			t.Fatalf("debReleaseInfo = %+v, %v; want %+v", rel, err, want)
		}
	}
	if got := reads.Load(); got != 1 {
		t.Errorf("InRelease read %d times, want once", got)
	}
	key := srv.URL + "\x00jammy"
	rel := m.deb.releases[key]
	rel.fetched = time.Now().Add(-debIndexTTL)
	m.deb.releases[key] = rel
	if _, err := m.debReleaseInfo(ctx, srv.URL, "jammy"); err != nil {
		t.Fatal(err)
	}
	if got := reads.Load(); got != 2 {
		t.Errorf("InRelease read %d times after the TTL, want twice", got)
	}
}

// A suite whose Release lists no components is read for the usual ones,
// and one without amd64 among its architectures for the first it lists.
func TestDebReleaseDefaults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/dists/jammy/Release" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "Codename: jammy\n")
	}))
	t.Cleanup(srv.Close)
	rel, err := (&Mirror{}).debReleaseInfo(context.Background(), srv.URL, "jammy")
	if err != nil || !slices.Equal(rel.components, debComponents) || !slices.Equal(rel.allArchs, []string{"amd64"}) {
		t.Errorf("debReleaseInfo = %+v, %v; want the default components and amd64", rel, err)
	}
}

// The components of all served suites are read, each once, with the usual
// ones for a suite that lists none, and the architectures for Architecture:
// all packages are each suite's: the registries client needs an index in
// every suite.
func TestDebReleaseOfSeveralSuites(t *testing.T) {
	releases := map[string]string{
		"/dists/jammy/InRelease":          "Architectures: arm64 armhf\nComponents: stable\n",
		"/dists/jammy-updates/InRelease":  "Architectures: armhf\nComponents: stable test\n",
		"/dists/jammy-security/InRelease": "Architectures: arm64\n",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := releases[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	rel, err := (&Mirror{}).debReleaseInfo(context.Background(), srv.URL, "jammy")
	if err != nil {
		t.Fatal(err)
	}
	wantComponents := append([]string{"stable", "test"}, debComponents...)
	if !slices.Equal(rel.suites, []string{"jammy", "jammy-updates", "jammy-security"}) || !slices.Equal(rel.components, wantComponents) || !slices.Equal(rel.allArchs, []string{"arm64", "armhf"}) {
		t.Errorf("debReleaseInfo = %+v; want the three suites, components %v, arm64 and armhf", rel, wantComponents)
	}
}

// Reading a release's Releases drops those read past debIndexTTL.
func TestDebReleaseInfoDropsExpiredEntries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	m := &Mirror{}
	ctx := context.Background()
	for _, root := range []string{"/old", "/fresh"} {
		if _, err := m.debReleaseInfo(ctx, srv.URL+root, "bookworm"); err != nil {
			t.Fatal(err)
		}
	}
	old := srv.URL + "/old\x00bookworm"
	rel := m.deb.releases[old]
	rel.fetched = time.Now().Add(-debIndexTTL)
	m.deb.releases[old] = rel

	if _, err := m.debReleaseInfo(ctx, srv.URL+"/new", "bookworm"); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.deb.releases[old]; ok {
		t.Error("expired entry kept")
	}
	if _, ok := m.deb.releases[srv.URL+"/fresh\x00bookworm"]; !ok {
		t.Error("entry within its TTL dropped")
	}
}
