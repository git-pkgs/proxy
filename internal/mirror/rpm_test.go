package mirror

import (
	"testing"
	"time"
)

// Storing a repository's client drops those past rpmIndexTTL, so indexes no
// longer asked for aren't kept for the Mirror's life.
func TestRPMRegistryDropsExpiredIndexes(t *testing.T) {
	m := &Mirror{}
	for _, repo := range []string{"https://old.example/os", "https://fresh.example/os"} {
		if _, err := m.rpmRegistry(repo, "fedora-40"); err != nil {
			t.Fatal(err)
		}
	}
	idx := m.rpm.regs["https://old.example/os"]
	idx.fetched = time.Now().Add(-rpmIndexTTL)
	m.rpm.regs["https://old.example/os"] = idx

	if _, err := m.rpmRegistry("https://new.example/os", "fedora-40"); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.rpm.regs["https://old.example/os"]; ok {
		t.Error("expired index kept")
	}
	if _, ok := m.rpm.regs["https://fresh.example/os"]; !ok {
		t.Error("index within its TTL dropped")
	}
}

// The release comes from the end of the distro qualifier syft writes from
// os-release, whose ID may contain hyphens.
func TestRPMRelease(t *testing.T) {
	tests := []struct{ distro, release, major string }{
		{"fedora-40", "40", "40"},
		{"rocky-9.4", "9.4", "9"},
		{"opensuse-leap-15.6", "15.6", "15"},
		{"rhel-8.10", "8.10", "8"},
		{"fedora", "", ""},
		{"fedora-", "", ""},
		{"-40", "", ""},
		{"Fedora-40", "", ""},
		{"fedora-40/../x", "", ""},
		{"fedora-40.x", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.distro, func(t *testing.T) {
			release, major, ok := rpmRelease(tt.distro)
			if release != tt.release || major != tt.major || ok != (tt.release != "") {
				t.Errorf("rpmRelease = %q, %q, %v; want %q, %q", release, major, ok, tt.release, tt.major)
			}
		})
	}
}
