package trivy

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/git-pkgs/proxy/internal/scanner"
)

type fakeRunner struct {
	stdout []byte
	err    error

	gotArgs  []string
	calls    int
	lastPath string
}

func (f *fakeRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	f.calls++
	f.gotArgs = append([]string(nil), args...)
	if len(args) > 0 {
		f.lastPath = args[len(args)-1]
	}
	return f.stdout, f.err
}

func openString(s string) func(context.Context) (io.ReadCloser, error) {
	return func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(s)), nil
	}
}

func TestScannerName(t *testing.T) {
	s := New(Options{})
	if s.Name() != Name {
		t.Errorf("Name() = %q, want %q", s.Name(), Name)
	}
}

func TestScanNoOpenContent(t *testing.T) {
	f := &fakeRunner{}
	s := New(Options{Runner: f})
	findings, err := s.Scan(context.Background(), scanner.Request{Ecosystem: "deb", Name: "foo", Version: "1"})
	if err != nil || findings != nil {
		t.Fatalf("expected (nil, nil), got (%v, %v)", findings, err)
	}
	if f.calls != 0 {
		t.Errorf("runner should not be called without OpenContent")
	}
}

func TestScanInvokesTrivy(t *testing.T) {
	const trivyJSON = `{
		"Results": [
			{
				"Target": "tmp/foo",
				"Vulnerabilities": [
					{
						"VulnerabilityID": "CVE-2024-0001",
						"PkgName":          "libssl",
						"InstalledVersion": "1.1.1",
						"FixedVersion":     "1.1.1u",
						"Severity":         "CRITICAL",
						"Title":            "OpenSSL remote crash",
						"References":       ["https://nvd.nist.gov/vuln/detail/CVE-2024-0001"]
					},
					{
						"VulnerabilityID": "CVE-2024-0002",
						"PkgName":          "zlib",
						"Severity":         "MEDIUM",
						"Description":      "buffer overflow"
					}
				]
			}
		]
	}`
	f := &fakeRunner{stdout: []byte(trivyJSON)}
	s := New(Options{Runner: f, ExtraArgs: []string{"--severity", "HIGH,CRITICAL"}})

	findings, err := s.Scan(context.Background(), scanner.Request{
		Filename:    "package.deb",
		OpenContent: openString("dummy bytes"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(findings))
	}
	if findings[0].Severity != scanner.SeverityCritical {
		t.Errorf("finding[0] severity = %v, want critical", findings[0].Severity)
	}
	if findings[0].ID != "CVE-2024-0001" {
		t.Errorf("finding[0] ID = %q", findings[0].ID)
	}
	if findings[0].Scanner != Name {
		t.Errorf("finding[0] scanner = %q", findings[0].Scanner)
	}
	if findings[0].FixedVersion != "1.1.1u" {
		t.Errorf("finding[0] FixedVersion = %q", findings[0].FixedVersion)
	}
	if findings[1].Severity != scanner.SeverityMedium {
		t.Errorf("finding[1] severity = %v, want medium", findings[1].Severity)
	}
	// Title falls back to Description when Title is empty.
	if findings[1].Summary != "buffer overflow" {
		t.Errorf("finding[1] summary = %q (want description fallback)", findings[1].Summary)
	}

	// Verify trivy was invoked with expected flags.
	want := []string{"fs", "--format", "json", "--quiet", "--no-progress", "--scanners", "vuln", "--severity", "HIGH,CRITICAL"}
	gotPrefix := f.gotArgs[:len(want)]
	for i, a := range want {
		if gotPrefix[i] != a {
			t.Errorf("trivy arg[%d] = %q, want %q", i, gotPrefix[i], a)
		}
	}
	// Final arg is the temp file path.
	if f.lastPath == "" || !strings.Contains(f.lastPath, "trivy-") {
		t.Errorf("expected last arg to be temp path, got %q", f.lastPath)
	}
	// Temp file must be cleaned up after Scan returns.
	if _, err := os.Stat(f.lastPath); !os.IsNotExist(err) {
		t.Errorf("temp file %q not cleaned up", f.lastPath)
	}
}

func TestScanEmptyOutput(t *testing.T) {
	f := &fakeRunner{stdout: []byte("   \n  ")}
	s := New(Options{Runner: f})
	findings, err := s.Scan(context.Background(), scanner.Request{
		OpenContent: openString("x"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if findings != nil {
		t.Errorf("expected nil findings for empty output, got %v", findings)
	}
}

func TestScanRunnerError(t *testing.T) {
	f := &fakeRunner{err: errors.New("trivy: not found")}
	s := New(Options{Runner: f})
	_, err := s.Scan(context.Background(), scanner.Request{
		OpenContent: openString("x"),
	})
	if err == nil {
		t.Fatal("expected error from runner")
	}
}

func TestScanInvalidJSON(t *testing.T) {
	f := &fakeRunner{stdout: []byte("not json {{{")}
	s := New(Options{Runner: f})
	_, err := s.Scan(context.Background(), scanner.Request{
		OpenContent: openString("x"),
	})
	if err == nil {
		t.Fatal("expected decode error")
	}
	if !strings.Contains(err.Error(), "decoding trivy JSON") {
		t.Errorf("expected decode error, got: %v", err)
	}
}

func TestScanPassesServerFlag(t *testing.T) {
	f := &fakeRunner{stdout: []byte(`{"Results":[]}`)}
	s := New(Options{Runner: f, Server: "http://trivy-server:4954"})
	if _, err := s.Scan(context.Background(), scanner.Request{
		Filename:    "package.deb",
		OpenContent: openString("dummy"),
	}); err != nil {
		t.Fatal(err)
	}
	if !containsPair(f.gotArgs, "--server", "http://trivy-server:4954") {
		t.Errorf("expected --server http://trivy-server:4954 in args, got %v", f.gotArgs)
	}
}

func TestScanImageInvokesTrivy(t *testing.T) {
	const trivyJSON = `{
		"Results": [
			{
				"Target": "docker.io/library/debian:10",
				"Vulnerabilities": [
					{
						"VulnerabilityID": "CVE-2024-9999",
						"PkgName":          "libc6",
						"InstalledVersion": "2.28-10",
						"FixedVersion":     "2.28-10+deb10u3",
						"Severity":         "CRITICAL",
						"Title":            "glibc heap overflow"
					}
				]
			}
		]
	}`
	f := &fakeRunner{stdout: []byte(trivyJSON)}
	s := New(Options{Runner: f, Server: "http://trivy:4954"})

	findings, err := s.ScanImage(context.Background(), "docker.io/library/debian:10")
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Severity != scanner.SeverityCritical {
		t.Errorf("severity = %v, want critical", findings[0].Severity)
	}
	want := []string{"image", "--format", "json", "--quiet", "--no-progress", "--scanners", "vuln", "--server", "http://trivy:4954"}
	for i, a := range want {
		if i >= len(f.gotArgs) || f.gotArgs[i] != a {
			t.Errorf("arg[%d] = %q, want %q", i, f.gotArgs[i], a)
		}
	}
	if f.lastPath != "docker.io/library/debian:10" {
		t.Errorf("expected image ref as last arg, got %q", f.lastPath)
	}
}

func TestScanImageRequiresRef(t *testing.T) {
	f := &fakeRunner{}
	s := New(Options{Runner: f})
	if _, err := s.ScanImage(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty ref")
	}
	if f.calls != 0 {
		t.Errorf("runner should not be invoked for empty ref")
	}
}

func containsPair(args []string, a, b string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == a && args[i+1] == b {
			return true
		}
	}
	return false
}

func TestParseFindingsEmptyResults(t *testing.T) {
	findings, err := parseFindings([]byte(`{"Results":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if findings != nil {
		t.Errorf("expected nil for empty Results, got %v", findings)
	}
}
