// Package oci provides a synchronous scanner gate for OCI image
// manifests.
//
// The container handler invokes the gate before serving a manifest
// response. The gate resolves the manifest digest, asks Trivy (in image
// mode, via `trivy image --server <url>`) to scan the upstream image
// directly, applies the configured policy, and persists the verdict in
// artifact_scans keyed by digest.
//
// Per-blob scanning via the generic scanner.Pipeline is the wrong
// abstraction for OCI: Trivy can only correlate vulnerabilities across
// layers when it sees the assembled image (manifest + config + all
// layers + dpkg/rpm status DB). The gate sidesteps the byte-level hook
// and points Trivy at the upstream registry instead, so the OS-level
// CVEs that the per-blob mode misses are caught.
//
// The gate caches verdicts by manifest digest in artifact_scans with
// scanner name "trivy-image". Within `TTL`, a previously-blocked digest
// is rejected without invoking Trivy; an allowed digest passes through.
package oci

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/scanner"
	"github.com/git-pkgs/proxy/internal/scanner/trivy"
)

// ScannerName is the scanner identifier persisted in artifact_scans.
// Distinct from the fs-mode "trivy" name so image-mode and fs-mode
// verdicts do not collide on the same digest by accident.
const ScannerName = "trivy-image"

// Verdict status values stored in artifact_scans.status.
const (
	statusAllow = "ok"
	statusBlock = "block"
	statusError = "error"
)

// Gate is the synchronous OCI manifest gate.
type Gate struct {
	// Scanner is the Trivy adapter. Required.
	Scanner *trivy.Scanner

	// Policy decides allow/warn/block from the highest severity found.
	Policy scanner.Policy

	// DB persists verdicts in artifact_scans. Optional; when nil the
	// gate still runs but does not cache.
	DB *database.DB

	// Logger is used for structured warn/block logs.
	Logger *slog.Logger

	// Timeout caps the duration of a single trivy image invocation.
	// Zero disables the timeout.
	Timeout time.Duration

	// TTL is how long a cached verdict remains valid. Zero disables
	// expiry (cached verdicts never re-scan).
	TTL time.Duration

	// FailClosed makes scanner errors a block (synthesizes a critical
	// finding). Default is fail-open: errors are logged and the image
	// is allowed.
	FailClosed bool
}

// Decision is the gate's verdict for a single image digest.
type Decision struct {
	Action          scanner.Action
	HighestSeverity scanner.Severity
	FindingCount    int
	// Findings is populated when the gate ran Trivy; empty on cache
	// hits (the gate stores only the verdict, not individual findings).
	Findings  []scanner.Finding
	FromCache bool
}

// Check returns the verdict for an image. imageRef is the reference
// passed to `trivy image` (e.g. "docker.io/library/debian:10").
// digest is the manifest content digest (e.g. "sha256:...") used as
// the cache key — supplying it via Docker-Content-Digest from the
// upstream HEAD/GET avoids re-resolving inside the gate.
//
// When digest is empty the gate runs Trivy unconditionally and skips
// caching.
func (g *Gate) Check(ctx context.Context, imageRef, digest string) (*Decision, error) {
	if g == nil || g.Scanner == nil {
		return &Decision{Action: scanner.ActionAllow}, nil
	}
	if imageRef == "" {
		return nil, fmt.Errorf("oci gate: imageRef required")
	}

	if cached, ok := g.lookupCached(digest); ok {
		return cached, nil
	}

	scanCtx := ctx
	if g.Timeout > 0 {
		var cancel context.CancelFunc
		scanCtx, cancel = context.WithTimeout(ctx, g.Timeout)
		defer cancel()
	}

	findings, err := g.Scanner.ScanImage(scanCtx, imageRef)
	if err != nil {
		g.logger().Warn("oci scanner error",
			"image", imageRef,
			"digest", digest,
			"error", err,
		)
		if g.FailClosed {
			findings = []scanner.Finding{{
				Scanner:  trivy.Name,
				ID:       "scanner-error",
				Severity: scanner.SeverityCritical,
				Summary:  fmt.Sprintf("trivy image failed: %v", err),
			}}
		} else {
			g.recordVerdict(digest, statusError, err.Error())
			return &Decision{Action: scanner.ActionAllow}, nil
		}
	}

	action, highest := g.Policy.Evaluate(findings)
	d := &Decision{
		Action:          action,
		HighestSeverity: highest,
		FindingCount:    len(findings),
		Findings:        findings,
	}

	status := statusAllow
	if action == scanner.ActionBlock {
		status = statusBlock
	}
	g.recordVerdict(digest, status, highest.String())

	switch action {
	case scanner.ActionBlock:
		g.logger().Warn("oci image blocked",
			"image", imageRef,
			"digest", digest,
			"severity", highest.String(),
			"findings", len(findings),
		)
	case scanner.ActionWarn:
		g.logger().Warn("oci image flagged",
			"image", imageRef,
			"digest", digest,
			"severity", highest.String(),
			"findings", len(findings),
		)
	}
	return d, nil
}

func (g *Gate) lookupCached(digest string) (*Decision, bool) {
	if g.DB == nil || digest == "" {
		return nil, false
	}
	row, err := g.DB.GetArtifactScan(digest, ScannerName)
	if err != nil || row == nil {
		return nil, false
	}
	if g.TTL > 0 && time.Since(row.ScannedAt) > g.TTL {
		return nil, false
	}
	d := &Decision{FromCache: true}
	switch row.Status {
	case statusBlock:
		d.Action = scanner.ActionBlock
	case statusAllow:
		d.Action = scanner.ActionAllow
	default:
		// Treat error/unknown status as a cache miss so we re-scan.
		return nil, false
	}
	if row.Error.Valid {
		d.HighestSeverity = scanner.ParseSeverity(row.Error.String)
	}
	return d, true
}

func (g *Gate) recordVerdict(digest, status, detail string) {
	if g.DB == nil || digest == "" {
		return
	}
	rec := &database.ArtifactScan{
		ContentHash: digest,
		Scanner:     ScannerName,
		Status:      status,
		ScannedAt:   time.Now(),
	}
	if detail != "" {
		rec.Error = sql.NullString{String: detail, Valid: true}
	}
	if err := g.DB.UpsertArtifactScan(rec); err != nil {
		g.logger().Warn("oci gate cache write failed",
			"digest", digest,
			"error", err,
		)
	}
}

func (g *Gate) logger() *slog.Logger {
	if g.Logger != nil {
		return g.Logger
	}
	return slog.Default()
}

// ErrBlocked is returned by callers that need to surface the gate's
// block verdict as an error to existing WriteArtifactError plumbing.
// The container handler wraps this with the digest and severity in the
// response body.
var ErrBlocked = errors.New("oci image blocked by scanner")
