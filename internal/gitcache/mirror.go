package gitcache

import (
	"context"
	"errors"
	"fmt"
)

// MirrorResult is what Mirror did.
type MirrorResult struct {
	// Cached reports that the repository, and the commit if one was given,
	// were cached already.
	Cached bool
	// Bytes is how much the cached repository grew.
	Bytes int64
}

// Mirror brings the repository cached under key up to date with repoURL,
// cloning it if needed, and waits for that to finish. With commit, an object
// ID, it also makes sure the commit is cached. One on no branch or tag is
// fetched by ID when fetchWants allows it (see OpenOptions.FetchWants), and
// is kept as long as a commit a client fetched by ID.
func (c *Cache) Mirror(ctx context.Context, key, repoURL, commit string, fetchWants bool) (MirrorResult, error) {
	if commit != "" && !isObjectID(commit) {
		return MirrorResult{}, fmt.Errorf("%q is not a full commit ID", commit)
	}
	lease, err := c.Open(ctx, key, repoURL, OpenOptions{FetchWants: fetchWants})
	if err != nil {
		return MirrorResult{}, err
	}
	defer lease.Release()
	rp := lease.repo
	cached := lease.hit
	before := int64(0)
	var refreshErr error
	// Open cloned a repository that wasn't cached; one that was is
	// refreshed here, waiting for the fetch a client would not wait for.
	if cached {
		before = rp.size.Load()
		refresh := c.startRefresh(rp, repoURL)
		select {
		case <-refresh.done:
			refreshErr = refresh.err
		case <-ctx.Done():
			return MirrorResult{}, ctx.Err()
		}
	}

	if commit != "" {
		fetched, err := c.ensureCommit(ctx, rp, repoURL, commit, fetchWants)
		if err != nil {
			return MirrorResult{}, err
		}
		cached = cached && !fetched
	}
	// A refresh that failed leaves the commit cached all the same.
	if refreshErr != nil && commit == "" {
		return MirrorResult{}, refreshErr
	}
	return MirrorResult{Cached: cached, Bytes: max(rp.size.Load()-before, 0)}, nil
}

// ensureCommit makes sure the repository has commit, fetching it by ID when
// it doesn't and fetchWants allows it, and reports whether it was fetched.
func (c *Cache) ensureCommit(ctx context.Context, rp *repo, repoURL, commit string, fetchWants bool) (bool, error) {
	missing, err := c.missingObjects(ctx, rp.dir, []string{commit})
	if err != nil || len(missing) == 0 {
		return false, err
	}
	if !fetchWants {
		return false, fmt.Errorf("commit %s: %w", commit, ErrNotFound)
	}
	if err := c.fetchMissingWants(ctx, rp, repoURL, missing); err != nil {
		if ctx.Err() != nil || errors.Is(err, ErrTooLarge) || errors.Is(err, ErrCacheFull) || errors.Is(err, errUpstreamUnanswered) {
			return false, err
		}
		// The upstream refuses a commit it doesn't have.
		return false, fmt.Errorf("commit %s: %w: %w", commit, ErrNotFound, err)
	}
	// fetchMissingWants skips a commit that failed moments ago.
	if missing, err = c.missingObjects(ctx, rp.dir, missing); err != nil {
		return false, err
	}
	if len(missing) > 0 {
		return false, fmt.Errorf("commit %s: %w", commit, ErrNotFound)
	}
	return true, nil
}
