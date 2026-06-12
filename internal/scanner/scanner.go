// Package scanner provides a pluggable hook for running security scanners
// on artifacts as they are ingested into the cache.
//
// Each Scanner implementation inspects a fetched artifact (by PURL, by
// content hash, or by its bytes via Request.OpenContent) and returns
// findings. The Pipeline aggregates findings from all configured scanners
// and asks a Policy whether the artifact should be allowed, warned about,
// or blocked. Blocked artifacts are removed from storage and the request
// fails with ErrArtifactQuarantined.
//
// The package is designed so additional scanners (Trivy, ClamAV, etc.)
// can be added without changing handler call sites: Request already
// exposes OpenContent for byte-level access.
package scanner

import (
	"context"
	"io"
	"strconv"
	"strings"
)

// Severity is a normalized severity level. Scanners convert their native
// severity values (CVSS scores, CRITICAL/HIGH/MODERATE strings, etc.) into
// one of these constants before returning findings.
type Severity int

const (
	SeverityUnknown Severity = iota
	SeverityLow
	SeverityMedium
	SeverityHigh
	SeverityCritical
)

// String returns the lowercase severity name used when persisting findings.
func (s Severity) String() string {
	switch s {
	case SeverityLow:
		return "low"
	case SeverityMedium:
		return "medium"
	case SeverityHigh:
		return "high"
	case SeverityCritical:
		return "critical"
	default:
		return "unknown"
	}
}

// AtLeast returns true when s is at least as severe as other.
func (s Severity) AtLeast(other Severity) bool {
	return s >= other
}

// ParseSeverity normalizes a string severity into a Severity constant.
// Accepts level names (case-insensitive: "critical", "high", "moderate",
// "medium", "low", "unknown", "none") and numeric strings that are
// interpreted as CVSS v3 base scores using the NVD buckets:
//
//	< 0.1  -> Unknown
//	< 4.0  -> Low
//	< 7.0  -> Medium
//	< 9.0  -> High
//	>= 9.0 -> Critical
//
// Unknown inputs return SeverityUnknown.
func ParseSeverity(s string) Severity {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return SeverityUnknown
	}
	switch strings.ToLower(trimmed) {
	case "critical":
		return SeverityCritical
	case "high":
		return SeverityHigh
	case "moderate", "medium":
		return SeverityMedium
	case "low":
		return SeverityLow
	case "none", "unknown":
		return SeverityUnknown
	}
	if f, err := strconv.ParseFloat(trimmed, 64); err == nil {
		return SeverityFromCVSS(f)
	}
	return SeverityUnknown
}

// SeverityFromCVSS converts a CVSS v3 base score to a Severity using
// the NVD buckets.
func SeverityFromCVSS(score float64) Severity {
	switch {
	case score < 0.1:
		return SeverityUnknown
	case score < 4.0: //nolint:mnd // NVD CVSS v3 bucket boundaries
		return SeverityLow
	case score < 7.0: //nolint:mnd // NVD CVSS v3 bucket boundaries
		return SeverityMedium
	case score < 9.0: //nolint:mnd // NVD CVSS v3 bucket boundaries
		return SeverityHigh
	default:
		return SeverityCritical
	}
}

// Request is the input passed to every scanner for a single artifact.
// OpenContent is populated by the pipeline and returns a reader over the
// artifact bytes in storage; scanners that only need metadata (e.g. PURL
// for advisory lookups) can ignore it. Callers must Close the returned
// reader.
type Request struct {
	Ecosystem   string
	Namespace   string
	Name        string
	Version     string
	PURL        string
	VersionPURL string
	Filename    string
	StoragePath string
	ContentHash string
	ContentType string
	UpstreamURL string
	Size        int64

	// OpenContent returns a reader over the stored artifact bytes.
	// Set by the pipeline; nil if no storage backend is configured.
	OpenContent func(ctx context.Context) (io.ReadCloser, error)
}

// Finding describes a single problem reported by a scanner.
type Finding struct {
	Scanner      string
	ID           string
	Severity     Severity
	Summary      string
	FixedVersion string
	References   []string
	Raw          string
}

// Scanner inspects an artifact and returns a list of findings.
//
// Implementations must be safe to call concurrently. A scanner that
// cannot reach its data source should return an error rather than an
// empty findings list; the pipeline applies the per-scanner fail mode
// to decide whether to treat the error as a critical synthetic finding
// or to log it and continue.
type Scanner interface {
	// Name returns the scanner name (e.g. "osv"). Used in metrics,
	// logs, and the artifact_scans cache key.
	Name() string

	// Scan returns findings for the given request.
	Scan(ctx context.Context, req Request) ([]Finding, error)
}
