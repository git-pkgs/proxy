package mirror

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/storage"
	"github.com/git-pkgs/registries/fetch"
)

// setupTestMirror creates a Mirror with real DB and filesystem storage for integration tests.
func setupTestMirror(t *testing.T, workers int) *Mirror {
	t.Helper()

	dbPath := t.TempDir() + "/test.db"
	db, err := database.Create(dbPath)
	if err != nil {
		t.Fatalf("creating database: %v", err)
	}
	if err := db.MigrateSchema(); err != nil {
		t.Fatalf("migrating schema: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	storeDir := t.TempDir()
	store, err := storage.OpenBucket(context.Background(), "file://"+storeDir)
	if err != nil {
		t.Fatalf("opening storage: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	fetcher := fetch.NewFetcher()
	resolver := fetch.NewResolver()
	proxy := handler.NewProxy(db, store, fetcher, resolver, logger)

	return New(proxy, db, store, logger, workers)
}

const testPackageLodash = "lodash"

type signedURLStorage struct {
	storage.Storage
}

func (signedURLStorage) SignedURL(context.Context, string, time.Duration) (string, error) {
	return "https://storage.example/artifact", nil
}

func TestMirrorRunEmptySource(t *testing.T) {
	m := setupTestMirror(t, 2)

	source := &PURLSource{PURLs: []string{}}
	progress, err := m.Run(context.Background(), source)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if progress.Total != 0 {
		t.Errorf("total = %d, want 0", progress.Total)
	}
	if progress.Phase != "complete" {
		t.Errorf("phase = %q, want %q", progress.Phase, "complete")
	}
}

func TestMirrorRunDryRun(t *testing.T) {
	m := setupTestMirror(t, 1)

	source := &PURLSource{
		PURLs: []string{
			"pkg:npm/lodash@4.17.21",
			"pkg:cargo/serde@1.0.0",
		},
	}

	items, err := m.RunDryRun(context.Background(), source)
	if err != nil {
		t.Fatalf("RunDryRun() error = %v", err)
	}

	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}

	// Dry run should not modify the database
	stats, err := m.db.GetCacheStats()
	if err != nil {
		t.Fatalf("GetCacheStats() error = %v", err)
	}
	if stats.TotalArtifacts != 0 {
		t.Errorf("artifacts = %d, want 0 (dry run should not cache)", stats.TotalArtifacts)
	}
}

func TestMirrorRunCanceled(t *testing.T) {
	m := setupTestMirror(t, 1)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	// Use a source that produces items but they'll all fail due to canceled context
	source := &PURLSource{
		PURLs: []string{"pkg:npm/lodash@4.17.21"},
	}

	progress, err := m.Run(ctx, source)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// With a canceled context, the fetch should fail
	if progress.Failed != 1 {
		t.Errorf("failed = %d, want 1", progress.Failed)
	}
}

func TestMirrorOneDirectServeCacheHit(t *testing.T) {
	m := setupTestMirror(t, 1)
	fetcher := fetch.NewFetcher(
		fetch.WithHTTPClient(&http.Client{Transport: rejectingMirrorTransport{}}), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	m.proxy.Fetcher = fetcher
	m.proxy.DirectServe = true
	m.proxy.Storage = signedURLStorage{Storage: m.storage}

	packagePURL := "pkg:npm/example"
	versionPURL := packagePURL + "@1.0.0"
	if err := m.db.UpsertPackage(&database.Package{
		PURL:      packagePURL,
		Ecosystem: "npm",
		Name:      "example",
	}); err != nil {
		t.Fatalf("UpsertPackage() error = %v", err)
	}
	if err := m.db.UpsertVersion(&database.Version{
		PURL:        versionPURL,
		PackagePURL: packagePURL,
	}); err != nil {
		t.Fatalf("UpsertVersion() error = %v", err)
	}
	const storagePath = "npm/example/1.0.0/example-1.0.0.tgz"
	size, hash, err := m.storage.Store(context.Background(), storagePath, strings.NewReader("tarball bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.db.UpsertArtifact(&database.Artifact{
		VersionPURL: versionPURL,
		Filename:    "example-1.0.0.tgz",
		UpstreamURL: "https://registry.example/artifact",
		StoragePath: sql.NullString{String: storagePath, Valid: true},
		ContentHash: sql.NullString{String: hash, Valid: true},
		Size:        sql.NullInt64{Int64: size, Valid: true},
		FetchedAt:   sql.NullTime{Time: time.Now(), Valid: true},
	}); err != nil {
		t.Fatalf("UpsertArtifact() error = %v", err)
	}

	tracker := newProgressTracker()
	m.mirrorOne(context.Background(), PackageVersion{
		Ecosystem: "npm",
		Name:      "example",
		Version:   "1.0.0",
	}, tracker)

	progress := tracker.snapshot()
	if progress.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", progress.Skipped)
	}
	if progress.Failed != 0 {
		t.Errorf("failed = %d, want 0", progress.Failed)
	}
}

func TestProgressTrackerSnapshot(t *testing.T) {
	pt := newProgressTracker()
	pt.total.Store(10)
	pt.completed.Store(5)
	pt.skipped.Store(3)
	pt.failed.Store(2)
	pt.bytes.Store(1024)
	pt.phase.Store("downloading")
	pt.addError("npm", testPackageLodash, "4.17.21", "fetch failed")

	snap := pt.snapshot()
	if snap.Total != 10 {
		t.Errorf("total = %d, want 10", snap.Total)
	}
	if snap.Completed != 5 {
		t.Errorf("completed = %d, want 5", snap.Completed)
	}
	if snap.Skipped != 3 {
		t.Errorf("skipped = %d, want 3", snap.Skipped)
	}
	if snap.Failed != 2 {
		t.Errorf("failed = %d, want 2", snap.Failed)
	}
	if snap.Bytes != 1024 {
		t.Errorf("bytes = %d, want 1024", snap.Bytes)
	}
	if snap.Phase != "downloading" {
		t.Errorf("phase = %q, want %q", snap.Phase, "downloading")
	}
	if len(snap.Errors) != 1 {
		t.Fatalf("errors = %d, want 1", len(snap.Errors))
	}
	if snap.Errors[0].Name != testPackageLodash {
		t.Errorf("error name = %q, want %q", snap.Errors[0].Name, testPackageLodash)
	}
	if snap.StartedAt.IsZero() {
		t.Error("started_at should not be zero")
	}
}

func TestProgressTrackerConcurrentAccess(t *testing.T) {
	pt := newProgressTracker()
	done := make(chan struct{})

	for range 10 {
		go func() {
			pt.completed.Add(1)
			pt.addError("npm", "test", "1.0.0", "error")
			_ = pt.snapshot()
			done <- struct{}{}
		}()
	}

	timeout := time.After(5 * time.Second)
	for range 10 {
		select {
		case <-done:
		case <-timeout:
			t.Fatal("timed out waiting for goroutines")
		}
	}

	snap := pt.snapshot()
	if snap.Completed != 10 {
		t.Errorf("completed = %d, want 10", snap.Completed)
	}
	if len(snap.Errors) != 10 {
		t.Errorf("errors = %d, want 10", len(snap.Errors))
	}
}

func TestNewMirrorDefaultWorkers(t *testing.T) {
	m := New(nil, nil, nil, slog.Default(), 0)
	if m.workers != 1 {
		t.Errorf("workers = %d, want 1 (minimum)", m.workers)
	}

	m = New(nil, nil, nil, slog.Default(), -5)
	if m.workers != 1 {
		t.Errorf("workers = %d, want 1 (minimum)", m.workers)
	}
}

// recordingFetcher fails every fetch and records the URLs it was asked for, so
// a test can show nothing reached upstream.
type recordingFetcher struct {
	mu   sync.Mutex
	urls []string
}

func (f *recordingFetcher) record(url string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.urls = append(f.urls, url)
	return errors.New("recordingFetcher: no upstream in this test")
}

func (f *recordingFetcher) Fetch(_ context.Context, url string) (*fetch.Artifact, error) {
	return nil, f.record(url)
}

func (f *recordingFetcher) FetchWithHeaders(_ context.Context, url string, _ http.Header) (*fetch.Artifact, error) {
	return nil, f.record(url)
}

func (f *recordingFetcher) Head(_ context.Context, url string) (int64, string, error) {
	return 0, "", f.record(url)
}

func (f *recordingFetcher) fetched() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.urls...)
}

func TestMirrorRunRefusesPackages(t *testing.T) {
	m := setupTestMirror(t, 2)
	fetcher := &recordingFetcher{}
	m.proxy.Fetcher = fetcher
	const reason = "packages cannot be mirrored here"
	m.RefusePackages = reason

	source := &PURLSource{PURLs: []string{"pkg:npm/lodash@4.17.21", "pkg:cargo/serde@1.0.0"}}
	progress, err := m.Run(context.Background(), source)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if progress.Failed != 2 || progress.Completed != 0 || progress.Skipped != 0 {
		t.Errorf("progress = %d failed, %d completed, %d skipped; want 2 failed only",
			progress.Failed, progress.Completed, progress.Skipped)
	}
	if len(progress.Errors) != 2 {
		t.Fatalf("errors = %+v, want one per package", progress.Errors)
	}
	for _, e := range progress.Errors {
		if e.Error != reason {
			t.Errorf("%s/%s error = %q, want %q", e.Ecosystem, e.Name, e.Error, reason)
		}
	}
	if urls := fetcher.fetched(); len(urls) != 0 {
		t.Errorf("refused packages were fetched from upstream: %v", urls)
	}

	stats, err := m.db.GetCacheStats()
	if err != nil {
		t.Fatalf("GetCacheStats: %v", err)
	}
	if stats.TotalArtifacts != 0 {
		t.Errorf("cached artifacts = %d, want 0", stats.TotalArtifacts)
	}
}

// Images keep their own refusal, so a mirror refusing packages still hands
// docker and oci items to Images.
func TestMirrorRefusePackagesLeavesImagesToImages(t *testing.T) {
	m := setupTestMirror(t, 1)
	m.proxy.Fetcher = &recordingFetcher{}
	m.RefusePackages = "packages refused"
	m.Images = UnavailableImages{Reason: "images refused"}

	progress, err := m.Run(context.Background(), &PURLSource{PURLs: []string{"pkg:docker/library/alpine@3.20"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(progress.Errors) != 1 || progress.Errors[0].Error != "images refused" {
		t.Errorf("errors = %+v, want the image refusal", progress.Errors)
	}
}
