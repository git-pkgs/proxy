package mirror

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/purl"
	"github.com/git-pkgs/registries"
)

// Refresh mirrors new releases of the packages clients download most, so
// they are cached before the first client asks for them.
type Refresh struct {
	// Top is how many packages per ecosystem are refreshed.
	Top int
	// Since is how recently a package must have been downloaded.
	Since time.Duration
	// MaxVersions stops a run after mirroring this many versions. 0 means
	// no cap.
	MaxVersions int
	// MaxBytes stops a run once it has downloaded this much. It is checked
	// before each version, so the last one may go over it. 0 means no cap.
	MaxBytes int64
}

func (r Refresh) spent(tracker *progressTracker) bool {
	return (r.MaxVersions > 0 && tracker.completed.Load() >= int64(r.MaxVersions)) ||
		(r.MaxBytes > 0 && tracker.bytes.Load() >= r.MaxBytes)
}

// ReleaseLister lists every release of a package, with its publish time and
// status, for refreshes.
type ReleaseLister interface {
	ListReleases(ctx context.Context, ecosystem, name string) ([]registries.Version, error)
}

// ListReleases implements ReleaseLister.
func (r RegistryVersions) ListReleases(ctx context.Context, ecosystem, name string) ([]registries.Version, error) {
	return r.fetchVersions(ctx, ecosystem, name)
}

// releaseLister lists releases where Versions lists versions, so both come
// from the configured upstream.
func (m *Mirror) releaseLister() ReleaseLister { //nolint:ireturn // any lister will do
	if lister, ok := m.Versions.(ReleaseLister); ok {
		return lister
	}
	return RegistryVersions{}
}

// Refresh mirrors, for each of the most used packages, the newest release
// of each major line it has a version cached on, as clients would be served
// it: releases within cooldown or on the denylist are left out. Packages it
// can't list are counted as failed.
func (m *Mirror) Refresh(ctx context.Context, r Refresh) (*Progress, error) {
	cutoff := time.Now().Add(-r.Since)
	packages, err := m.db.MostUsedPackages(cutoff, r.Top)
	if err != nil {
		return nil, fmt.Errorf("listing the most used packages: %w", err)
	}
	tracker := newProgressTracker()
	tracker.phase.Store("downloading")
	run := func(items []PackageVersion) bool {
		for _, item := range items {
			if ctx.Err() != nil {
				return false
			}
			if r.spent(tracker) {
				m.logger.Info("mirror refresh reached its cap for one run",
					"mirrored", tracker.completed.Load(), "bytes", tracker.bytes.Load())
				return false
			}
			tracker.total.Add(1)
			m.mirrorOne(ctx, item, tracker)
		}
		return true
	}
	for _, group := range byEcosystem(packages) {
		if ix, ok := osIndexes[group[0].Ecosystem]; ok {
			items, errs := m.indexRefreshItems(ctx, ix, group, cutoff)
			for _, err := range errs {
				m.recordFailure(tracker, PackageVersion{Ecosystem: ix.ecosystem}, err)
			}
			if !run(items) {
				break
			}
			continue
		}
		done := true
		for _, pkg := range group {
			items, err := m.refreshItems(ctx, pkg)
			if err != nil {
				m.recordFailure(tracker, PackageVersion{Ecosystem: pkg.Ecosystem, Name: pkg.Name}, err)
				continue
			}
			if done = run(items); !done {
				break
			}
		}
		if !done {
			break
		}
	}
	tracker.phase.Store("complete")
	p := tracker.snapshot()
	return &p, nil
}

// byEcosystem splits packages, ordered by ecosystem, into one group per
// ecosystem.
func byEcosystem(packages []database.UsedPackage) [][]database.UsedPackage {
	var groups [][]database.UsedPackage
	for i, pkg := range packages {
		if i == 0 || pkg.Ecosystem != packages[i-1].Ecosystem {
			groups = append(groups, nil)
		}
		groups[len(groups)-1] = append(groups[len(groups)-1], pkg)
	}
	return groups
}

// refreshItems returns the versions to mirror for a used package.
func (m *Mirror) refreshItems(ctx context.Context, pkg database.UsedPackage) ([]PackageVersion, error) {
	p, err := purl.Parse(pkg.PURL)
	if err != nil {
		return nil, err
	}
	ecosystem, name, _ := mirrorTarget(p)
	if !refreshable(ecosystem) {
		return nil, nil
	}
	releases, err := m.releaseLister().ListReleases(ctx, ecosystem, name)
	if err != nil {
		return nil, fmt.Errorf("listing releases: %w", err)
	}

	var cached []string
	seen := map[string]bool{}
	for _, f := range pkg.Files {
		if !seen[f.Version] {
			seen[f.Version] = true
			cached = append(cached, f.Version)
		}
	}
	allowed := func(v registries.Version) bool {
		return m.proxy == nil || m.proxy.ReleaseAllowed(pkg.Ecosystem, pkg.Name, v.Number, v.PublishedAt)
	}

	var items []PackageVersion
	for _, version := range newestPerCachedLine(ecosystem, cached, releases, allowed) {
		pv := PackageVersion{Ecosystem: ecosystem, Name: name, Version: version}
		if eco, ok := fileEcosystems[ecosystem]; ok {
			pv.variants = map[string]bool{}
			for _, f := range pkg.Files {
				pv.variants[eco.cachedVariant(f.Filename)] = true
			}
		}
		items = append(items, pv)
	}
	return items, nil
}

// refreshable reports whether a refresh lists the releases of an
// ecosystem's packages. Images follow mirror.schedules, OS packages their
// repository indexes, and Julia files are cached by tree hash rather than by
// version.
func refreshable(ecosystem string) bool {
	if isImageEcosystem(ecosystem) || osIndexes[ecosystem].ecosystem != "" || ecosystem == "julia" {
		return false
	}
	return slices.Contains(registries.SupportedEcosystems(), purl.EcosystemToPURLType(ecosystem))
}
