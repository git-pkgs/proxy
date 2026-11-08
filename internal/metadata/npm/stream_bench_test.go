package npm

import (
	"bytes"
	"testing"

	"github.com/git-pkgs/proxy/internal/metadata/npm/npmbench"
)

func BenchmarkScanPackument(b *testing.B) {
	body := npmbench.Packument(5000)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ScanPackument(bytes.NewReader(body)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLookupVersion(b *testing.B) {
	body := npmbench.Packument(5000)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		entry, err := LookupVersion(bytes.NewReader(body), "1.0.2500")
		if err != nil || entry.Entry == nil || entry.Published == "" {
			b.Fatalf("LookupVersion = %+v, %v", entry, err)
		}
	}
}
