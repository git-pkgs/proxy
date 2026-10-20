package gitcache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// wantedPrefix holds commits fetched because a client asked for them by
	// object ID, such as a pull request head that is on no branch, as
	// {wantedPrefix}{unix time}/{object ID}. The refs keep them from being
	// garbage collected, and are hidden from clients.
	wantedPrefix = "refs/proxy/wanted/"
	// wantedMaxAge is how long a commit fetched by ID is kept while no
	// cached branch or tag includes it.
	wantedMaxAge = 7 * 24 * time.Hour
	// wantRetryAfter is how long a want the upstream could not provide is
	// not asked for again. Short, since a commit can reach a CI job before
	// it reaches every upstream replica. A want too large for what
	// max_repo_size leaves waits tooLargeRetryAfter instead.
	wantRetryAfter = time.Minute
	// maxFailedWants bounds the record of failed wants.
	maxFailedWants = 4096
	// maxWantScan bounds how much of a request body is read for its wants.
	maxWantScan = 1 << 20
	// maxWantFetch bounds the missing objects one request may have fetched.
	maxWantFetch = 32
	// maxWantSplit bounds the wants of a failed fetch that are fetched again
	// one at a time; a larger batch is refused as a whole.
	maxWantSplit = 4
)

// readWants reads the object IDs a fetch request wants, from its first
// section, and returns them with a reader that replays the whole body. In
// protocol v0 that section holds the wants; in v2 it is the whole request.
func readWants(body io.Reader) ([]string, io.Reader) {
	var consumed bytes.Buffer
	pkts := newPktReader(io.TeeReader(io.LimitReader(body, maxWantScan), &consumed))
	var wants []string
	for first := true; ; first = false {
		kind, line, err := pkts.next()
		if err != nil || kind == pktFlushKind {
			break
		}
		if kind != pktData {
			continue
		}
		if command, ok := strings.CutPrefix(line, "command="); ok && first && command != "fetch" {
			break
		}
		if rest, ok := strings.CutPrefix(line, "want "); ok {
			if oid, _, _ := strings.Cut(rest, " "); isObjectID(oid) {
				wants = append(wants, oid)
			}
		}
	}
	return wants, io.MultiReader(&consumed, body)
}

// fetchMissingWants fetches the wanted objects the repository lacks. Wants
// for branch and tag tips are already there after the refresh; this covers
// commits a client names directly, as actions/checkout does.
func (c *Cache) fetchMissingWants(ctx context.Context, rp *repo, repoURL string, wants []string) error {
	missing, err := c.missingObjects(ctx, rp.dir, wants)
	if err != nil {
		return err
	}
	missing = c.withoutFailedWants(rp.key, missing)
	if len(missing) == 0 || len(missing) > maxWantFetch {
		return nil
	}
	unlock, err := rp.lockWrites(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	// Another request may have fetched them while this one waited.
	if missing, err = c.missingObjects(ctx, rp.dir, missing); err != nil || len(missing) == 0 {
		return err
	}
	defer c.evict()

	err = c.fetchWants(ctx, rp, repoURL, missing)
	if err == nil || ctx.Err() != nil {
		return err
	}
	if len(missing) == 1 || len(missing) > maxWantSplit ||
		errors.Is(err, ErrTooLarge) || errors.Is(err, ErrCacheFull) || errors.Is(err, errUpstreamUnanswered) {
		// Fetched one at a time, wants too large for what max_repo_size
		// or max_size leaves would exhaust the same budget, and an upstream
		// that did not answer would be waited on once per want.
		c.recordFailedWants(rp.key, missing, err)
		return err
	}
	// Find which wants failed, so a good one asked for with a bad one is
	// not refused too. These fetches hold the repository's write lock, so
	// only a small batch is split.
	var errs []error
	for _, oid := range missing {
		if err := c.fetchWants(ctx, rp, repoURL, []string{oid}); err != nil {
			if ctx.Err() != nil {
				return err
			}
			c.recordFailedWants(rp.key, []string{oid}, err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// fetchWants fetches oids from the upstream under wanted refs, within what
// max_repo_size leaves. A want that does not fit is refused, not the
// repository: transfer.unpackLimit makes a cut-off transfer leave nothing
// behind.
func (c *Cache) fetchWants(ctx context.Context, rp *repo, repoURL string, oids []string) error {
	limit, err := c.fetchLimit(rp)
	if err != nil {
		return err
	}
	now := time.Now()
	refspecs := make([]string, len(oids))
	for i, oid := range oids {
		refspecs[i] = oid + ":" + wantedRef(oid, now)
	}
	res := c.newReservation()
	err = c.fetch(ctx, rp.dir, repoURL, limit, res, []string{"--atomic"}, refspecs...)
	res.settle(rp, dirSize(rp.dir))
	return err
}

func wantedRef(oid string, at time.Time) string {
	return wantedPrefix + strconv.FormatInt(at.Unix(), 10) + "/" + oid
}

// pruneWanted deletes wanted refs that are no longer needed: those older
// than wantedMaxAge, and, after a fetch has moved branches or tags, those
// whose commit a cached branch or tag now includes.
func (c *Cache) pruneWanted(ctx context.Context, dir string, wanted []string, fetched bool) error {
	if len(wanted) == 0 {
		return nil
	}
	cutoff := time.Now().Add(-wantedMaxAge).Unix()
	var drop []string
	keep := make(map[string]string, len(wanted))
	for _, ref := range wanted {
		created, oid, ok := strings.Cut(strings.TrimPrefix(ref, wantedPrefix), "/")
		unix, err := strconv.ParseInt(created, 10, 64)
		if !ok || err != nil || unix < cutoff || !isObjectID(oid) {
			drop = append(drop, ref)
			continue
		}
		keep[oid] = ref
	}
	var mergedErr error
	if fetched && len(keep) > 0 {
		// rev-list prints what the wanted commits, read from stdin, reach
		// and no branch or tag does, so a wanted commit it leaves out is
		// merged.
		cmd := c.command(ctx, nil, "-C", dir, "rev-list", "--stdin", "--not", "--branches", "--tags")
		cmd.Stdin = strings.NewReader(strings.Join(slices.Sorted(maps.Keys(keep)), "\n") + "\n")
		out, err := cmd.Output()
		if err != nil {
			mergedErr = fmt.Errorf("listing unmerged wanted commits: %w", err)
		} else {
			unmerged := make(map[string]bool)
			for line := range strings.Lines(string(out)) {
				unmerged[strings.TrimSpace(line)] = true
			}
			for oid, ref := range keep {
				if !unmerged[oid] {
					drop = append(drop, ref)
				}
			}
		}
	}
	if len(drop) == 0 {
		return mergedErr
	}
	var stdin strings.Builder
	for _, ref := range drop {
		stdin.WriteString("delete " + ref + "\n")
	}
	cmd := c.command(ctx, nil, "-C", dir, "update-ref", "--stdin")
	cmd.Stdin = strings.NewReader(stdin.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("deleting wanted refs: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return mergedErr
}

// withoutFailedWants drops the object IDs a recent fetch for the
// repository could not get.
func (c *Cache) withoutFailedWants(key string, oids []string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	return slices.DeleteFunc(oids, func(oid string) bool {
		until, ok := c.failedWants[key+" "+oid]
		return ok && now.Before(until)
	})
}

// recordFailedWants keeps oids from being fetched again for the repository
// for wantRetryAfter, or for tooLargeRetryAfter when they did not fit in
// max_repo_size.
func (c *Cache) recordFailedWants(key string, oids []string, err error) {
	now := time.Now()
	until := now.Add(wantRetryAfter)
	if errors.Is(err, ErrTooLarge) {
		until = now.Add(tooLargeRetryAfter)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, expiry := range c.failedWants {
		if !now.Before(expiry) {
			delete(c.failedWants, k)
		}
	}
	for _, oid := range oids {
		if len(c.failedWants) >= maxFailedWants {
			return
		}
		c.failedWants[key+" "+oid] = until
	}
}

// missingObjects returns the object IDs in oids the repository does not have.
func (c *Cache) missingObjects(ctx context.Context, dir string, oids []string) ([]string, error) {
	if len(oids) == 0 {
		return nil, nil
	}
	cmd := c.command(ctx, nil, "-C", dir, "cat-file", "--batch-check")
	cmd.Stdin = strings.NewReader(strings.Join(oids, "\n") + "\n")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var missing []string
	for line := range strings.Lines(string(out)) {
		if oid, ok := strings.CutSuffix(strings.TrimSuffix(line, "\n"), " missing"); ok {
			missing = append(missing, oid)
		}
	}
	return missing, nil
}

func isObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 { //nolint:mnd // SHA-1 and SHA-256 hex lengths
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
