package oci

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/scanner"
	"github.com/git-pkgs/proxy/internal/scanner/trivy"
)

type fakeRunner struct {
	stdout []byte
	err    error
	calls  int
}

func (f *fakeRunner) Run(_ context.Context, _ ...string) ([]byte, error) {
	f.calls++
	return f.stdout, f.err
}

func newSilentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newGate(t *testing.T, runner *fakeRunner, opts ...func(*Gate)) *Gate {
	t.Helper()
	tr := trivy.New(trivy.Options{Runner: runner})
	g := &Gate{
		Scanner: tr,
		Policy: scanner.Policy{
			BlockAtSeverity: scanner.SeverityCritical,
			WarnAtSeverity:  scanner.SeverityHigh,
		},
		Logger: newSilentLogger(),
	}
	for _, o := range opts {
		o(g)
	}
	return g
}

func TestCheckAllowsCleanImage(t *testing.T) {
	f := &fakeRunner{stdout: []byte(`{"Results":[]}`)}
	g := newGate(t, f)
	d, err := g.Check(context.Background(), "docker.io/library/debian:10", "sha256:abc")
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != scanner.ActionAllow {
		t.Errorf("expected Allow, got %v", d.Action)
	}
	if d.FromCache {
		t.Error("first call must not be a cache hit")
	}
}

func TestCheckBlocksOnCritical(t *testing.T) {
	const out = `{"Results":[{"Vulnerabilities":[{"VulnerabilityID":"CVE-2024-9","Severity":"CRITICAL"}]}]}`
	f := &fakeRunner{stdout: []byte(out)}
	g := newGate(t, f)
	d, err := g.Check(context.Background(), "docker.io/library/debian:10", "sha256:abc")
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != scanner.ActionBlock {
		t.Errorf("expected Block, got %v", d.Action)
	}
	if d.HighestSeverity != scanner.SeverityCritical {
		t.Errorf("expected critical, got %v", d.HighestSeverity)
	}
	if d.FindingCount != 1 {
		t.Errorf("expected 1 finding, got %d", d.FindingCount)
	}
}

func TestCheckCachesVerdict(t *testing.T) {
	db, err := database.Create(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const out = `{"Results":[{"Vulnerabilities":[{"VulnerabilityID":"CVE-2024-9","Severity":"CRITICAL"}]}]}`
	f := &fakeRunner{stdout: []byte(out)}
	g := newGate(t, f, func(g *Gate) {
		g.DB = db
		g.TTL = time.Hour
	})

	if _, err := g.Check(context.Background(), "docker.io/library/debian:10", "sha256:abc"); err != nil {
		t.Fatal(err)
	}
	if f.calls != 1 {
		t.Fatalf("expected 1 trivy call, got %d", f.calls)
	}

	// Second call should hit cache.
	d, err := g.Check(context.Background(), "docker.io/library/debian:10", "sha256:abc")
	if err != nil {
		t.Fatal(err)
	}
	if !d.FromCache {
		t.Error("second call should report FromCache")
	}
	if d.Action != scanner.ActionBlock {
		t.Errorf("cached action = %v, want Block", d.Action)
	}
	if f.calls != 1 {
		t.Errorf("trivy should not be called on cache hit, calls=%d", f.calls)
	}
}

func TestCheckCacheRespectsTTL(t *testing.T) {
	db, err := database.Create(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	f := &fakeRunner{stdout: []byte(`{"Results":[]}`)}
	g := newGate(t, f, func(g *Gate) {
		g.DB = db
		g.TTL = time.Nanosecond
	})
	if _, err := g.Check(context.Background(), "docker.io/library/debian:10", "sha256:expired"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := g.Check(context.Background(), "docker.io/library/debian:10", "sha256:expired"); err != nil {
		t.Fatal(err)
	}
	if f.calls != 2 {
		t.Errorf("expected re-scan after TTL, calls=%d", f.calls)
	}
}

func TestCheckFailOpen(t *testing.T) {
	f := &fakeRunner{err: errors.New("trivy: db corrupt")}
	g := newGate(t, f)
	d, err := g.Check(context.Background(), "docker.io/library/debian:10", "sha256:err")
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != scanner.ActionAllow {
		t.Errorf("fail-open should Allow on error, got %v", d.Action)
	}
}

func TestCheckFailClosed(t *testing.T) {
	f := &fakeRunner{err: errors.New("trivy: db corrupt")}
	g := newGate(t, f, func(g *Gate) { g.FailClosed = true })
	d, err := g.Check(context.Background(), "docker.io/library/debian:10", "sha256:err")
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != scanner.ActionBlock {
		t.Errorf("fail-closed should Block on error, got %v", d.Action)
	}
	if d.HighestSeverity != scanner.SeverityCritical {
		t.Errorf("fail-closed severity = %v, want critical", d.HighestSeverity)
	}
}

func TestCheckRequiresImageRef(t *testing.T) {
	g := newGate(t, &fakeRunner{})
	if _, err := g.Check(context.Background(), "", "sha256:x"); err == nil {
		t.Fatal("expected error for empty ref")
	}
}

func TestCheckNilGate(t *testing.T) {
	var g *Gate
	d, err := g.Check(context.Background(), "docker.io/library/debian:10", "sha256:x")
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != scanner.ActionAllow {
		t.Errorf("nil gate must allow, got %v", d.Action)
	}
}
