package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/git-pkgs/proxy/internal/scanner"
	"github.com/git-pkgs/registries/fetch"
)

type stubScanner struct {
	name     string
	findings []scanner.Finding
	err      error
	calls    atomic.Int32
}

func (s *stubScanner) Name() string { return s.name }
func (s *stubScanner) Scan(_ context.Context, _ scanner.Request) ([]scanner.Finding, error) {
	s.calls.Add(1)
	return s.findings, s.err
}

func newScannerPipeline(t *testing.T, stub *stubScanner, policy scanner.Policy) *scanner.Pipeline {
	t.Helper()
	p := scanner.NewPipeline(policy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.Register(stub, scanner.FailOpen, 0)
	return p
}

// TestAfterStore_BlockDeletesAndReturnsError exercises the block path on
// fetchAndCacheFromURL: a critical finding must remove the file from storage
// and return ErrArtifactQuarantined wrapped in a QuarantineError.
func TestAfterStore_BlockDeletesAndReturnsError(t *testing.T) {
	proxy, _, store, fetcher := setupTestProxy(t)

	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("vulnerable")),
		ContentType: "application/octet-stream",
	}

	stub := &stubScanner{
		name:     "stub",
		findings: []scanner.Finding{{ID: "VULN-1", Severity: scanner.SeverityCritical, Summary: "owned"}},
	}
	proxy.Scanners = newScannerPipeline(t, stub, scanner.Policy{
		BlockAtSeverity: scanner.SeverityCritical,
		WarnAtSeverity:  scanner.SeverityHigh,
	})

	_, err := proxy.GetOrFetchArtifactFromURL(context.Background(),
		"npm", "evilpkg", "1.0.0", "evilpkg-1.0.0.tgz", "https://example.test/x.tgz")
	if err == nil {
		t.Fatal("expected quarantine error, got nil")
	}
	if !errors.Is(err, scanner.ErrArtifactQuarantined) {
		t.Fatalf("expected ErrArtifactQuarantined, got %v", err)
	}
	var qe *scanner.QuarantineError
	if !errors.As(err, &qe) {
		t.Fatal("expected QuarantineError in chain")
	}
	if qe.Highest != scanner.SeverityCritical {
		t.Errorf("highest = %v, want Critical", qe.Highest)
	}

	if len(store.files) != 0 {
		t.Errorf("storage should be empty after block, has %d files", len(store.files))
	}
	if stub.calls.Load() != 1 {
		t.Errorf("expected scanner to be called once, got %d", stub.calls.Load())
	}
}

// TestAfterStore_WarnPersistsArtifact: warn action stores the artifact and
// does not surface an error.
func TestAfterStore_WarnPersistsArtifact(t *testing.T) {
	proxy, _, store, fetcher := setupTestProxy(t)

	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("noisy")),
		ContentType: "application/octet-stream",
	}

	stub := &stubScanner{
		name:     "stub",
		findings: []scanner.Finding{{ID: "ADV-1", Severity: scanner.SeverityHigh}},
	}
	proxy.Scanners = newScannerPipeline(t, stub, scanner.Policy{
		BlockAtSeverity: scanner.SeverityCritical,
		WarnAtSeverity:  scanner.SeverityHigh,
	})

	result, err := proxy.GetOrFetchArtifactFromURL(context.Background(),
		"npm", "warnpkg", "1.0.0", "warnpkg-1.0.0.tgz", "https://example.test/x.tgz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = result.Reader.Close() }()

	if len(store.files) != 1 {
		t.Errorf("expected artifact to remain in storage, got %d files", len(store.files))
	}
}

// TestAfterStore_CacheHitSkipsScanner: when the artifact is already cached,
// fetchAndCache* is not entered, so the scanner must not be invoked.
func TestAfterStore_CacheHitSkipsScanner(t *testing.T) {
	proxy, db, store, fetcher := setupTestProxy(t)
	seedPackage(t, db, store, "npm", "lodash", "4.17.21", "lodash-4.17.21.tgz", "cached")

	stub := &stubScanner{name: "stub"}
	proxy.Scanners = newScannerPipeline(t, stub, scanner.Policy{
		BlockAtSeverity: scanner.SeverityCritical,
		WarnAtSeverity:  scanner.SeverityHigh,
	})

	result, err := proxy.GetOrFetchArtifact(context.Background(), "npm", "lodash", "4.17.21", "lodash-4.17.21.tgz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = result.Reader.Close() }()

	if !result.Cached {
		t.Error("expected cache hit")
	}
	if fetcher.fetchCalled {
		t.Error("fetcher should not be called on cache hit")
	}
	if stub.calls.Load() != 0 {
		t.Errorf("scanner must not be called on cache hit, got %d calls", stub.calls.Load())
	}
}

// TestAfterStore_MirrorClampToWarn: when ScannerMirrorClampToWarn is set,
// a block-worthy decision becomes a no-op error-wise and the artifact stays.
func TestAfterStore_MirrorClampToWarn(t *testing.T) {
	proxy, _, store, fetcher := setupTestProxy(t)
	proxy.ScannerMirrorClampToWarn = true

	fetcher.artifact = &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader("vulnerable")),
		ContentType: "application/octet-stream",
	}

	stub := &stubScanner{
		name:     "stub",
		findings: []scanner.Finding{{ID: "VULN-1", Severity: scanner.SeverityCritical}},
	}
	proxy.Scanners = newScannerPipeline(t, stub, scanner.Policy{
		BlockAtSeverity: scanner.SeverityCritical,
		WarnAtSeverity:  scanner.SeverityHigh,
	})

	_, err := proxy.GetOrFetchArtifactFromURL(context.Background(),
		"npm", "evilpkg", "1.0.0", "evilpkg-1.0.0.tgz", "https://example.test/x.tgz")
	if err != nil {
		t.Fatalf("expected no error under mirror clamp, got %v", err)
	}
	if len(store.files) != 1 {
		t.Errorf("expected artifact to remain in storage, got %d files", len(store.files))
	}
}

func TestWriteArtifactError_Quarantine(t *testing.T) {
	w := httptest.NewRecorder()
	qe := &scanner.QuarantineError{
		Highest: scanner.SeverityCritical,
		Findings: []scanner.Finding{
			{Scanner: "osv", ID: "CVE-2024-0001", Severity: scanner.SeverityCritical, Summary: "RCE"},
		},
	}
	ok := WriteArtifactError(w, qe)
	if !ok {
		t.Fatal("WriteArtifactError should report it wrote the response")
	}
	if w.Code != 451 {
		t.Fatalf("status = %d, want 451", w.Code)
	}
	if got := w.Header().Get("X-Scanner-Severity"); got != "critical" {
		t.Errorf("X-Scanner-Severity = %q, want critical", got)
	}
	if got := w.Header().Get("X-Scanner-Findings"); got != "1" {
		t.Errorf("X-Scanner-Findings = %q, want 1", got)
	}

	var body struct {
		Error    string `json:"error"`
		Severity string `json:"severity"`
		Findings []struct {
			ID       string `json:"id"`
			Severity string `json:"severity"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.Severity != "critical" || len(body.Findings) != 1 || body.Findings[0].ID != "CVE-2024-0001" {
		t.Errorf("unexpected body: %+v", body)
	}
}

func TestWriteArtifactError_PassThrough(t *testing.T) {
	w := httptest.NewRecorder()
	if WriteArtifactError(w, errors.New("ordinary error")) {
		t.Fatal("WriteArtifactError should not write for unrelated errors")
	}
	if w.Code != 200 {
		t.Errorf("status = %d, want 200 (untouched)", w.Code)
	}
}
