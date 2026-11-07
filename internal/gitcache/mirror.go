package gitcache

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// MirrorResult is what Mirror did.
type MirrorResult struct {
	// Cached reports that the repository, and the commit if a version was
	// given, were cached already.
	Cached bool
	// Bytes is how much the cached repository grew.
	Bytes int64
	// Commit is the commit the version names, empty without a version.
	Commit string
	// Submodules are the submodules Commit records, sorted by path.
	Submodules []Submodule
}

// Submodule is a submodule a commit records: its path, the URL .gitmodules
// gives for it, which may be relative to the repository's, and the commit
// the gitlink pins.
type Submodule struct {
	Path, URL, Commit string
}

// Mirror brings the repository cached under key up to date with repoURL,
// cloning it if needed, and waits for that to finish. version, if given, is
// a full commit ID or a tag. A commit is made sure to be cached: one on no
// branch or tag is fetched by ID when fetchWants allows it (see
// OpenOptions.FetchWants), and is kept as long as a commit a client fetched
// by ID. A tag is cached with the repository's tags.
func (c *Cache) Mirror(ctx context.Context, key, repoURL, version string, fetchWants bool) (MirrorResult, error) {
	tag := version != "" && !isObjectID(version)
	if tag {
		if err := c.checkTagName(ctx, version); err != nil {
			return MirrorResult{}, err
		}
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
	commit := version
	switch {
	case tag:
		if commit, err = c.resolveTag(ctx, rp.dir, version); err != nil {
			if refreshErr != nil {
				// The tag may be one the failed refresh would have brought.
				return MirrorResult{}, refreshErr
			}
			return MirrorResult{}, err
		}
	case version != "":
		if err := c.ensureCommit(ctx, rp, repoURL, commit, fetchWants); err != nil {
			return MirrorResult{}, err
		}
	}
	// A refresh that failed leaves the commit cached all the same.
	if refreshErr != nil && version == "" {
		return MirrorResult{}, refreshErr
	}
	var submodules []Submodule
	if commit != "" {
		if submodules, err = c.submodules(ctx, rp.dir, commit); err != nil {
			return MirrorResult{}, err
		}
	}
	grown := max(rp.size.Load()-before, 0)
	return MirrorResult{Cached: cached && grown == 0, Bytes: grown, Commit: commit, Submodules: submodules}, nil
}

// checkTagName refuses a version that is no valid tag name, such as one
// with .. or a revision suffix like ^{tree}.
func (c *Cache) checkTagName(ctx context.Context, name string) error {
	if err := c.command(ctx, nil, "check-ref-format", "refs/tags/"+name).Run(); err != nil {
		return fmt.Errorf("%q is neither a full commit ID nor a tag name", name)
	}
	return nil
}

// resolveTag returns the commit tag points at.
func (c *Cache) resolveTag(ctx context.Context, dir, tag string) (string, error) {
	out, err := c.command(ctx, nil, "-C", dir, "rev-parse", "--verify", "--quiet", "--end-of-options", "refs/tags/"+tag+"^{commit}").Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return "", fmt.Errorf("tag %s: %w", tag, ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("resolving tag %s: %w", tag, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// submodules lists the submodules commit records: its gitlinks that
// .gitmodules names a URL for.
func (c *Cache) submodules(ctx context.Context, dir, commit string) ([]Submodule, error) {
	if c.command(ctx, nil, "-C", dir, "cat-file", "-e", commit+":.gitmodules").Run() != nil {
		return nil, nil
	}
	// --list rather than --get-regexp, which fails when nothing matches.
	out, err := c.run(ctx, dir, "config", "-z", "--blob", commit+":.gitmodules", "--list")
	if err != nil {
		return nil, fmt.Errorf("reading .gitmodules: %w", err)
	}
	paths, urls := map[string]string{}, map[string]string{}
	for entry := range strings.SplitSeq(out, "\x00") {
		key, value, ok := strings.Cut(entry, "\n")
		section, ok2 := strings.CutPrefix(key, "submodule.")
		name, field, dotted := cutLast(section, ".")
		if !ok || !ok2 || !dotted {
			continue
		}
		switch field {
		case "path":
			paths[value] = name
		case "url":
			urls[name] = value
		}
	}
	tree, err := c.run(ctx, dir, "ls-tree", "-r", "-z", "--full-tree", commit)
	if err != nil {
		return nil, err
	}
	var submodules []Submodule
	for entry := range strings.SplitSeq(tree, "\x00") {
		meta, path, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 || fields[0] != "160000" {
			continue
		}
		if url := urls[paths[path]]; url != "" && isObjectID(fields[2]) {
			submodules = append(submodules, Submodule{Path: path, URL: url, Commit: fields[2]})
		}
	}
	slices.SortFunc(submodules, func(a, b Submodule) int { return strings.Compare(a.Path, b.Path) })
	return submodules, nil
}

func cutLast(s, sep string) (before, after string, found bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
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
