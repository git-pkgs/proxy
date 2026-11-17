package mirror

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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
	ctx := context.Background()

	if _, _, err := m.debRegistry(ctx, srv.URL+"/old", "bookworm", "amd64"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.debRegistry(ctx, srv.URL+"/fresh", "bookworm", "amd64"); err != nil {
		t.Fatal(err)
	}
	old := debIndexKey(srv.URL+"/old", "bookworm", "amd64")
	idx := m.deb.regs[old]
	idx.fetched = time.Now().Add(-debIndexTTL)
	m.deb.regs[old] = idx

	if _, _, err := m.debRegistry(ctx, srv.URL+"/new", "bookworm", "amd64"); err != nil {
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

// The architecture for Architecture: all packages is read from the Release
// once per debIndexTTL.
func TestDebAllArchIsReadOncePerTTL(t *testing.T) {
	var reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/dists/jammy/InRelease" {
			http.NotFound(w, r)
			return
		}
		reads.Add(1)
		_, _ = io.WriteString(w, "Architectures: arm64\n")
	}))
	t.Cleanup(srv.Close)
	m := &Mirror{}
	ctx := context.Background()

	for range 2 {
		if arch, err := m.debAllArch(ctx, srv.URL, "jammy"); err != nil || arch != "arm64" {
			t.Fatalf("debAllArch = %q, %v; want arm64", arch, err)
		}
	}
	if got := reads.Load(); got != 1 {
		t.Errorf("InRelease read %d times, want once", got)
	}
	key := srv.URL + "\x00jammy"
	a := m.deb.allArchs[key]
	a.fetched = time.Now().Add(-debIndexTTL)
	m.deb.allArchs[key] = a
	if _, err := m.debAllArch(ctx, srv.URL, "jammy"); err != nil {
		t.Fatal(err)
	}
	if got := reads.Load(); got != 2 {
		t.Errorf("InRelease read %d times after the TTL, want twice", got)
	}
}
