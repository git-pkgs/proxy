// Package mirror provides selective package mirroring for pre-populating the proxy cache.
package mirror

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/storage"
	"golang.org/x/sync/errgroup"
)

// Mirror pre-populates the proxy cache from various input sources.
type Mirror struct {
	proxy   *handler.Proxy
	db      *database.DB
	storage storage.Storage
	logger  *slog.Logger
	workers int
}

// New creates a new Mirror with the given dependencies.
func New(proxy *handler.Proxy, db *database.DB, store storage.Storage, logger *slog.Logger, workers int) *Mirror {
	if workers < 1 {
		workers = 1
	}
	return &Mirror{
		proxy:   proxy,
		db:      db,
		storage: store,
		logger:  logger,
		workers: workers,
	}
}

// Progress tracks the state of a mirror operation.
type Progress struct {
	Total     int64        `json:"total"`
	Completed int64        `json:"completed"`
	Skipped   int64        `json:"skipped"`
	Failed    int64        `json:"failed"`
	Bytes     int64        `json:"bytes"`
	Errors    []MirrorError `json:"errors,omitempty"`
	StartedAt time.Time    `json:"started_at"`
	Phase     string       `json:"phase"`
}

// MirrorError records a single failed mirror attempt.
type MirrorError struct {
	Ecosystem string `json:"ecosystem"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	Error     string `json:"error"`
}

type progressTracker struct {
	total     atomic.Int64
	completed atomic.Int64
	skipped   atomic.Int64
	failed    atomic.Int64
	bytes     atomic.Int64
	mu        sync.Mutex
	errors    []MirrorError
	startedAt time.Time
	phase     atomic.Value // string
}

func newProgressTracker() *progressTracker {
	pt := &progressTracker{
		startedAt: time.Now(),
	}
	pt.phase.Store("resolving")
	return pt
}

func (pt *progressTracker) addError(eco, name, version, err string) {
	pt.mu.Lock()
	pt.errors = append(pt.errors, MirrorError{
		Ecosystem: eco,
		Name:      name,
		Version:   version,
		Error:     err,
	})
	pt.mu.Unlock()
}

func (pt *progressTracker) snapshot() Progress {
	pt.mu.Lock()
	errs := make([]MirrorError, len(pt.errors))
	copy(errs, pt.errors)
	pt.mu.Unlock()

	phase, _ := pt.phase.Load().(string)
	return Progress{
		Total:     pt.total.Load(),
		Completed: pt.completed.Load(),
		Skipped:   pt.skipped.Load(),
		Failed:    pt.failed.Load(),
		Bytes:     pt.bytes.Load(),
		Errors:    errs,
		StartedAt: pt.startedAt,
		Phase:     phase,
	}
}

// Run mirrors all packages from the source using a bounded worker pool.
// It returns the final progress when complete.
func (m *Mirror) Run(ctx context.Context, source Source) (*Progress, error) {
	tracker := newProgressTracker()

	// Collect items from source
	var items []PackageVersion
	tracker.phase.Store("resolving")
	err := source.Enumerate(ctx, func(pv PackageVersion) error {
		items = append(items, pv)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("enumerating packages: %w", err)
	}

	tracker.total.Store(int64(len(items)))
	tracker.phase.Store("downloading")

	// Process items with bounded concurrency
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(m.workers)

	for _, item := range items {
		g.Go(func() error {
			m.mirrorOne(gctx, item, tracker)
			return nil // never fail the group; errors are tracked
		})
	}

	_ = g.Wait()

	tracker.phase.Store("complete")
	p := tracker.snapshot()
	return &p, nil
}

// RunDryRun enumerates what would be mirrored without downloading.
func (m *Mirror) RunDryRun(ctx context.Context, source Source) ([]PackageVersion, error) {
	var items []PackageVersion
	err := source.Enumerate(ctx, func(pv PackageVersion) error {
		items = append(items, pv)
		return nil
	})
	return items, err
}

func (m *Mirror) mirrorOne(ctx context.Context, pv PackageVersion, tracker *progressTracker) {
	result, err := m.proxy.GetOrFetchArtifact(ctx, pv.Ecosystem, pv.Name, pv.Version, "")
	if err != nil {
		tracker.failed.Add(1)
		tracker.addError(pv.Ecosystem, pv.Name, pv.Version, err.Error())
		m.logger.Warn("mirror failed",
			"ecosystem", pv.Ecosystem, "name", pv.Name, "version", pv.Version, "error", err)
		return
	}

	_ = result.Reader.Close()

	if result.Cached {
		tracker.skipped.Add(1)
		m.logger.Debug("already cached",
			"ecosystem", pv.Ecosystem, "name", pv.Name, "version", pv.Version)
	} else {
		tracker.completed.Add(1)
		tracker.bytes.Add(result.Size)
		m.logger.Info("mirrored",
			"ecosystem", pv.Ecosystem, "name", pv.Name, "version", pv.Version,
			"size", result.Size)
	}
}
