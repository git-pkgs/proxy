package scanner

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// FailMode controls how a single scanner's error is interpreted when the
// pipeline aggregates findings.
type FailMode int

const (
	// FailOpen logs the error and treats the scanner as producing zero
	// findings. The pipeline-wide decision can still allow, warn, or
	// block based on other scanners.
	FailOpen FailMode = iota

	// FailClosed converts the error into a synthetic critical finding,
	// causing the pipeline to block. Use for high-assurance environments
	// where a missing scanner result must not be silently allowed.
	FailClosed
)

const (
	defaultScannerTimeout = 30 * time.Second
	scannerErrorFindingID = "scanner-error"
)

// registeredScanner holds a Scanner plus its per-invocation policy.
type registeredScanner struct {
	scanner  Scanner
	failMode FailMode
	timeout  time.Duration
}

// Decision is the result of running the pipeline against a single
// artifact. It carries the policy outcome, the findings observed, and
// the highest severity seen.
type Decision struct {
	Action   Action
	Highest  Severity
	Findings []Finding
}

// Pipeline runs one or more scanners concurrently and applies a policy
// to the aggregated findings.
type Pipeline struct {
	scanners []registeredScanner
	policy   Policy
	logger   *slog.Logger

	// Cache is an optional finding cache. When non-nil the pipeline
	// short-circuits per-scanner work whose result is fresh.
	Cache *Cache
}

// NewPipeline creates an empty pipeline with the given policy.
func NewPipeline(policy Policy, logger *slog.Logger) *Pipeline {
	if logger == nil {
		logger = slog.Default()
	}
	return &Pipeline{policy: policy, logger: logger}
}

// Register adds a scanner to the pipeline. timeout <= 0 falls back to
// defaultScannerTimeout.
func (p *Pipeline) Register(s Scanner, failMode FailMode, timeout time.Duration) {
	if timeout <= 0 {
		timeout = defaultScannerTimeout
	}
	p.scanners = append(p.scanners, registeredScanner{scanner: s, failMode: failMode, timeout: timeout})
}

// Empty reports whether the pipeline has any scanners configured.
// Nil-safe so handlers can write `if p == nil || p.Empty() { skip }`.
func (p *Pipeline) Empty() bool {
	return p == nil || len(p.scanners) == 0
}

// Policy returns the active policy. Useful for tests and for the mirror
// command which may clamp blocking actions to warnings.
func (p *Pipeline) Policy() Policy {
	return p.policy
}

// Scan runs every registered scanner against the request, aggregates
// findings, applies the policy, and returns a Decision. The pipeline
// never returns an error itself; per-scanner errors are folded in via
// the configured FailMode.
func (p *Pipeline) Scan(ctx context.Context, req Request) Decision {
	if p.Empty() {
		return Decision{Action: ActionAllow}
	}

	var (
		mu       sync.Mutex
		findings []Finding
	)

	var wg sync.WaitGroup
	wg.Add(len(p.scanners))
	for _, rs := range p.scanners {
		rs := rs
		go func() {
			defer wg.Done()
			fs := p.runOne(ctx, rs, req)
			if len(fs) == 0 {
				return
			}
			mu.Lock()
			findings = append(findings, fs...)
			mu.Unlock()
		}()
	}
	wg.Wait()

	action, highest := p.policy.Evaluate(findings)
	return Decision{Action: action, Highest: highest, Findings: findings}
}

// runOne runs a single scanner under its timeout and applies caching
// and fail-mode policy.
func (p *Pipeline) runOne(ctx context.Context, rs registeredScanner, req Request) []Finding {
	name := rs.scanner.Name()

	if p.Cache != nil && req.ContentHash != "" {
		if cached, ok := p.Cache.Lookup(name, req.ContentHash); ok {
			return cached
		}
	}

	scanCtx, cancel := context.WithTimeout(ctx, rs.timeout)
	defer cancel()

	start := time.Now()
	fs, err := rs.scanner.Scan(scanCtx, req)
	duration := time.Since(start)

	if err != nil {
		p.logger.Warn("scanner error",
			"scanner", name,
			"purl", req.VersionPURL,
			"fail_mode", failModeName(rs.failMode),
			"duration", duration,
			"error", err)
		if rs.failMode == FailClosed && !errors.Is(err, context.Canceled) {
			return []Finding{{
				Scanner:  name,
				ID:       scannerErrorFindingID,
				Severity: SeverityCritical,
				Summary:  "scanner failed and fail_mode=closed: " + err.Error(),
			}}
		}
		if p.Cache != nil && req.ContentHash != "" {
			p.Cache.Record(name, req.ContentHash, nil, err)
		}
		return nil
	}

	// Annotate findings with the scanner name in case the scanner
	// implementation omitted it.
	for i := range fs {
		if fs[i].Scanner == "" {
			fs[i].Scanner = name
		}
	}

	if p.Cache != nil && req.ContentHash != "" {
		p.Cache.Record(name, req.ContentHash, fs, nil)
	}

	p.logger.Debug("scanner ran",
		"scanner", name,
		"purl", req.VersionPURL,
		"findings", len(fs),
		"duration", duration)
	return fs
}

func failModeName(m FailMode) string {
	if m == FailClosed {
		return "closed"
	}
	return "open"
}
