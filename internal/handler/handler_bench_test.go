package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/storage"
	"github.com/git-pkgs/purl"
	"github.com/git-pkgs/registries/fetch"
)

const benchmarkArtifactSize = 64 << 10

const benchmarkMetadataSize = 1 << 20

type benchmarkResponseWriter struct {
	header http.Header
}

func (w *benchmarkResponseWriter) Header() http.Header {
	return w.header
}

func (w *benchmarkResponseWriter) Write(p []byte) (int, error) {
	return len(p), nil
}

func (w *benchmarkResponseWriter) WriteHeader(_ int) {}

func benchmarkCachedProxy(b *testing.B) (*Proxy, *mockStorage) {
	b.Helper()

	proxy, db, store, _ := setupTestProxy(b)
	content := strings.Repeat("x", benchmarkArtifactSize)
	seedPackage(b, db, store, "npm", "lodash", "4.17.21", "lodash-4.17.21.tgz", content)

	artifact, err := db.GetArtifact("pkg:npm/lodash@4.17.21", "lodash-4.17.21.tgz")
	if err != nil {
		b.Fatalf("get seeded artifact: %v", err)
	}
	sum := sha256.Sum256([]byte(content))
	artifact.ContentHash.String = hex.EncodeToString(sum[:])
	if err := db.UpsertArtifact(artifact); err != nil {
		b.Fatalf("update seeded artifact hash: %v", err)
	}

	return proxy, store
}

func BenchmarkArtifactCacheHit(b *testing.B) {
	ctx := context.Background()

	b.Run("stream-64KiB", func(b *testing.B) {
		proxy, _ := benchmarkCachedProxy(b)
		w := &benchmarkResponseWriter{header: make(http.Header)}
		b.SetBytes(benchmarkArtifactSize)
		b.ReportAllocs()
		b.ResetTimer()

		for b.Loop() {
			result, err := proxy.GetOrFetchArtifact(ctx, "npm", "lodash", "4.17.21", "lodash-4.17.21.tgz")
			if err != nil {
				b.Fatal(err)
			}
			ServeArtifact(w, result)
		}
	})

	b.Run("direct-serve", func(b *testing.B) {
		proxy, store := benchmarkCachedProxy(b)
		proxy.DirectServe = true
		store.signedURL = "https://storage.example/npm/lodash-4.17.21.tgz?signature=abc"
		w := &benchmarkResponseWriter{header: make(http.Header)}
		b.ReportAllocs()
		b.ResetTimer()

		for b.Loop() {
			result, err := proxy.GetOrFetchArtifact(ctx, "npm", "lodash", "4.17.21", "lodash-4.17.21.tgz")
			if err != nil {
				b.Fatal(err)
			}
			ServeArtifact(w, result)
		}
	})
}

func BenchmarkArtifactCacheHitParallel(b *testing.B) {
	proxy, _ := benchmarkCachedProxy(b)
	ctx := context.Background()
	b.SetBytes(benchmarkArtifactSize)
	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		w := &benchmarkResponseWriter{header: make(http.Header)}
		for pb.Next() {
			result, err := proxy.GetOrFetchArtifact(ctx, "npm", "lodash", "4.17.21", "lodash-4.17.21.tgz")
			if err != nil {
				b.Error(err)
				return
			}
			ServeArtifact(w, result)
		}
	})
}

func BenchmarkReadMetadata(b *testing.B) {
	payload := bytes.Repeat([]byte("x"), benchmarkMetadataSize)
	proxy := &Proxy{MetadataMaxSize: benchmarkMetadataSize}
	b.SetBytes(benchmarkMetadataSize)
	b.ReportAllocs()

	var data []byte
	for b.Loop() {
		var err error
		data, err = proxy.ReadMetadata(bytes.NewReader(payload))
		if err != nil {
			b.Fatal(err)
		}
	}
	if len(data) != len(payload) {
		b.Fatalf("metadata size = %d, want %d", len(data), len(payload))
	}
}

func BenchmarkArtifactPURLConstruction(b *testing.B) {
	for _, tc := range []struct {
		name      string
		ecosystem string
		packageID string
	}{
		{"npm", "npm", "lodash"},
		{"scoped-npm", "npm", "@scope/package"},
		{"go", "golang", "github.com/git-pkgs/proxy"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			var packagePURL, versionPURL string
			for b.Loop() {
				packagePURL = purl.MakePURLString(tc.ecosystem, tc.packageID, "")
				versionPURL = purl.MakePURLString(tc.ecosystem, tc.packageID, "1.2.3")
			}
			if packagePURL == "" || versionPURL == "" {
				b.Fatal("empty PURL")
			}
		})
	}
}

type benchmarkNPMServer struct {
	client      *http.Client
	requestURL  string
	db          *database.DB
	versionPURL string
	filename    string
}

func newBenchmarkNPMServer(b *testing.B) *benchmarkNPMServer {
	b.Helper()

	ctx := context.Background()
	dir := b.TempDir()
	db, err := database.Create(filepath.Join(dir, "benchmark.db"))
	if err != nil {
		b.Fatalf("create database: %v", err)
	}
	b.Cleanup(func() { _ = db.Close() })

	store, err := storage.OpenBucket(ctx, "file://"+filepath.Join(dir, "cache"))
	if err != nil {
		b.Fatalf("open storage: %v", err)
	}
	b.Cleanup(func() { _ = store.Close() })

	content := bytes.Repeat([]byte("x"), benchmarkArtifactSize)
	storagePath := storage.ArtifactPath("npm", "", "lodash", "4.17.21", "lodash-4.17.21.tgz")
	size, hash, err := store.Store(ctx, storagePath, bytes.NewReader(content))
	if err != nil {
		b.Fatalf("store artifact: %v", err)
	}

	pkg := &database.Package{PURL: "pkg:npm/lodash", Ecosystem: "npm", Name: "lodash"}
	if err := db.UpsertPackage(pkg); err != nil {
		b.Fatalf("seed package: %v", err)
	}
	version := &database.Version{PURL: "pkg:npm/lodash@4.17.21", PackagePURL: pkg.PURL}
	if err := db.UpsertVersion(version); err != nil {
		b.Fatalf("seed version: %v", err)
	}
	artifact := &database.Artifact{
		VersionPURL: version.PURL,
		Filename:    "lodash-4.17.21.tgz",
		UpstreamURL: "https://registry.npmjs.org/lodash/-/lodash-4.17.21.tgz",
		StoragePath: sql.NullString{String: storagePath, Valid: true},
		ContentHash: sql.NullString{String: hash, Valid: true},
		Size:        sql.NullInt64{Int64: size, Valid: true},
		ContentType: sql.NullString{String: "application/gzip", Valid: true},
		FetchedAt:   sql.NullTime{Time: time.Now(), Valid: true},
	}
	if err := db.UpsertArtifact(artifact); err != nil {
		b.Fatalf("seed artifact: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := NewProxy(db, store, &mockFetcher{}, fetch.NewResolver(), logger)
	handler := NewNPMHandler(proxy, "http://proxy.example", "https://registry.npmjs.org")
	server := httptest.NewServer(handler.Routes())
	b.Cleanup(server.Close)
	client := server.Client()
	return &benchmarkNPMServer{
		client:      client,
		requestURL:  server.URL + "/lodash/-/lodash-4.17.21.tgz",
		db:          db,
		versionPURL: version.PURL,
		filename:    artifact.Filename,
	}
}

func (s *benchmarkNPMServer) request() error {
	resp, err := s.client.Get(s.requestURL)
	if err != nil {
		return fmt.Errorf("GET cached artifact: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET cached artifact status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		return fmt.Errorf("read cached artifact: %w", err)
	}
	if n != benchmarkArtifactSize {
		return fmt.Errorf("cached artifact size = %d, want %d", n, benchmarkArtifactSize)
	}
	return nil
}

func (s *benchmarkNPMServer) hitCount(b *testing.B) int64 {
	b.Helper()
	artifact, err := s.db.GetArtifact(s.versionPURL, s.filename)
	if err != nil {
		b.Fatalf("get artifact hit count: %v", err)
	}
	return artifact.HitCount
}

func benchmarkNPMArtifactCacheHitHTTP(b *testing.B, parallel bool) {
	server := newBenchmarkNPMServer(b)
	if err := server.request(); err != nil {
		b.Fatal(err)
	}
	startHits := server.hitCount(b)

	b.SetBytes(benchmarkArtifactSize)
	b.ReportAllocs()
	b.ResetTimer()
	if parallel {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				if err := server.request(); err != nil {
					b.Error(err)
					return
				}
			}
		})
	} else {
		for b.Loop() {
			if err := server.request(); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.StopTimer()

	if hitCount := server.hitCount(b) - startHits; hitCount != int64(b.N) {
		b.Fatalf("new artifact hits = %d, want %d", hitCount, b.N)
	}
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "requests/s")
}

func BenchmarkNPMArtifactCacheHitHTTP(b *testing.B) {
	benchmarkNPMArtifactCacheHitHTTP(b, false)
}

func BenchmarkNPMArtifactCacheHitHTTPParallel(b *testing.B) {
	benchmarkNPMArtifactCacheHitHTTP(b, true)
}
