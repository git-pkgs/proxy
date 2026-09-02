package scanner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/metrics"
)

const (
	modeBlock   = "block"
	modeMonitor = "monitor"

	defaultTimeout = 30 * time.Second
)

type entry struct {
	scanner    Scanner
	mode       string
	ecosystems map[string]struct{} // nil/empty means all ecosystems
}

// Group runs a set of configured Scanners concurrently and turns their
// individual verdicts into a single decision.
type Group struct {
	entries  []entry
	timeout  time.Duration
	failOpen bool
	logger   *slog.Logger
}

// NewGroup builds a Group from cfg. If cfg.Enabled is false, the returned
// Group has no entries and Enabled() reports false, so callers can skip
// the scan path entirely.
func NewGroup(cfg config.ScanningConfig, logger *slog.Logger) (*Group, error) {
	g := &Group{
		timeout:  defaultTimeout,
		failOpen: cfg.FailOpen,
		logger:   logger,
	}
	if !cfg.Enabled {
		return g, nil
	}
	if cfg.SigningKeyExpanded() == "" {
		return nil, fmt.Errorf("scanning.signing_key is required when scanning.enabled is true")
	}
	if d, err := time.ParseDuration(cfg.Timeout); err == nil && d > 0 {
		g.timeout = d
	}

	for _, sc := range cfg.Scanners {
		mode := sc.Mode
		if mode == "" {
			mode = modeBlock
		}
		if mode != modeBlock && mode != modeMonitor {
			return nil, fmt.Errorf("scanner %q: invalid mode %q (must be %q or %q)", sc.Name, mode, modeBlock, modeMonitor)
		}

		var ecosystems map[string]struct{}
		if len(sc.Ecosystems) > 0 {
			ecosystems = make(map[string]struct{}, len(sc.Ecosystems))
			for _, eco := range sc.Ecosystems {
				ecosystems[eco] = struct{}{}
			}
		}

		g.entries = append(g.entries, entry{
			scanner:    NewHTTPScanner(sc.Name, sc.URL, sc.HeadersExpanded(), http.DefaultClient),
			mode:       mode,
			ecosystems: ecosystems,
		})
	}

	return g, nil
}

// Enabled reports whether any scanner is configured.
func (g *Group) Enabled() bool {
	return g != nil && len(g.entries) > 0
}

// Timeout returns the per-scan-call timeout used to bound the signed fetch
// URL's validity.
func (g *Group) Timeout() time.Duration {
	return g.timeout
}

func (g *Group) applicable(ecosystem string) []entry {
	var out []entry
	for _, e := range g.entries {
		if len(e.ecosystems) == 0 {
			out = append(out, e)
			continue
		}
		if _, ok := e.ecosystems[ecosystem]; ok {
			out = append(out, e)
		}
	}
	return out
}

// Scan runs every scanner applicable to req.Ecosystem concurrently, never
// sequentially, and returns a single decision.
//
// The moment any "block" mode scanner reports Allowed: false (or errors,
// unless FailOpen is set), Scan cancels a context shared by every
// goroutine: in-flight calls to the other scanners are aborted rather than
// waited out, since a single block already decides the outcome. Scan still
// waits for all goroutines to observe that cancellation and return before
// it itself returns, so no scan call outlives this method call.
//
// If nothing blocks, Scan waits for every "block" mode scanner to finish
// before reporting Allowed: true — an allow decision can't be finalized
// until all of them have answered. "monitor" mode scanners never gate the
// wait or trigger cancellation: a monitor verdict of Allowed: false is
// logged and folded into Result.Findings, but never blocks.
func (g *Group) Scan(ctx context.Context, req Request) Result {
	entries := g.applicable(req.Ecosystem)
	if len(entries) == 0 {
		return Result{Allowed: true}
	}

	scanCtx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()

	var (
		mu       sync.Mutex
		findings []Finding
		blocked  *Result
	)

	var wg sync.WaitGroup
	for _, e := range entries {
		wg.Add(1)
		go func(e entry) {
			defer wg.Done()
			entryFindings, entryBlock := g.evaluate(scanCtx, req, e)

			mu.Lock()
			findings = append(findings, entryFindings...)
			if entryBlock != nil && blocked == nil {
				blocked = entryBlock
				cancel()
			}
			mu.Unlock()
		}(e)
	}

	wg.Wait()

	if blocked != nil {
		blocked.Findings = findings
		return *blocked
	}

	return Result{Allowed: true, Findings: findings}
}

// evaluate runs a single scanner and reports its findings plus, if this
// scanner's verdict should block the artifact, the Result to block with
// (nil otherwise). It never sets Result.Findings on a returned block
// Result — the caller assembles Findings from every entry once all of them
// have finished.
func (g *Group) evaluate(scanCtx context.Context, req Request, e entry) (findings []Finding, block *Result) {
	start := time.Now()
	res, err := e.scanner.Scan(scanCtx, req)
	duration := time.Since(start)

	if err != nil {
		errType := "error"
		switch {
		case errors.Is(scanCtx.Err(), context.DeadlineExceeded):
			errType = "timeout"
		case errors.Is(scanCtx.Err(), context.Canceled):
			// scanCtx was cancelled because another scanner in the group
			// already decided the verdict, not because this call itself
			// timed out or failed on its own.
			errType = "cancelled"
		}
		metrics.RecordScanError(req.Ecosystem, e.scanner.Name(), errType)

		if errType == "cancelled" {
			// Another scanner already decided the verdict and cancelled
			// scanCtx; this call didn't fail on its own, so don't log it
			// as if it did.
			return nil, nil
		}

		if e.mode == modeMonitor || g.failOpen {
			g.logger.Warn("scanner call failed, treating as allowed",
				"scanner", e.scanner.Name(), "mode", e.mode, "error", err)
			return nil, nil
		}

		g.logger.Warn("scanner call failed, blocking artifact",
			"scanner", e.scanner.Name(), "mode", e.mode, "error", err)
		return nil, &Result{
			Allowed:     false,
			Reason:      fmt.Sprintf("scanner %q failed: %v", e.scanner.Name(), err),
			ScannerName: e.scanner.Name(),
			InfraError:  true,
		}
	}

	metrics.RecordScanResult(req.Ecosystem, e.scanner.Name(), res.Allowed, duration)

	if e.mode == modeMonitor {
		if !res.Allowed {
			g.logger.Warn("monitor scanner flagged artifact",
				"scanner", e.scanner.Name(), "reason", res.Reason)
		}
		return res.Findings, nil
	}

	if !res.Allowed {
		return res.Findings, &Result{Allowed: false, Reason: res.Reason, ScannerName: e.scanner.Name()}
	}
	return res.Findings, nil
}
