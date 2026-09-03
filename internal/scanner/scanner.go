// Package scanner provides pluggable pre-cache artifact scanning.
//
// A Scanner inspects an artifact staged in the proxy's own storage before
// it becomes visible to clients, and returns a verdict on whether it may
// be cached. The proxy never uploads artifact bytes to a scanner directly:
// it hands the scanner a short-lived signed URL and the scanner pulls the
// bytes itself. See HTTPScanner for the built-in adapter that implements
// this over a small HTTP/JSON contract, letting trivy, ClamAV, Wiz, or any
// custom service integrate without the proxy needing built-in knowledge of
// any specific tool.
package scanner

import "context"

// Request describes a staged artifact awaiting a scan verdict.
type Request struct {
	Ecosystem   string `json:"ecosystem"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Filename    string `json:"filename"`
	PURL        string `json:"purl"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`

	// FetchURL is a short-lived signed URL the scanner must GET itself to
	// retrieve the exact bytes staged in the proxy's storage.
	FetchURL string `json:"fetch_url"`
}

// Finding describes a single issue reported by a scanner.
type Finding struct {
	Severity    string
	Title       string
	Description string
}

// Result is a scanner's verdict for a Request.
type Result struct {
	Allowed  bool
	Reason   string
	Findings []Finding

	// ScannerName identifies which scanner produced this result. Set by
	// Group, not by individual Scanner implementations.
	ScannerName string

	// InfraError reports whether Allowed: false was forced by a scanner
	// call failing (network error, timeout, bad response) rather than an
	// actual verdict from the scanner. Set by Group. Callers that surface
	// Reason to untrusted clients must not do so when this is true: it may
	// contain raw connection errors (internal hostnames, ports) instead of
	// a verdict meant to be shown outside the proxy.
	InfraError bool
}

// Scanner is the extension point for pluggable pre-cache scanning.
type Scanner interface {
	// Name identifies this scanner in logs and metrics.
	Name() string

	// Scan requests a verdict for req. Implementations must respect ctx
	// cancellation: Group cancels in-flight scans once a blocking verdict
	// has already been decided by another scanner.
	Scan(ctx context.Context, req Request) (Result, error)
}
