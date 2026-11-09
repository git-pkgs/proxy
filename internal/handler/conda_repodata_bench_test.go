package handler

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

// repodataBenchBody reads the repodata.json named by CONDA_REPODATA_BENCH,
// such as conda-forge's noarch one, and skips the benchmark without it.
func repodataBenchBody(b *testing.B) []byte {
	b.Helper()
	path := os.Getenv("CONDA_REPODATA_BENCH")
	if path == "" {
		b.Skip("CONDA_REPODATA_BENCH names no repodata.json")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		b.Fatal(err)
	}
	return body
}

func BenchmarkCondaFilterRepodata(b *testing.B) {
	body := repodataBenchBody(b)
	policy, err := cooldown.NewPolicy(&cooldown.Config{Default: "3d"}, nil)
	if err != nil {
		b.Fatal(err)
	}
	proxy := testProxy()
	proxy.Logger = slog.New(slog.DiscardHandler)
	proxy.Cooldown = policy
	h := NewCondaHandler(proxy, "http://proxy.example")
	now := time.Now()
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if err := h.filterRepodataWith(bytes.NewReader(body), io.Discard, h.newRepodataFilter(now)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCondaReadRepodataIndex(b *testing.B) {
	body := repodataBenchBody(b)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		index, err := readRepodataIndex(bytes.NewReader(body))
		if err != nil || len(index) == 0 {
			b.Fatalf("index of %d files, %v", len(index), err)
		}
	}
}
