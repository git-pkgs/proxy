package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/scanner"
	"github.com/git-pkgs/purl"
	"github.com/git-pkgs/registries/fetch"
)

// newTestScanServer returns an httptest.Server implementing the HTTPScanner
// notify contract, always replying with the given verdict.
func newTestScanServer(t testing.TB, allowed bool, reason string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode scan notify body: %v", err)
		}
		if body["fetch_url"] == "" || body["fetch_url"] == nil {
			t.Error("scan notify body missing fetch_url")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": allowed, "reason": reason})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestScanGroup(t testing.TB, scanURL string, failOpen bool) *scanner.Group {
	t.Helper()
	g, err := scanner.NewGroup(config.ScanningConfig{
		Enabled:    true,
		FailOpen:   failOpen,
		Timeout:    "15s",
		SigningKey: "test-signing-key",
		Scanners: []config.ScannerConfig{
			{Name: "test-scanner", URL: scanURL, Mode: "block"},
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("scanner.NewGroup() error: %v", err)
	}
	return g
}

func TestGetOrFetchArtifact_ScanAllowed(t *testing.T) {
	proxy, db, store, fetcher := setupTestProxy(t)
	proxy.ScanSigningKey = []byte("test-signing-key")
	proxy.Scanners = newTestScanGroup(t, newTestScanServer(t, true, "").URL, false)

	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("clean content")),
		ContentType: "application/gzip",
	}

	result, err := proxy.GetOrFetchArtifact(context.Background(), "npm", "leftpad", "1.0.0", "leftpad-1.0.0.tgz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = result.Reader.Close() }()

	body, _ := io.ReadAll(result.Reader)
	if string(body) != "clean content" {
		t.Errorf("body = %q, want %q", body, "clean content")
	}

	cached, err := db.GetCachedArtifact(
		purl.MakePURLString("npm", "leftpad", ""), purl.MakePURLString("npm", "leftpad", "1.0.0"), "leftpad-1.0.0.tgz")
	if err != nil {
		t.Fatalf("GetCachedArtifact() error: %v", err)
	}
	if cached == nil {
		t.Error("expected allowed artifact to be committed to the cache database")
	}
	if len(store.files) == 0 {
		t.Error("expected allowed artifact bytes to remain in storage")
	}
}

func TestGetOrFetchArtifact_ScanBlocked(t *testing.T) {
	proxy, db, store, fetcher := setupTestProxy(t)
	proxy.ScanSigningKey = []byte("test-signing-key")
	proxy.Scanners = newTestScanGroup(t, newTestScanServer(t, false, "malware detected").URL, false)

	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("evil content")),
		ContentType: "application/gzip",
	}

	_, err := proxy.GetOrFetchArtifact(context.Background(), "npm", "evilpkg", "1.0.0", "evilpkg-1.0.0.tgz")
	if err == nil {
		t.Fatal("expected error for blocked artifact")
	}
	if !errors.Is(err, ErrArtifactBlocked) {
		t.Errorf("error = %v, want wrapped ErrArtifactBlocked", err)
	}
	if !strings.Contains(err.Error(), "malware detected") {
		t.Errorf("error %q does not include scanner reason", err.Error())
	}

	cached, err := db.GetCachedArtifact(
		purl.MakePURLString("npm", "evilpkg", ""), purl.MakePURLString("npm", "evilpkg", "1.0.0"), "evilpkg-1.0.0.tgz")
	if err != nil {
		t.Fatalf("GetCachedArtifact() error: %v", err)
	}
	if cached != nil {
		t.Error("blocked artifact must never be committed to the cache database")
	}
	if len(store.files) != 0 {
		t.Errorf("blocked artifact bytes must be deleted from storage, got %d files", len(store.files))
	}
}

func TestGetOrFetchArtifact_BlockedDeleteSurvivesClientDisconnect(t *testing.T) {
	proxy, db, store, fetcher := setupTestProxy(t)
	proxy.ScanSigningKey = []byte("test-signing-key")

	const scanDelay = 150 * time.Millisecond
	blockingSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(scanDelay)
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": false, "reason": "malware detected"})
	}))
	t.Cleanup(blockingSrv.Close)
	proxy.Scanners = newTestScanGroup(t, blockingSrv.URL, false)

	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("evil content")),
		ContentType: "application/gzip",
	}

	// The client disconnects long before the (genuinely malicious) verdict
	// comes back; cleanup of the blocked bytes must not be skipped just
	// because the client is gone.
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)

	_, err := proxy.GetOrFetchArtifact(ctx, "npm", "evilpkg", "1.0.0", "evilpkg-1.0.0.tgz")
	if err == nil {
		t.Fatal("expected error for blocked artifact")
	}
	if !errors.Is(err, ErrArtifactBlocked) {
		t.Errorf("error = %v, want wrapped ErrArtifactBlocked", err)
	}

	cached, _ := db.GetCachedArtifact(
		purl.MakePURLString("npm", "evilpkg", ""), purl.MakePURLString("npm", "evilpkg", "1.0.0"), "evilpkg-1.0.0.tgz")
	if cached != nil {
		t.Error("blocked artifact must never be committed to the cache database")
	}
	if len(store.files) != 0 {
		t.Errorf("blocked artifact bytes must still be deleted even though the client disconnected mid-scan, got %d orphaned files", len(store.files))
	}
}

func TestGetOrFetchArtifact_ScanErrorFailClosed(t *testing.T) {
	proxy, db, store, fetcher := setupTestProxy(t)
	proxy.ScanSigningKey = []byte("test-signing-key")

	brokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(brokenSrv.Close)
	proxy.Scanners = newTestScanGroup(t, brokenSrv.URL, false)

	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("content")),
		ContentType: "application/gzip",
	}

	_, err := proxy.GetOrFetchArtifact(context.Background(), "npm", "flaky", "1.0.0", "flaky-1.0.0.tgz")
	if err == nil {
		t.Fatal("expected error when scanner infrastructure fails")
	}
	if !errors.Is(err, ErrArtifactBlocked) {
		t.Errorf("error = %v, want wrapped ErrArtifactBlocked (fail-closed default)", err)
	}
	if strings.Contains(err.Error(), brokenSrv.URL) {
		t.Errorf("error %q leaks the internal scanner URL to the client-facing message", err.Error())
	}
	if !strings.Contains(err.Error(), "scan could not be completed") {
		t.Errorf("error %q does not use the generic infra-failure message", err.Error())
	}

	cached, _ := db.GetCachedArtifact(
		purl.MakePURLString("npm", "flaky", ""), purl.MakePURLString("npm", "flaky", "1.0.0"), "flaky-1.0.0.tgz")
	if cached != nil {
		t.Error("artifact must not be committed when scanning fails fail-closed")
	}
	if len(store.files) != 0 {
		t.Errorf("artifact bytes must be deleted on scan infra failure, got %d files", len(store.files))
	}
}

func TestGetOrFetchArtifact_ScanErrorFailOpen(t *testing.T) {
	proxy, db, store, fetcher := setupTestProxy(t)
	proxy.ScanSigningKey = []byte("test-signing-key")

	brokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(brokenSrv.Close)
	proxy.Scanners = newTestScanGroup(t, brokenSrv.URL, true)

	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("content")),
		ContentType: "application/gzip",
	}

	result, err := proxy.GetOrFetchArtifact(context.Background(), "npm", "flaky", "1.0.0", "flaky-1.0.0.tgz")
	if err != nil {
		t.Fatalf("unexpected error: %v (FailOpen must treat scanner infra failure as allowed)", err)
	}
	defer func() { _ = result.Reader.Close() }()

	cached, err := db.GetCachedArtifact(
		purl.MakePURLString("npm", "flaky", ""), purl.MakePURLString("npm", "flaky", "1.0.0"), "flaky-1.0.0.tgz")
	if err != nil {
		t.Fatalf("GetCachedArtifact() error: %v", err)
	}
	if cached == nil {
		t.Error("expected artifact to be committed to the cache when scanning fails fail-open")
	}
	if len(store.files) == 0 {
		t.Error("expected artifact bytes to remain in storage when scanning fails fail-open")
	}
}

func TestGetOrFetchArtifact_ScanSurvivesClientDisconnect(t *testing.T) {
	proxy, db, store, fetcher := setupTestProxy(t)
	proxy.ScanSigningKey = []byte("test-signing-key")

	const scanDelay = 150 * time.Millisecond
	slowSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(scanDelay)
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": true})
	}))
	t.Cleanup(slowSrv.Close)
	proxy.Scanners = newTestScanGroup(t, slowSrv.URL, false)

	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("clean content")),
		ContentType: "application/gzip",
	}

	// Simulate a client that disconnects shortly after issuing the request:
	// its context is cancelled well before the scanner replies, but the
	// scan itself must run to completion rather than being torn down with
	// it.
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)

	start := time.Now()
	result, err := proxy.GetOrFetchArtifact(ctx, "npm", "leftpad", "1.0.0", "leftpad-1.0.0.tgz")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v (a cancelled client context must not be mistaken for a scanner failure)", err)
	}
	defer func() { _ = result.Reader.Close() }()

	if elapsed < scanDelay {
		t.Errorf("GetOrFetchArtifact returned after %v, want it to wait out the full scan (%v) despite client cancellation", elapsed, scanDelay)
	}

	cached, err := db.GetCachedArtifact(
		purl.MakePURLString("npm", "leftpad", ""), purl.MakePURLString("npm", "leftpad", "1.0.0"), "leftpad-1.0.0.tgz")
	if err != nil {
		t.Fatalf("GetCachedArtifact() error: %v", err)
	}
	if cached == nil {
		t.Error("expected artifact to be committed to the cache; a client disconnect must not cause a false block")
	}
	if len(store.files) == 0 {
		t.Error("expected artifact bytes to remain in storage; a client disconnect must not delete a legitimately allowed artifact")
	}
}

func TestGetOrFetchArtifact_ScanDisabledIsNoOp(t *testing.T) {
	proxy, db, _, fetcher := setupTestProxy(t)
	// proxy.Scanners left nil: scanning disabled.

	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("content")),
		ContentType: "application/gzip",
	}

	result, err := proxy.GetOrFetchArtifact(context.Background(), "npm", "plainpkg", "1.0.0", "plainpkg-1.0.0.tgz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = result.Reader.Close() }()

	cached, err := db.GetCachedArtifact(
		purl.MakePURLString("npm", "plainpkg", ""), purl.MakePURLString("npm", "plainpkg", "1.0.0"), "plainpkg-1.0.0.tgz")
	if err != nil {
		t.Fatalf("GetCachedArtifact() error: %v", err)
	}
	if cached == nil {
		t.Error("expected artifact to be cached when scanning is disabled")
	}
}
