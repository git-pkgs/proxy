// Package mirror provides selective package mirroring for pre-populating the proxy cache.
package mirror

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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

	// Images mirrors docker and oci items. Without it they fail like any
	// other unsupported ecosystem.
	Images ImageMirrorer

	// Charts mirrors helm charts. Without it they fail.
	Charts ChartMirrorer

	// Versions lists the versions of unversioned PURLs in mirror jobs and
	// schedules. Nil lists them from the public registries.
	Versions VersionLister

	// JuliaUpstream is the Pkg server Julia packages are fetched from. Empty
	// uses https://pkg.julialang.org.
	JuliaUpstream string

	// RefusePackages, when set, fails every item Images does not take with
	// this reason instead of fetching it, so nothing is cached.
	RefusePackages string
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
	Total     int64         `json:"total"`
	Completed int64         `json:"completed"`
	Skipped   int64         `json:"skipped"`
	Failed    int64         `json:"failed"`
	Bytes     int64         `json:"bytes"`
	Errors    []MirrorError `json:"errors,omitempty"`
	StartedAt time.Time     `json:"started_at"`
	Phase     string        `json:"phase"`
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

const maxTrackedErrors = 1000
const progressReportInterval = 500 * time.Millisecond //nolint:mnd // progress update frequency

func (pt *progressTracker) addError(eco, name, version, err string) {
	pt.mu.Lock()
	if len(pt.errors) < maxTrackedErrors {
		pt.errors = append(pt.errors, MirrorError{
			Ecosystem: eco,
			Name:      name,
			Version:   version,
			Error:     err,
		})
	}
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

// ProgressFunc is called periodically with a snapshot of the current progress.
type ProgressFunc func(Progress)

// Run mirrors all packages from the source using a bounded worker pool.
// It returns the final progress when complete. If onProgress is non-nil,
// it is called with progress snapshots as work proceeds.
func (m *Mirror) Run(ctx context.Context, source Source, onProgress ...ProgressFunc) (*Progress, error) {
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

	// Start periodic progress reporting if a callback was provided
	var progressFn ProgressFunc
	if len(onProgress) > 0 && onProgress[0] != nil {
		progressFn = onProgress[0]
	}
	progressDone := make(chan struct{})
	if progressFn != nil {
		progressFn(tracker.snapshot()) // initial snapshot with total set
		go func() {
			ticker := time.NewTicker(progressReportInterval)
			defer ticker.Stop()
			for {
				select {
				case <-progressDone:
					return
				case <-ticker.C:
					progressFn(tracker.snapshot())
				}
			}
		}()
	}

	// Process items with bounded concurrency
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(m.workers)

	for _, item := range items {
		g.Go(func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					m.logger.Error("panic in mirror worker", "recover", r,
						"ecosystem", item.Ecosystem, "name", item.Name, "version", item.Version)
					tracker.failed.Add(1)
					tracker.addError(item.Ecosystem, item.Name, item.Version, fmt.Sprintf("panic: %v", r))
				}
			}()
			m.mirrorOne(gctx, item, tracker)
			return nil // never fail the group; errors are tracked
		})
	}

	_ = g.Wait()

	close(progressDone) // stop the progress reporter goroutine

	tracker.phase.Store("complete")
	p := tracker.snapshot()

	// Send final snapshot
	if progressFn != nil {
		progressFn(p)
	}

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
	if isImageEcosystem(pv.Ecosystem) && m.Images != nil {
		m.mirrorImage(ctx, pv, tracker)
		return
	}
	if m.RefusePackages != "" {
		tracker.failed.Add(1)
		tracker.addError(pv.Ecosystem, pv.Name, pv.Version, m.RefusePackages)
		m.logger.Warn("mirror refused",
			"ecosystem", pv.Ecosystem, "name", pv.Name, "version", pv.Version, "reason", m.RefusePackages)
		return
	}
	if eco, ok := fileEcosystems[pv.Ecosystem]; ok {
		m.mirrorFiles(ctx, pv, eco, tracker)
		return
	}
	if pv.Ecosystem == "composer" {
		m.mirrorComposer(ctx, pv, tracker)
		return
	}
	if pv.Ecosystem == "helm" {
		m.mirrorHelm(ctx, pv, tracker)
		return
	}
	if pv.Ecosystem == "julia" {
		m.mirrorJulia(ctx, pv, tracker)
		return
	}
	if pv.Ecosystem == "swift" {
		m.mirrorSwift(ctx, pv, tracker)
		return
	}
	result, err := m.proxy.GetOrFetchArtifact(ctx, pv.Ecosystem, pv.Name, pv.Version, "")
	if err != nil {
		m.recordFailure(tracker, pv, err)
		return
	}
	m.recordResult(tracker, pv, result)
}

// recordFailure counts and logs a package that could not be mirrored.
func (m *Mirror) recordFailure(tracker *progressTracker, pv PackageVersion, err error) {
	tracker.failed.Add(1)
	tracker.addError(pv.Ecosystem, pv.Name, pv.Version, err.Error())
	m.logger.Warn("mirror failed",
		"ecosystem", pv.Ecosystem, "name", pv.Name, "version", pv.Version, "error", err)
}

// recordResult closes the fetched artifact and counts it as mirrored, or as
// skipped when it was already cached.
func (m *Mirror) recordResult(tracker *progressTracker, pv PackageVersion, result *handler.CacheResult) {
	if result.Reader != nil {
		_ = result.Reader.Close()
	}

	if result.Cached {
		tracker.skipped.Add(1)
		m.logger.Debug("already cached",
			"ecosystem", pv.Ecosystem, "name", pv.Name, "version", pv.Version)
	} else {
		tracker.completed.Add(1)
		tracker.bytes.Add(result.Artifact.Size)
		m.logger.Info("mirrored",
			"ecosystem", pv.Ecosystem, "name", pv.Name, "version", pv.Version,
			"size", result.Artifact.Size)
	}
}

// mirrorComposer mirrors the archive of a tagged Composer release under the
// file name the Composer handler serves it as, checked against the listed
// shasum, and records the release's publish time, which the handler needs to
// serve the archive from the cache without the metadata.
func (m *Mirror) mirrorComposer(ctx context.Context, pv PackageVersion, tracker *progressTracker) {
	// Packagist names are lowercase, and so are the paths Composer requests.
	name := strings.ToLower(pv.Name)
	v, err := m.versionFinder().FindVersion(ctx, pv.Ecosystem, name, pv.Version)
	if err != nil {
		m.recordFailure(tracker, pv, err)
		return
	}
	distURL, _ := v.Metadata["dist_url"].(string)
	distType, _ := v.Metadata["dist_type"].(string)
	filename, declared, err := handler.ComposerReleaseArchive(v.Number, distURL, distType, strings.TrimPrefix(v.Integrity, "sha1-"))
	if err != nil {
		m.recordFailure(tracker, pv, err)
		return
	}
	result, err := m.proxy.GetOrFetchArtifactFromURLWithIntegrity(ctx, pv.Ecosystem, name, v.Number, filename, distURL, declared)
	if err != nil {
		m.recordFailure(tracker, pv, err)
		return
	}
	if err := m.proxy.RecordComposerPublishTime(name, v.Number, v.PublishedAt); err != nil {
		m.logger.Warn("failed to store composer publish time",
			"name", name, "version", v.Number, "error", err)
	}
	m.recordResult(tracker, pv, result)
}

// mirrorFiles mirrors every file of a version, or those its qualifiers
// select. Each file is cached under the name and version the download
// handler reads from its filename, so clients are served it.
func (m *Mirror) mirrorFiles(ctx context.Context, pv PackageVersion, eco fileEcosystem, tracker *progressTracker) {
	fail := func(err error) { m.recordFailure(tracker, pv, err) }
	if eco.check != nil {
		if err := eco.check(pv); err != nil {
			fail(err)
			return
		}
	}
	v, err := m.versionFinder().FindVersion(ctx, pv.Ecosystem, pv.Name, pv.Version)
	if err != nil {
		fail(fmt.Errorf("listing files: %w", err))
		return
	}
	var matched, fetched int
	var size int64
	for _, file := range v.Artifacts {
		if !eco.selects(pv, file) {
			continue
		}
		name, version, filename, ok := eco.cacheKey(pv, file)
		if !ok {
			continue
		}
		matched++
		result, err := m.proxy.GetOrFetchArtifactFromURLWithDigest(ctx, pv.Ecosystem, name, version, filename, file.URL, fileDigest(file.Integrity))
		if err != nil {
			fail(fmt.Errorf("%s: %w", file.Filename, err))
			return
		}
		if result.Reader != nil {
			_ = result.Reader.Close()
		}
		if !result.Cached {
			fetched++
			size += result.Artifact.Size
		}
	}
	switch selection := eco.selection(pv); {
	case matched == 0 && selection != "":
		fail(fmt.Errorf("no file matches %s", selection))
	case matched == 0:
		fail(errors.New("version has no files"))
	case fetched == 0:
		tracker.skipped.Add(1)
		m.logger.Debug("already cached",
			"ecosystem", pv.Ecosystem, "name", pv.Name, "version", pv.Version)
	default:
		tracker.completed.Add(1)
		tracker.bytes.Add(size)
		m.logger.Info("mirrored",
			"ecosystem", pv.Ecosystem, "name", pv.Name, "version", pv.Version,
			"files", fetched, "size", size)
	}
}

// fileDigest returns the SHA-256 of one file as a digest, which is checked
// against that file alone. The integrity GetOrFetchArtifactFromURLWithIntegrity
// takes is kept with the version instead, and the files of a version that
// share a name and version would overwrite each other's. Other checksums are
// not checked, as the download handlers don't check them either.
func fileDigest(integrity string) string {
	if sum, ok := strings.CutPrefix(integrity, "sha256-"); ok {
		return "sha256:" + sum
	}
	return ""
}

// versionFinder finds versions where Versions lists them, so both come from
// the configured upstream.
func (m *Mirror) versionFinder() VersionFinder { //nolint:ireturn // any finder will do
	if finder, ok := m.Versions.(VersionFinder); ok {
		return finder
	}
	return RegistryVersions{}
}
