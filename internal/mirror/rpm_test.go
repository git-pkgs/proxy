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
