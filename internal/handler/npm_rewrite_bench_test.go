package handler

import (
	"testing"

	"github.com/git-pkgs/proxy/internal/metadata/npm/npmbench"
)

func BenchmarkNPMRewriteMetadata(b *testing.B) {
	body := npmbench.Packument(5000)
	h := &NPMHandler{proxy: testProxy(), proxyURL: "http://proxy.example"}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := h.rewriteMetadataKeeping("big", body); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNPMVersionTarball(b *testing.B) {
	body := npmbench.Packument(5000)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if npmVersionTarball(body, "1.0.2500") == "" {
			b.Fatal("tarball not found")
		}
	}
}
