package npm

import (
	"bytes"
	"io"
	"os"
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

// BenchmarkRewriteStream times RewriteStream's second pass, the copy, over
// the 5,000-version packument, or the one NPM_PACKUMENT_BENCH names.
func BenchmarkRewriteStream(b *testing.B) {
	body := npmbench.Packument(5000)
	name := "big"
	if path := os.Getenv("NPM_PACKUMENT_BENCH"); path != "" {
		var err error
		if body, err = os.ReadFile(path); err != nil {
			b.Fatal(err)
		}
		name = os.Getenv("NPM_PACKUMENT_BENCH_NAME")
	}
	scan, err := ScanPackument(bytes.NewReader(body))
	if err != nil {
		b.Fatal(err)
	}
	options := Options{ProxyURL: "http://proxy.example"}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := RewriteStream(name, scan, bytes.NewReader(body), io.Discard, options); err != nil {
			b.Fatal(err)
		}
	}
}
