package gitcache

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
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

	// A refresh past max_repo_size gets the repository dropped once it is
	// released, commit and all.
	if errors.Is(refreshErr, ErrTooLarge) {
		return MirrorResult{}, refreshErr
	}
	if commit != "" {
		if err := c.ensureCommit(ctx, rp, repoURL, commit, fetchWants); err != nil {
			return MirrorResult{}, err
		}
	}
	// A refresh that failed leaves the commit cached all the same.
	if refreshErr != nil && commit == "" {
		return MirrorResult{}, refreshErr
	}
	grown := max(rp.size.Load()-before, 0)
	return MirrorResult{Cached: cached && grown == 0, Bytes: grown}, nil
}

// ensureCommit makes sure the repository has commit, fetching it by ID when
// it doesn't and fetchWants allows it. A commit on no branch or tag that is
// cached already gets a new wanted ref, so it is kept for wantedMaxAge from
// this run on.
func (c *Cache) ensureCommit(ctx context.Context, rp *repo, repoURL, commit string, fetchWants bool) error {
	missing, err := c.missingObjects(ctx, rp.dir, []string{commit})
	if err != nil {
		return err
	}
	if len(missing) == 0 {
		return c.renewWanted(ctx, rp, commit)
	}
	if !fetchWants {
		return fmt.Errorf("commit %s: %w", commit, ErrNotFound)
	}
	if err := c.fetchMissingWants(ctx, rp, repoURL, missing); err != nil {
		if ctx.Err() != nil || errors.Is(err, ErrTooLarge) || errors.Is(err, ErrCacheFull) || errors.Is(err, errUpstreamUnanswered) {
			return err
		}
		// The upstream refuses a commit it doesn't have.
		return fmt.Errorf("commit %s: %w: %w", commit, ErrNotFound, err)
	}
	// fetchMissingWants skips a commit that failed moments ago.
	if missing, err = c.missingObjects(ctx, rp.dir, missing); err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("commit %s: %w", commit, ErrNotFound)
	}
	return nil
}

// renewWanted writes a new wanted ref for commit when no branch or tag
// includes it.
func (c *Cache) renewWanted(ctx context.Context, rp *repo, commit string) error {
	unlock, err := rp.lockWrites(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	unmerged, err := c.unmergedCommits(ctx, rp.dir, []string{commit})
	if err != nil || !unmerged[commit] {
		return err
	}
	cmd := c.command(ctx, nil, "-C", rp.dir, "update-ref", wantedRef(commit, time.Now()), commit)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("renewing wanted ref: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
