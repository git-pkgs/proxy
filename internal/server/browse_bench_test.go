package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"math/rand"
	"testing"
)

func createBenchTarGz(prefix string, fileCount, fileSize int) []byte {
	rnd := rand.New(rand.NewSource(1)) //nolint:gosec
	buf := new(bytes.Buffer)
	gw := gzip.NewWriter(buf)
	tw := tar.NewWriter(gw)

	payload := make([]byte, fileSize)
	for i := range fileCount {
		rnd.Read(payload)
		_ = tw.WriteHeader(&tar.Header{
			Name: fmt.Sprintf("%sfile%04d.dat", prefix, i),
			Size: int64(fileSize),
			Mode: 0644,
		})
		_, _ = tw.Write(payload)
	}
	_ = tw.Close()
	_ = gw.Close()
	return buf.Bytes()
}

func BenchmarkOpenArchive(b *testing.B) {
	cases := []struct {
		name      string
		ecosystem string
		filename  string
		data      []byte
	}{
		{"npm", "npm", "pkg.tgz", createBenchTarGz("package/", 64, 16*1024)},
		{"go", "go", "v1.2.3.tar.gz", createBenchTarGz("repo-abc123/", 64, 16*1024)},
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(tc.data)))
			b.ReportAllocs()
			for b.Loop() {
				r, err := openArchive(tc.filename, bytes.NewReader(tc.data), tc.ecosystem)
				if err != nil {
					b.Fatal(err)
				}
				_ = r.Close()
			}
		})
	}
}
