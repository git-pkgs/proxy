package gitcache

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"
)

// RepositoryStats describes a cached repository.
type RepositoryStats struct {
	// Key is the repository's path in the cache, such as
	// github-1a2b3c4d/acme/widgets.git.
	Key  string
	Size int64
	// LastChecked is when the upstream's refs were last compared, and
	// LastUsed when a client last read the repository.
	LastChecked, LastUsed time.Time
	// Requests counts clones and fetches since the cache was opened.
	Requests int64
	// LastError names the kind of error the last refresh failed with, such
	// as fetch_failed or too_large, and LastErrorAt when. Both are empty
	// once a refresh succeeds.
	LastError   string
	LastErrorAt time.Time
}

// refreshError is the last failed refresh of a repository.
type refreshError struct {
	kind string
	at   time.Time
}

// Repositories describes the repositories in the cache, sorted by key.
func (c *Cache) Repositories() []RepositoryStats {
	c.mu.Lock()
	repos := make([]*repo, 0, len(c.repos))
	for _, rp := range c.repos {
		repos = append(repos, rp)
	}
	c.mu.Unlock()

	var out []RepositoryStats
	for _, rp := range repos {
		if !rp.exists() {
			continue
		}
		s := RepositoryStats{
			Key:         rp.key,
			Size:        rp.size.Load(),
			LastChecked: unixNanoTime(rp.lastChecked.Load()),
			LastUsed:    unixNanoTime(rp.lastUsed.Load()),
			Requests:    rp.requests.Load(),
		}
		if e := rp.lastError.Load(); e != nil {
			s.LastError, s.LastErrorAt = e.kind, e.at
		}
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b RepositoryStats) int { return strings.Compare(a.Key, b.Key) })
	return out
}

func unixNanoTime(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// RepositoryState is what a cached repository holds: the branch HEAD names
// with its newest commit, and how many branches and tags there are.
type RepositoryState struct {
	Branch     string
	Commit     string
	CommitTime time.Time
	// Subject is the first line of the commit message, at most
	// maxSubjectLength bytes.
	Subject        string
	Branches, Tags int
}

// maxSubjectLength bounds RepositoryState.Subject.
const maxSubjectLength = 200

// headFields are the commit ID, time and subject readState asks git log for.
const headFields = 3

// cachedState is a repository's state as read after the check at checked.
type cachedState struct {
	checked int64
	state   RepositoryState
}

// RepositoryState reads the state of the repository cached under key. It is
// read again only after the repository's next check of the upstream, so a
// page listing many repositories runs git for those that changed.
func (c *Cache) RepositoryState(ctx context.Context, key string) (RepositoryState, error) {
	c.mu.Lock()
	rp := c.repos[key]
	c.mu.Unlock()
	if rp == nil || !rp.exists() {
		return RepositoryState{}, ErrNotFound
	}
	checked := rp.lastChecked.Load()
	if cached := rp.state.Load(); cached != nil && cached.checked == checked {
		return cached.state, nil
	}
	state, err := c.readState(ctx, rp.dir)
	if err != nil {
		return RepositoryState{}, err
	}
	rp.state.Store(&cachedState{checked: checked, state: state})
	return state, nil
}

func (c *Cache) readState(ctx context.Context, dir string) (RepositoryState, error) {
	var state RepositoryState
	refs, err := c.run(ctx, dir, "for-each-ref", "--format=%(refname)", "refs/heads", "refs/tags")
	if err != nil {
		return state, err
	}
	for ref := range strings.Lines(refs) {
		if strings.HasPrefix(ref, "refs/heads/") {
			state.Branches++
		} else {
			state.Tags++
		}
	}
	// A repository with no branch has no HEAD commit to show.
	if branch, err := c.run(ctx, dir, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
		state.Branch = strings.TrimSpace(branch)
	}
	if head, err := c.run(ctx, dir, "log", "-1", "--format=%H%x00%ct%x00%s", "HEAD", "--"); err == nil {
		fields := strings.SplitN(strings.TrimSpace(head), "\x00", headFields)
		if len(fields) == headFields {
			state.Commit, state.Subject = fields[0], fields[2]
			if unix, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				state.CommitTime = time.Unix(unix, 0)
			}
			if len(state.Subject) > maxSubjectLength {
				state.Subject = strings.ToValidUTF8(state.Subject[:maxSubjectLength], "") + "…"
			}
		}
	}
	return state, nil
}
