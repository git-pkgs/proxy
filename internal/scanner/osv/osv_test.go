package osv

import (
	"context"
	"errors"
	"testing"

	"github.com/git-pkgs/proxy/internal/enrichment"
	"github.com/git-pkgs/proxy/internal/scanner"
)

type fakeLookup struct {
	got    [3]string
	vulns  []enrichment.VulnInfo
	err    error
	called bool
}

func (f *fakeLookup) CheckVulnerabilities(_ context.Context, ecosystem, name, version string) ([]enrichment.VulnInfo, error) {
	f.called = true
	f.got = [3]string{ecosystem, name, version}
	return f.vulns, f.err
}

func newScanner(f *fakeLookup) *Scanner {
	return &Scanner{svc: f}
}

func TestScannerName(t *testing.T) {
	s := &Scanner{}
	if s.Name() != Name {
		t.Errorf("Name() = %q, want %q", s.Name(), Name)
	}
}

func TestScanNilService(t *testing.T) {
	s := &Scanner{svc: nil}
	_, err := s.Scan(context.Background(), scanner.Request{Ecosystem: "npm", Name: "x", Version: "1"})
	if err == nil {
		t.Fatal("expected error for nil service")
	}
}

func TestScanIncompleteRequest(t *testing.T) {
	f := &fakeLookup{}
	s := newScanner(f)
	findings, err := s.Scan(context.Background(), scanner.Request{Name: "x", Version: "1"})
	if err != nil || findings != nil {
		t.Fatalf("expected (nil, nil) for incomplete request, got (%v, %v)", findings, err)
	}
	if f.called {
		t.Error("CheckVulnerabilities should not be called when fields are empty")
	}
}

func TestScanSkipsUnsupportedEcosystem(t *testing.T) {
	// OSV doesn't index OCI / deb / rpm / conda / cran / julia / conan /
	// gradle — querying any of them returns 400 Invalid ecosystem. The
	// scanner must treat those as "no data" rather than as a scanner
	// failure, otherwise fail_mode=closed would block every artifact
	// fetched through those protocols.
	cases := []string{"oci", "deb", "rpm", "conda", "cran", "julia", "conan", "gradle", "unknown"}
	for _, eco := range cases {
		f := &fakeLookup{}
		s := newScanner(f)
		findings, err := s.Scan(context.Background(), scanner.Request{
			Ecosystem: eco, Name: "anything", Version: "1.0",
		})
		if err != nil {
			t.Errorf("%s: expected nil error, got %v", eco, err)
		}
		if findings != nil {
			t.Errorf("%s: expected nil findings, got %v", eco, findings)
		}
		if f.called {
			t.Errorf("%s: CheckVulnerabilities should not be called for unsupported ecosystem", eco)
		}
	}
}

func TestScanPropagatesError(t *testing.T) {
	f := &fakeLookup{err: errors.New("boom")}
	s := newScanner(f)
	_, err := s.Scan(context.Background(), scanner.Request{Ecosystem: "npm", Name: "lodash", Version: "1.0.0"})
	if err == nil {
		t.Fatal("expected error to propagate")
	}
}

func TestScanMapsVulnInfo(t *testing.T) {
	f := &fakeLookup{
		vulns: []enrichment.VulnInfo{
			{ID: "GHSA-1", Severity: "HIGH", Summary: "high one", FixedVersion: "1.2.3", References: []string{"https://example.test/1"}},
			{ID: "GHSA-2", Severity: "moderate", Summary: "moderate one"},
			{ID: "GHSA-3", Severity: "", CVSSScore: 9.4, Summary: "cvss fallback"},
			{ID: "GHSA-4", Severity: "", CVSSScore: 0, Summary: "no severity at all"},
		},
	}
	s := newScanner(f)

	findings, err := s.Scan(context.Background(), scanner.Request{
		Ecosystem: "npm",
		Name:      "lodash",
		Version:   "1.0.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !f.called {
		t.Fatal("expected CheckVulnerabilities to be called")
	}
	if f.got != [3]string{"npm", "lodash", "1.0.0"} {
		t.Errorf("unexpected args %v", f.got)
	}
	if len(findings) != 4 {
		t.Fatalf("expected 4 findings, got %d", len(findings))
	}

	want := []scanner.Severity{
		scanner.SeverityHigh,
		scanner.SeverityMedium,
		scanner.SeverityCritical, // CVSS 9.4 falls back to critical
		scanner.SeverityUnknown,
	}
	for i, w := range want {
		if findings[i].Severity != w {
			t.Errorf("finding[%d] severity = %v, want %v", i, findings[i].Severity, w)
		}
		if findings[i].Scanner != Name {
			t.Errorf("finding[%d] scanner = %q, want %q", i, findings[i].Scanner, Name)
		}
	}

	if findings[0].FixedVersion != "1.2.3" {
		t.Errorf("FixedVersion not passed through: %q", findings[0].FixedVersion)
	}
	if len(findings[0].References) != 1 || findings[0].References[0] != "https://example.test/1" {
		t.Errorf("References not passed through: %v", findings[0].References)
	}
}
