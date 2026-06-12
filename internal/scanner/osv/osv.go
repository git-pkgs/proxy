// Package osv adapts the existing enrichment-service vulnerability lookup
// into the scanner.Scanner interface. It does not introduce a new data
// source — it reuses the OSV-backed enrichment service already used by
// the API handlers — but it runs at ingest time rather than on demand.
package osv

import (
	"context"
	"fmt"
	"strings"

	"github.com/git-pkgs/proxy/internal/enrichment"
	"github.com/git-pkgs/proxy/internal/scanner"
)

// Name is the scanner identifier persisted in artifact_findings.scanner
// and used as the artifact_scans cache key.
const Name = "osv"

// supportedEcosystems lists the proxy-side ecosystem identifiers that
// the upstream OSV API can answer queries for. Ecosystems outside this
// set (e.g. oci, deb, rpm, conda, cran, julia, conan, gradle) make OSV
// return `400 Invalid ecosystem`, which under fail_mode=closed would
// quarantine every artifact ingested through those protocols. Treat them
// as "scanner has no data" instead of "scanner errored".
var supportedEcosystems = map[string]struct{}{
	"npm":      {},
	"pypi":     {},
	"cargo":    {},
	"gem":      {},
	"maven":    {},
	"golang":   {},
	"go":       {},
	"nuget":    {},
	"composer": {},
	"hex":      {},
	"pub":      {},
}

// IsSupportedEcosystem reports whether OSV can answer queries for the
// given proxy-side ecosystem identifier.
func IsSupportedEcosystem(ecosystem string) bool {
	_, ok := supportedEcosystems[strings.ToLower(ecosystem)]
	return ok
}

// vulnLookup is the subset of enrichment.Service that the OSV scanner
// depends on. Defining it here lets tests inject a fake without
// constructing a full enrichment service.
type vulnLookup interface {
	CheckVulnerabilities(ctx context.Context, ecosystem, name, version string) ([]enrichment.VulnInfo, error)
}

// Scanner wraps enrichment.Service.CheckVulnerabilities so vulnerability
// lookups happen as part of the ingest pipeline.
type Scanner struct {
	svc vulnLookup
}

// New returns an OSV scanner backed by the given enrichment service.
func New(svc *enrichment.Service) *Scanner {
	return &Scanner{svc: svc}
}

// Name implements scanner.Scanner.
func (s *Scanner) Name() string { return Name }

// Scan implements scanner.Scanner by querying OSV for the request's
// (ecosystem, name, version) tuple. Vulnerability severities are
// normalized through scanner.ParseSeverity with a CVSS-score fallback.
func (s *Scanner) Scan(ctx context.Context, req scanner.Request) ([]scanner.Finding, error) {
	if s.svc == nil {
		return nil, fmt.Errorf("osv scanner: enrichment service not configured")
	}
	if req.Ecosystem == "" || req.Name == "" || req.Version == "" {
		return nil, nil
	}
	// OSV rejects queries for ecosystems it doesn't index (oci, deb, rpm,
	// conda, cran, julia, conan, gradle, ...) with HTTP 400. That is not
	// a scanner failure — there is simply nothing to ask — so we report
	// zero findings rather than letting fail_mode=closed quarantine the
	// artifact.
	if !IsSupportedEcosystem(req.Ecosystem) {
		return nil, nil
	}
	vulns, err := s.svc.CheckVulnerabilities(ctx, req.Ecosystem, req.Name, req.Version)
	if err != nil {
		return nil, err
	}
	findings := make([]scanner.Finding, 0, len(vulns))
	for _, v := range vulns {
		sev := scanner.ParseSeverity(v.Severity)
		if sev == scanner.SeverityUnknown && v.CVSSScore > 0 {
			sev = scanner.SeverityFromCVSS(v.CVSSScore)
		}
		findings = append(findings, scanner.Finding{
			Scanner:      Name,
			ID:           v.ID,
			Severity:     sev,
			Summary:      v.Summary,
			FixedVersion: v.FixedVersion,
			References:   v.References,
		})
	}
	return findings, nil
}
