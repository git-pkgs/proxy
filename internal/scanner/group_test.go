package scanner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeScanner is a Scanner test double that can simulate a delay, a fixed
// result or error, and report whether its context was cancelled before it
// returned.
type fakeScanner struct {
	name      string
	delay     time.Duration
	result    Result
	err       error
	cancelled *bool
}

func (f *fakeScanner) Name() string { return f.name }

func (f *fakeScanner) Scan(ctx context.Context, _ Request) (Result, error) {
	select {
	case <-time.After(f.delay):
	case <-ctx.Done():
		if f.cancelled != nil {
			*f.cancelled = true
		}
		return Result{}, ctx.Err()
	}
	return f.result, f.err
}

func newTestGroup(entries []entry, failOpen bool) *Group {
	return &Group{
		entries:  entries,
		timeout:  time.Second,
		failOpen: failOpen,
		logger:   discardLogger(),
	}
}

func TestGroup_Enabled(t *testing.T) {
	disabled, err := NewGroup(config.ScanningConfig{Enabled: false}, discardLogger())
	if err != nil {
		t.Fatalf("NewGroup() error: %v", err)
	}
	if disabled.Enabled() {
		t.Error("Enabled() = true for disabled config, want false")
	}

	enabled, err := NewGroup(config.ScanningConfig{
		Enabled:    true,
		Timeout:    "10s",
		SigningKey: "test-signing-key",
		Scanners: []config.ScannerConfig{
			{Name: "clamav", URL: "http://clamav.invalid", Mode: "block"},
		},
	}, discardLogger())
	if err != nil {
		t.Fatalf("NewGroup() error: %v", err)
	}
	if !enabled.Enabled() {
		t.Error("Enabled() = false for configured scanner, want true")
	}
}

func TestGroup_NewGroup_InvalidMode(t *testing.T) {
	_, err := NewGroup(config.ScanningConfig{
		Enabled:    true,
		SigningKey: "test-signing-key",
		Scanners: []config.ScannerConfig{
			{Name: "bad", URL: "http://bad.invalid", Mode: "quarantine"},
		},
	}, discardLogger())
	if err == nil {
		t.Fatal("NewGroup() error = nil, want error for invalid mode")
	}
}

func TestGroup_NewGroup_MissingSigningKey(t *testing.T) {
	_, err := NewGroup(config.ScanningConfig{
		Enabled: true,
		Scanners: []config.ScannerConfig{
			{Name: "clamav", URL: "http://clamav.invalid", Mode: "block"},
		},
	}, discardLogger())
	if err == nil {
		t.Fatal("NewGroup() error = nil, want error for missing signing key")
	}
}

func TestGroup_Scan_NoApplicableScanners(t *testing.T) {
	g := newTestGroup([]entry{
		{
			scanner:    &fakeScanner{name: "npm-only", result: Result{Allowed: false}},
			mode:       modeBlock,
			ecosystems: map[string]struct{}{"npm": {}},
		},
	}, false)

	result := g.Scan(context.Background(), Request{Ecosystem: "pypi"})
	if !result.Allowed {
		t.Error("Allowed = false, want true when no scanner applies to the ecosystem")
	}
}

func TestGroup_Scan_Allowed(t *testing.T) {
	g := newTestGroup([]entry{
		{scanner: &fakeScanner{name: "clamav", result: Result{Allowed: true}}, mode: modeBlock},
	}, false)

	result := g.Scan(context.Background(), Request{Ecosystem: "npm"})
	if !result.Allowed {
		t.Error("Allowed = false, want true")
	}
}

func TestGroup_Scan_Blocked(t *testing.T) {
	g := newTestGroup([]entry{
		{scanner: &fakeScanner{name: "clamav", result: Result{Allowed: false, Reason: "malware"}}, mode: modeBlock},
	}, false)

	result := g.Scan(context.Background(), Request{Ecosystem: "npm"})
	if result.Allowed {
		t.Error("Allowed = true, want false")
	}
	if result.Reason != "malware" {
		t.Errorf("Reason = %q, want %q", result.Reason, "malware")
	}
	if result.ScannerName != "clamav" {
		t.Errorf("ScannerName = %q, want %q", result.ScannerName, "clamav")
	}
	if result.InfraError {
		t.Error("InfraError = true, want false — this is a genuine scanner verdict, not an infrastructure failure")
	}
}

func TestGroup_Scan_MonitorNeverBlocks(t *testing.T) {
	g := newTestGroup([]entry{
		{
			scanner: &fakeScanner{name: "trivy", result: Result{
				Allowed:  false,
				Reason:   "cve found",
				Findings: []Finding{{Severity: "medium", Title: "CVE-1234"}},
			}},
			mode: modeMonitor,
		},
	}, false)

	result := g.Scan(context.Background(), Request{Ecosystem: "npm"})
	if !result.Allowed {
		t.Error("Allowed = false, want true — monitor mode must never block")
	}
	if len(result.Findings) != 1 || result.Findings[0].Title != "CVE-1234" {
		t.Errorf("Findings = %+v, want the monitor scanner's finding folded in", result.Findings)
	}
}

func TestGroup_Scan_FirstBlockCancelsOthers(t *testing.T) {
	var slowSawCancel bool
	g := newTestGroup([]entry{
		{scanner: &fakeScanner{name: "fast-block", result: Result{Allowed: false, Reason: "blocked"}}, mode: modeBlock},
		{scanner: &fakeScanner{name: "slow", delay: 2 * time.Second, cancelled: &slowSawCancel}, mode: modeBlock},
	}, false)

	start := time.Now()
	result := g.Scan(context.Background(), Request{Ecosystem: "npm"})
	elapsed := time.Since(start)

	if result.Allowed {
		t.Error("Allowed = true, want false")
	}
	if elapsed >= 2*time.Second {
		t.Errorf("Scan() took %v, want it to return promptly once the slow scanner's context was cancelled", elapsed)
	}
	if !slowSawCancel {
		t.Error("slow scanner never observed context cancellation")
	}

	if got := testutil.ToFloat64(metrics.ScanErrors.WithLabelValues("npm", "slow", "cancelled")); got != 1 {
		t.Errorf("scan_errors{error_type=cancelled} = %v, want 1 — the slow scanner was aborted by a sibling's block, not by its own timeout", got)
	}
	if got := testutil.ToFloat64(metrics.ScanErrors.WithLabelValues("npm", "slow", "timeout")); got != 0 {
		t.Errorf("scan_errors{error_type=timeout} = %v, want 0 — intra-group cancellation must not be mislabelled as a timeout", got)
	}
}

func TestGroup_Scan_WaitsForAllBlockScannersBeforeAllowing(t *testing.T) {
	g := newTestGroup([]entry{
		{scanner: &fakeScanner{name: "fast", result: Result{Allowed: true}}, mode: modeBlock},
		{scanner: &fakeScanner{name: "slow", delay: 30 * time.Millisecond, result: Result{Allowed: true}}, mode: modeBlock},
	}, false)

	start := time.Now()
	result := g.Scan(context.Background(), Request{Ecosystem: "npm"})
	elapsed := time.Since(start)

	if !result.Allowed {
		t.Error("Allowed = false, want true")
	}
	if elapsed < 30*time.Millisecond {
		t.Errorf("Scan() returned after %v, want it to wait for the slower block scanner", elapsed)
	}
}

func TestGroup_Scan_ErrorFailClosedByDefault(t *testing.T) {
	g := newTestGroup([]entry{
		{scanner: &fakeScanner{name: "flaky", err: errors.New("connection refused")}, mode: modeBlock},
	}, false)

	result := g.Scan(context.Background(), Request{Ecosystem: "npm"})
	if result.Allowed {
		t.Error("Allowed = true, want false — default posture is fail-closed on scanner error")
	}
	if !result.InfraError {
		t.Error("InfraError = false, want true — the block came from a scanner call failure, not a verdict")
	}
	if !strings.Contains(result.Reason, "connection refused") {
		t.Errorf("Reason = %q, want it to include the underlying error for server-side logging", result.Reason)
	}
}

func TestGroup_Scan_ErrorFailOpen(t *testing.T) {
	g := newTestGroup([]entry{
		{scanner: &fakeScanner{name: "flaky", err: errors.New("connection refused")}, mode: modeBlock},
	}, true)

	result := g.Scan(context.Background(), Request{Ecosystem: "npm"})
	if !result.Allowed {
		t.Error("Allowed = false, want true — FailOpen must treat scanner errors as allowed")
	}
}
