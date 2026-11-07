package mirror

import (
	"context"
	"errors"

	"github.com/git-pkgs/proxy/internal/gitcache"
)

// GitMirrorer caches Git repositories for pkg:git items. location is the
// repository's host and path, such as codeberg.org/forgejo/forgejo, and
// commit a full commit ID, or empty to cache the repository's branches and
// tags only.
type GitMirrorer interface {
	Mirror(ctx context.Context, location, commit string) (gitcache.MirrorResult, error)
}

// UnavailableGit fails every pkg:git item with Reason.
type UnavailableGit struct {
	Reason string
}

func (u UnavailableGit) Mirror(context.Context, string, string) (gitcache.MirrorResult, error) {
	return gitcache.MirrorResult{}, errors.New(u.Reason)
}

var errNoGitUpstream = errors.New("git repositories are mirrored from upstream.git, and none is configured")

// mirrorGit caches a pkg:git repository, and the commit its version names,
// in the cache /git serves clones from.
func (m *Mirror) mirrorGit(ctx context.Context, pv PackageVersion, tracker *progressTracker) {
	if m.Git == nil {
		m.recordFailure(tracker, pv, errNoGitUpstream)
		return
	}
	result, err := m.Git.Mirror(ctx, pv.Name, pv.Version)
	if err != nil {
		m.recordFailure(tracker, pv, err)
		return
	}
	if result.Cached {
		tracker.skipped.Add(1)
		m.logger.Debug("already cached", "ecosystem", pv.Ecosystem, "name", pv.Name, "version", pv.Version)
		return
	}
	tracker.completed.Add(1)
	tracker.bytes.Add(result.Bytes)
	m.logger.Info("mirrored", "ecosystem", pv.Ecosystem, "name", pv.Name, "version", pv.Version, "size", result.Bytes)
}
