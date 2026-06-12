package scanner

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

type fakeScanner struct {
	name     string
	findings []Finding
	err      error
	delay    time.Duration
	calls    atomic.Int32
}

func (s *fakeScanner) Name() string { return s.name }
func (s *fakeScanner) Scan(ctx context.Context, _ Request) ([]Finding, error) {
	s.calls.Add(1)
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.findings, s.err
}

func newPipeline() *Pipeline {
	return NewPipeline(Policy{
		BlockAtSeverity: SeverityCritical,
		WarnAtSeverity:  SeverityHigh,
	}, slog.Default())
}

func TestPipelineEmptyAllows(t *testing.T) {
	p := newPipeline()
	if !p.Empty() {
		t.Fatal("expected empty pipeline")
	}
	d := p.Scan(context.Background(), Request{})
	if d.Action != ActionAllow {
		t.Fatalf("expected Allow, got %v", d.Action)
	}
}

func TestPipelineNilEmpty(t *testing.T) {
	var p *Pipeline
	if !p.Empty() {
		t.Fatal("nil pipeline should report empty")
	}
}

func TestPipelineAggregates(t *testing.T) {
	a := &fakeScanner{name: "a", findings: []Finding{{ID: "x", Severity: SeverityHigh}}}
	b := &fakeScanner{name: "b", findings: []Finding{{ID: "y", Severity: SeverityCritical}}}
	p := newPipeline()
	p.Register(a, FailOpen, 0)
	p.Register(b, FailOpen, 0)

	d := p.Scan(context.Background(), Request{})
	if d.Action != ActionBlock {
		t.Fatalf("expected Block, got %v", d.Action)
	}
	if d.Highest != SeverityCritical {
		t.Fatalf("expected Critical, got %v", d.Highest)
	}
	if len(d.Findings) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(d.Findings))
	}
}

func TestPipelineFailOpen(t *testing.T) {
	a := &fakeScanner{name: "broken", err: errors.New("boom")}
	p := newPipeline()
	p.Register(a, FailOpen, 0)

	d := p.Scan(context.Background(), Request{})
	if d.Action != ActionAllow {
		t.Fatalf("FailOpen with no other findings should Allow, got %v", d.Action)
	}
	if len(d.Findings) != 0 {
		t.Fatalf("expected 0 findings, got %d", len(d.Findings))
	}
}

func TestPipelineFailClosed(t *testing.T) {
	a := &fakeScanner{name: "broken", err: errors.New("boom")}
	p := newPipeline()
	p.Register(a, FailClosed, 0)

	d := p.Scan(context.Background(), Request{})
	if d.Action != ActionBlock {
		t.Fatalf("FailClosed should block on error, got %v", d.Action)
	}
	if len(d.Findings) != 1 || d.Findings[0].ID != scannerErrorFindingID {
		t.Fatalf("expected synthetic scanner-error finding, got %+v", d.Findings)
	}
}

func TestPipelineRespectsTimeout(t *testing.T) {
	a := &fakeScanner{name: "slow", delay: 200 * time.Millisecond}
	p := newPipeline()
	p.Register(a, FailOpen, 10*time.Millisecond)

	start := time.Now()
	d := p.Scan(context.Background(), Request{})
	elapsed := time.Since(start)
	if elapsed > 150*time.Millisecond {
		t.Fatalf("timeout not respected, elapsed=%v", elapsed)
	}
	// FailOpen with deadline-exceeded means no findings, decision Allow.
	if d.Action != ActionAllow {
		t.Fatalf("expected Allow on timeout under FailOpen, got %v", d.Action)
	}
}
