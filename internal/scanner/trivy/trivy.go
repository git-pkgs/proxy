// Package trivy adapts the Trivy CLI into the scanner.Scanner interface.
//
// Trivy is invoked as `trivy fs --format json --quiet --scanners vuln`
// against a temp-file copy of the cached artifact, so any storage
// backend (local, S3, GCS) works through the existing OpenContent hook.
// The temp file is removed after the scan completes.
//
// Trivy covers ecosystems that OSV does not index well (OCI image
// blobs, deb/rpm packages, distro filesystems). For ecosystems already
// covered by OSV (npm/cargo/pypi/etc.), running trivy in addition is
// safe but redundant — operators can disable per-ecosystem by leaving
// it out of the providers list.
//
// The `trivy` binary must be installed and on PATH (or referenced via
// the Binary option). Trivy will download its vulnerability database on
// first run; consider running `trivy fs --download-db-only` as a warm-up
// step on the host.
package trivy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/git-pkgs/proxy/internal/scanner"
)

// Name is the scanner identifier persisted in artifact_findings.scanner.
const Name = "trivy"

// Runner abstracts the execution of the trivy CLI. The default
// implementation runs a subprocess; tests inject a fake.
type Runner interface {
	Run(ctx context.Context, args ...string) (stdout []byte, err error)
}

// CLIRunner runs the trivy binary as a subprocess.
type CLIRunner struct {
	// Binary is the trivy executable path. Defaults to "trivy" (resolved via PATH).
	Binary string
}

// Run executes trivy and returns its stdout. Non-zero exit status is
// wrapped with stderr content for diagnostics.
func (r *CLIRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	bin := r.Binary
	if bin == "" {
		bin = "trivy"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		errOut := bytes.TrimSpace(stderr.Bytes())
		if len(errOut) > 0 {
			return stdout.Bytes(), fmt.Errorf("trivy: %w: %s", err, errOut)
		}
		return stdout.Bytes(), fmt.Errorf("trivy: %w", err)
	}
	return stdout.Bytes(), nil
}

// Options configures a Trivy scanner.
type Options struct {
	// Binary is the trivy executable path (default "trivy").
	Binary string
	// ExtraArgs are appended to the trivy command line before the
	// target path. Useful for `--severity HIGH,CRITICAL`, `--skip-db-update`,
	// `--offline-scan`, etc.
	ExtraArgs []string
	// Server is the URL of a `trivy server` instance. When set, the CLI
	// is invoked with `--server <url>`, offloading vulnerability-DB
	// matching to the remote server. The CLI still runs locally and
	// performs artifact extraction. Leave empty for standalone mode.
	Server string
	// Runner overrides the default CLI runner. Used by tests.
	Runner Runner
}

// Scanner runs Trivy against artifact bytes or remote image references.
type Scanner struct {
	runner    Runner
	extraArgs []string
	server    string
}

// New returns a Trivy scanner.
func New(opts Options) *Scanner {
	r := opts.Runner
	if r == nil {
		r = &CLIRunner{Binary: opts.Binary}
	}
	return &Scanner{runner: r, extraArgs: opts.ExtraArgs, server: opts.Server}
}

// Name implements scanner.Scanner.
func (s *Scanner) Name() string { return Name }

// Scan implements scanner.Scanner by writing the artifact bytes to a
// temp file and invoking `trivy fs --format json` against it.
func (s *Scanner) Scan(ctx context.Context, req scanner.Request) ([]scanner.Finding, error) {
	if req.OpenContent == nil {
		return nil, nil
	}
	tmpPath, err := materialize(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("materializing artifact: %w", err)
	}
	defer func() { _ = os.Remove(tmpPath) }()

	args := []string{
		"fs",
		"--format", "json",
		"--quiet",
		"--no-progress",
		"--scanners", "vuln",
	}
	if s.server != "" {
		args = append(args, "--server", s.server)
	}
	args = append(args, s.extraArgs...)
	args = append(args, tmpPath)

	out, err := s.runner.Run(ctx, args...)
	if err != nil {
		return nil, err
	}
	return parseFindings(out)
}

// ScanImage runs `trivy image <ref>` against an OCI image reference (e.g.
// "docker.io/library/debian:10" or "docker.io/library/debian@sha256:...").
// Trivy pulls the manifest and blobs from the upstream registry itself —
// this avoids the per-blob materialization that loses image-level context
// (assembled rootfs, dpkg/rpm status DB).
//
// ScanImage is invoked by the OCI manifest gate; it is not part of the
// scanner.Scanner interface because most scanners do not have a
// corresponding image mode.
func (s *Scanner) ScanImage(ctx context.Context, imageRef string) ([]scanner.Finding, error) {
	if imageRef == "" {
		return nil, fmt.Errorf("trivy: image reference required")
	}
	args := []string{
		"image",
		"--format", "json",
		"--quiet",
		"--no-progress",
		"--scanners", "vuln",
	}
	if s.server != "" {
		args = append(args, "--server", s.server)
	}
	args = append(args, s.extraArgs...)
	args = append(args, imageRef)

	out, err := s.runner.Run(ctx, args...)
	if err != nil {
		return nil, err
	}
	return parseFindings(out)
}

func materialize(ctx context.Context, req scanner.Request) (string, error) {
	rc, err := req.OpenContent(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()

	name := req.Filename
	if name == "" {
		name = "artifact"
	}
	// Strip any directory component to avoid path traversal in the
	// pattern; CreateTemp ignores OS-temp-dir characters but a stray "/"
	// would be confusing.
	name = filepath.Base(name)
	if name == "." || name == "/" || name == "" {
		name = "artifact"
	}

	f, err := os.CreateTemp("", "trivy-*-"+name)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, rc); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// trivyReport is the subset of `trivy fs --format json` output that we
// consume. Trivy emits additional fields (Class, Type, CVSS scores per
// vendor, etc.) that we currently ignore.
type trivyReport struct {
	Results []trivyResult `json:"Results"`
}

type trivyResult struct {
	Target          string               `json:"Target"`
	Vulnerabilities []trivyVulnerability `json:"Vulnerabilities"`
}

type trivyVulnerability struct {
	VulnerabilityID  string   `json:"VulnerabilityID"`
	PkgName          string   `json:"PkgName"`
	InstalledVersion string   `json:"InstalledVersion"`
	FixedVersion     string   `json:"FixedVersion"`
	Severity         string   `json:"Severity"`
	Title            string   `json:"Title"`
	Description      string   `json:"Description"`
	References       []string `json:"References"`
}

func parseFindings(out []byte) ([]scanner.Finding, error) {
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, nil
	}
	var r trivyReport
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, fmt.Errorf("decoding trivy JSON: %w", err)
	}
	var findings []scanner.Finding
	for _, res := range r.Results {
		for _, v := range res.Vulnerabilities {
			summary := v.Title
			if summary == "" {
				summary = v.Description
			}
			findings = append(findings, scanner.Finding{
				Scanner:      Name,
				ID:           v.VulnerabilityID,
				Severity:     scanner.ParseSeverity(v.Severity),
				Summary:      summary,
				FixedVersion: v.FixedVersion,
				References:   v.References,
			})
		}
	}
	return findings, nil
}
